import assert from 'node:assert/strict'
import { test } from 'node:test'
import { availabilityURL, fetchSeries, pathsURL, seriesURL, type Target } from './api.ts'
import { axisLabel } from './layout.ts'
import {
  afterName,
  alertKey,
  alertName,
  countedFlags,
  csvCell,
  filterByFamily,
  parseFamilyChoice,
  resolveSeries,
  screenTitle,
  seriesKey,
  seriesLabel,
  shownFamilies,
  showsFamily,
  summarise,
  fromLocalInput,
  minMax,
  refreshMs,
  toLocalInput,
  zoomOut,
} from './util.ts'

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

test('two families of one alert have different keys', () => {
  const a = { rule_id: 1, target_id: 2, agent_id: 3, family: 'v4' }
  assert.notEqual(alertKey(a), alertKey({ ...a, family: 'v6' }))
  assert.equal(alertKey(a), alertKey({ ...a }))
})

test('seriesLabel names agent and family, whichever are known', () => {
  assert.equal(seriesLabel('local', 'v4'), 'local · v4')
  assert.equal(seriesLabel(undefined, 'v6'), 'v6')
  assert.equal(seriesLabel('local', ''), 'local')
})

test('a row never ends in a separator when it has no vantage or family to name', () => {
  assert.equal(afterName(seriesLabel('local', 'v4')), ' · local · v4')
  assert.equal(afterName(seriesLabel(undefined, undefined)), '')
  assert.equal(afterName(undefined), '')
})

test('an alert is named with its series, and without a trailing comma when it has none', () => {
  const a = { rule: 'loss', target: '/Production/web', agent: 'local', family: 'v6' }
  assert.equal(alertName(a), 'loss on /Production/web, local · v6')
  assert.equal(alertName({ rule: 'loss', target: '/Production/web' }), 'loss on /Production/web')
})

// Protects the summary figures from an interval that sent nothing (no data,
// with a reason): it is left out of loss, so one such interval beside a lossy
// one does not dilute the loss and does not count as a clean one, and the flag
// it carries still reaches the series' flags.
test('summarise leaves intervals that sent nothing out of the loss', () => {
  const s = summarise({
    values: new Uint32Array([1000, 2000]),
    sent: new Float64Array([10, 0, 0]),
    received: new Float64Array([5, 0, 0]),
    flags: new Uint8Array([0, 1 << 6, 1 << 6]),
  })
  assert.equal(s.loss, 50)
  assert.equal(s.sent, 10)
  assert.equal(s.flags, 1 << 6)
})

// Protects the window that is only no data: nothing was sent, which callers read
// off `sent` (loss alone would say 0%).
test('summarise of a window with nothing sent says so by sent, with no latency', () => {
  const s = summarise({
    values: new Uint32Array(),
    sent: new Float64Array([0, 0]),
    received: new Float64Array([0, 0]),
    flags: new Uint8Array([1 << 6, 1 << 6]),
  })
  assert.equal(s.sent, 0)
  assert.equal(s.median, null)
})

// A target only as far as the series code reads it.
const target = (address_family: string | null, agents = 'local'): Target =>
  ({ id: 7, address_family, settings: { agents: { effective: agents } } }) as unknown as Target
const ids = new Map([
  ['local', 0],
  ['edge', 3],
])

test('a single-family target is one series per agent with no family', () => {
  for (const af of ['v4', 'v6', null]) {
    const s = resolveSeries(target(af), ids)
    assert.equal(s.length, 1)
    assert.equal(s[0].family, undefined)
  }
})

test('a both target is v4 then v6 for each agent', () => {
  const s = resolveSeries(target('both', 'local edge'), ids)
  assert.deepEqual(
    s.map((x) => [x.agentId, x.family]),
    [
      [0, 'v4'],
      [0, 'v6'],
      [3, 'v4'],
      [3, 'v6'],
    ],
  )
})

test('a both target nobody measures is one unresolved series, not two', () => {
  const s = resolveSeries(target('both', 'ghost'), ids)
  assert.equal(s.length, 1)
  assert.equal(s[0].agentId, null)
  assert.equal(s[0].family, undefined)
})

test('the family filter against v4-only, v6-only and both targets', () => {
  const v4 = target('v4')
  const v6 = target('v6')
  const both = target('both')
  const shows = (c: 'v4' | 'v6' | 'both', t: Target) =>
    filterByFamily(resolveSeries(t, ids), c).map((s) => s.family ?? s.target.address_family)
  assert.deepEqual(shows('both', v4), ['v4'])
  assert.deepEqual(shows('both', v6), ['v6'])
  assert.deepEqual(shows('both', both), ['v4', 'v6'])
  assert.deepEqual(shows('v4', v4), ['v4'])
  assert.deepEqual(shows('v4', v6), [])
  assert.deepEqual(shows('v4', both), ['v4'])
  assert.deepEqual(shows('v6', v4), [])
  assert.deepEqual(shows('v6', v6), ['v6'])
  assert.deepEqual(shows('v6', both), ['v6'])
  // An unresolved both target belongs to both families, so neither choice hides it.
  const ghost = resolveSeries(target('both', 'ghost'), ids)
  assert.equal(filterByFamily(ghost, 'v6').length, 1)
  assert.equal(filterByFamily(ghost, 'v4').length, 1)
  // A group-like target with no family is read as v4, the family the API defaults to.
  assert.equal(showsFamily('v6', target(null)), false)
  assert.equal(showsFamily('v4', target(null)), true)
})

test('shownFamilies is what the detail page draws', () => {
  assert.deepEqual(shownFamilies(target('both'), 'both'), ['v4', 'v6'])
  assert.deepEqual(shownFamilies(target('both'), 'v6'), ['v6'])
  assert.deepEqual(shownFamilies(target('v4'), 'both'), [undefined])
  assert.deepEqual(shownFamilies(target('v4'), 'v6'), [])
})

test('a stored family choice is read back, and anything else is both', () => {
  assert.equal(parseFamilyChoice('v4'), 'v4')
  assert.equal(parseFamilyChoice('v6'), 'v6')
  assert.equal(parseFamilyChoice('both'), 'both')
  assert.equal(parseFamilyChoice(null), 'both')
  assert.equal(parseFamilyChoice('v5'), 'both')
})

test('a series key carries the family, and is unchanged without one', () => {
  assert.equal(seriesKey(7, 3), '7-3')
  assert.equal(seriesKey(7, null), '7-unresolved')
  assert.equal(seriesKey(7, 3, 'v4'), '7-3-v4')
  assert.notEqual(seriesKey(7, 3, 'v4'), seriesKey(7, 3, 'v6'))
})

test('the requests carry family only when it is set', () => {
  assert.equal(seriesURL(7, 3, 10, 20), '/api/v1/measurements?target_id=7&agent_id=3&from=10&to=20')
  assert.equal(seriesURL(7, 3, 10, 20, 500), '/api/v1/measurements?target_id=7&agent_id=3&from=10&to=20&columns=500')
  assert.equal(seriesURL(7, 3, 10, 20, 500, 'v6'), '/api/v1/measurements?target_id=7&agent_id=3&from=10&to=20&columns=500&family=v6')
  assert.equal(pathsURL(7, 3, 10, 20), '/api/v1/paths?target_id=7&agent_id=3&from=10&to=20')
  assert.equal(pathsURL(7, 3, 10, 20, 'v6'), '/api/v1/paths?target_id=7&agent_id=3&from=10&to=20&family=v6')
  assert.equal(availabilityURL(7, 10, 20), '/api/v1/availability?target_id=7&from=10&to=20')
  assert.equal(availabilityURL(7, 10, 20, 100), '/api/v1/availability?target_id=7&from=10&to=20&down_threshold=100')
  assert.equal(availabilityURL(7, 10, 20, 100, 'v6'), '/api/v1/availability?target_id=7&from=10&to=20&down_threshold=100&family=v6')
})

test('the two families of one window are two in-flight requests', async () => {
  const urls: string[] = []
  const real = globalThis.fetch
  globalThis.fetch = (async (u: string) => {
    urls.push(u)
    return new Response('x', { status: 500 })
  }) as typeof fetch
  try {
    await Promise.allSettled([fetchSeries(7, 3, 10, 20, 5, 'v4'), fetchSeries(7, 3, 10, 20, 5, 'v6')])
  } finally {
    globalThis.fetch = real
  }
  assert.equal(urls.length, 2)
})

test('countedFlags leaves out the userspace pair only where the probe type always carries it', () => {
  // irtt, tcp and http(s) are timed in userspace on every interval.
  for (const type of ['irtt', 'tcp', 'http', 'https']) {
    assert.equal(countedFlags(0b11, type), 0, type)
    assert.equal(countedFlags(0b01, type), 0, type)
    // Anything else on the same interval still counts: dropped replies, a clock step.
    assert.equal(countedFlags(0b11 | (1 << 3), type), 1 << 3, type)
    assert.equal(countedFlags(1 << 5, type), 1 << 5, type)
  }
  // icmp and dns are stamped by the kernel, so there the pair means that failed.
  for (const type of ['icmp', 'dns', '', undefined, null]) {
    assert.equal(countedFlags(0b11, type), 0b11, String(type))
    assert.equal(countedFlags(0b10, type), 0b10, String(type))
  }
})

test('screenTitle names every screen, detail after its target', () => {
  assert.equal(screenTitle('overview'), 'Overview - smokeng')
  assert.equal(screenTitle('access'), 'Access - smokeng')
  assert.equal(screenTitle('signin'), 'Sign in - smokeng')
  assert.equal(screenTitle('detail', 'core/router'), 'core/router - smokeng')
  assert.equal(screenTitle('detail', null), 'Target - smokeng')
})
