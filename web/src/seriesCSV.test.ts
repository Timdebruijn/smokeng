import assert from 'node:assert/strict'
import { test } from 'node:test'
import { compareRows, csvFileName, extraSeriesRows, roundTripRows, type RoundTripData } from './seriesCSV.ts'
import { csvCell } from './util.ts'

const FLAGS = [
  { bit: 1, label: 'userspace TX' },
  { bit: 8, label: 'dropped replies' },
]
const T0 = 1_700_000_000 // 2023-11-14T22:13:20Z

// Three intervals a minute apart, then one after a missed stretch.
const data: RoundTripData = {
  ts: [T0, T0 + 60, T0 + 120, T0 + 300],
  sent: [20, 20, 0, 20],
  received: [20, 0, 0, 19],
  loss: [0, 1, NaN, 0.05],
  median: [1500, NaN, NaN, 2000],
  flags: [0, 0, 0, 1 | 8],
  icmpErrors: [null, null, null, null],
  sendErrors: [null, null, null, null],
  lossPositions: [null, null, null, null],
}
const csv = (rows: (string | number)[][]) => rows.map((r) => r.map(csvCell).join(','))

test('a measured row carries its figures and a UTC timestamp', () => {
  const rows = roundTripRows(data, FLAGS, 60)
  assert.equal(rows[0][0], 'interval_start_utc')
  assert.deepEqual(rows[1].slice(0, 5), ['2023-11-14T22:13:20Z', 1500, 20, 20, 0])
})

test('a value that was not measured is an empty cell, never 0', () => {
  const lines = csv(roundTripRows(data, FLAGS, 60))
  // Sent and lost: no median, but the loss is known.
  assert.equal(lines[2].split(',')[1], '')
  assert.equal(lines[2].split(',')[4], '100')
  // Nothing sent: neither median nor loss.
  assert.equal(lines[3].split(',')[1], '')
  assert.equal(lines[3].split(',')[4], '')
})

test('a gap is not filled in, and the flags travel with the row', () => {
  const rows = roundTripRows(data, FLAGS, 60)
  assert.equal(rows.length, 1 + 4)
  assert.equal(rows[4][0], '2023-11-14T22:18:20Z')
  assert.equal(rows[4][rows[4].length - 1], 'userspace TX; dropped replies')
  assert.equal(rows[1][rows[1].length - 1], '')
})

test('a pooled column says what it spans and that its flags are any-interval', () => {
  const rows = roundTripRows(
    { ...data, pooled: { ends: [T0 + 60, T0 + 120, T0 + 180, T0 + 420], rounds: [1, 1, 0, 1], positional: [0, 0, 0, 1] } },
    FLAGS,
    60,
  )
  assert.deepEqual(rows[0].slice(0, 4), ['column_start_utc', 'column_end_utc', 'intervals', 'covered_percent'])
  assert.equal(rows[1][3], 100)
  assert.equal(rows[4][3], 50)
  assert.match(String(rows[4][rows[4].length - 1]), /positional loss$/)
  assert.ok(Number.isNaN(roundTripRows({ ...data, pooled: { ends: data.ts, rounds: [1, 1, 1, 1], positional: [0, 0, 0, 0] } }, FLAGS, 0)[1][3]))
})

test('an extra series tells "nothing to compare" from "not measured"', () => {
  const rows = extraSeriesRows({ ts: [T0, T0 + 60, T0 + 120], median: [-3, NaN, NaN], measured: [1, 1, 0] })
  assert.deepEqual(rows.slice(1).map((r) => r[2]), ['measured', 'nothing to compare', 'not measured'])
  assert.equal(csv(rows)[2].split(',')[1], '')
})

test('compare puts one series per column and leaves other series cells empty', () => {
  const rows = compareRows([
    { label: 'a', ts: [T0, T0 + 60, T0 + 300], med: [1000, null, 1200] },
    { label: 'b', ts: [T0 + 60, T0 + 120], med: [2000, 2100] },
  ])
  assert.deepEqual(csv(rows), [
    '"start_utc","a median_us","b median_us"',
    '"2023-11-14T22:13:20Z",1000,',
    '"2023-11-14T22:14:20Z",,2000',
    '"2023-11-14T22:15:20Z",,2100',
    '"2023-11-14T22:18:20Z",1200,',
  ])
})

test('the file name carries the target and the window', () => {
  assert.equal(csvFileName(['/Lab/my target', undefined, 'IPv6'], T0, T0 + 3600), 'Lab_my_target-IPv6-20231114T221320Z-20231114T231320Z.csv')
})
