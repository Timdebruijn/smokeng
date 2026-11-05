import assert from 'node:assert/strict'
import { test } from 'node:test'
import { axisLabel } from './layout.ts'
import { csvCell, fromLocalInput, minMax, refreshMs, toLocalInput, zoomOut } from './util.ts'

// A golden reference can hold hundreds of thousands of samples. Math.min(...xs)
// passes each element as an argument, and the engine refuses past about 125,000:
// a RangeError thrown while rendering, which blanked the whole page.
test('the spread form really does fail at the sizes a reference can reach', () => {
  const big = new Float64Array(1_000_000).fill(1)
  assert.throws(() => Math.min(...big), RangeError)
})

test('minMax handles any length', () => {
  const big = new Float64Array(1_000_000)
  for (let i = 0; i < big.length; i++) big[i] = (i * 7919) % 100_003
  big[500_000] = -4
  big[999_999] = 1_000_000
  assert.deepEqual(minMax(big), { lo: -4, hi: 1_000_000 })
  assert.deepEqual(minMax([3, 1, 2]), { lo: 1, hi: 3 })
  assert.deepEqual(minMax([5]), { lo: 5, hi: 5 })
  assert.deepEqual(minMax(new Uint32Array([9, 4, 11])), { lo: 4, hi: 11 })
})

test('minMax says so when there is nothing to take a minimum of', () => {
  assert.equal(minMax([]), null)
  // Not a number is not a value: it is skipped, where Math.min would turn the
  // whole answer into NaN.
  assert.deepEqual(minMax([3, NaN, 1]), { lo: 1, hi: 3 })
  assert.equal(minMax([NaN, NaN]), null)
  assert.deepEqual(minMax([-Infinity, 2, Infinity]), { lo: -Infinity, hi: Infinity })
})

// A target name is whatever an editor typed, and a spreadsheet reads a cell that
// starts with = + - or @ as a formula, which can run commands or send data out.
test('csvCell quotes text and neutralises what a spreadsheet would run', () => {
  assert.equal(csvCell('plain'), '"plain"')
  assert.equal(csvCell('say "hi"'), '"say ""hi"""')
  assert.equal(csvCell('a,b\nc'), '"a,b\nc"')
  for (const risky of ['=1+1', '+1', '-1', '@SUM(A1)', '\t=1', '\r=1', '=HYPERLINK("http://x","y")']) {
    const cell = csvCell(risky)
    assert.ok(cell.startsWith(`"'`), `${JSON.stringify(risky)} became ${cell}, which a spreadsheet would still read as a formula`)
    assert.ok(cell.includes(risky.replace(/"/g, '""')), 'the text itself is kept')
  }
  // Not at the start, nothing to neutralise.
  assert.equal(csvCell('a=1'), '"a=1"')
  assert.equal(csvCell(''), '""')
})

test('csvCell leaves numbers as numbers', () => {
  assert.equal(csvCell(0.9995), '0.9995')
  assert.equal(csvCell(-3.5), '-3.5')
  assert.equal(csvCell(0), '0')
  // No number to give is an empty cell, not the text "NaN" in a numeric column.
  assert.equal(csvCell(NaN), '')
  assert.equal(csvCell(Infinity), '')
})

test('a long window refreshes about once per pixel column, a short one every ten seconds', () => {
  assert.equal(refreshMs(3600), 10_000)
  assert.equal(refreshMs(86400), 86_400)
  assert.equal(refreshMs(30 * 86400), 2_592_000)
})

test('zooming out doubles the window around its centre, and stays live at the present', () => {
  const now = 1_000_000
  assert.deepEqual(zoomOut(500_000, 600_000, now), { from: 450_000, to: 650_000 })
  // A window whose doubled end would pass now becomes a trailing window.
  assert.deepEqual(zoomOut(now - 3600, now, now), { rangeS: 7200 })
  // One whose doubled end stops short of now stays a fixed window.
  assert.deepEqual(zoomOut(now - 10_000, now - 4_000, now), { from: now - 13_000, to: now - 1_000 })
})

test('a datetime-local value round-trips to the minute, and nonsense is refused', () => {
  const t = 1_793_911_200 // a whole minute
  assert.equal(fromLocalInput(toLocalInput(t)), t)
  assert.equal(fromLocalInput(''), null)
  assert.equal(fromLocalInput('yesterday'), null)
  assert.equal(fromLocalInput('2026-13-45T99:99'), null)
})

test('the time axis says the date once a window is longer than a day', () => {
  const t = new Date(2026, 10, 5, 14, 30).getTime() / 1000
  assert.equal(axisLabel(t, 3600), '14:30')
  assert.equal(axisLabel(t, 86400), '14:30')
  assert.equal(axisLabel(t, 2 * 86400), '5 Nov 14:30')
  assert.equal(axisLabel(t, 30 * 86400), '5 Nov')
})
