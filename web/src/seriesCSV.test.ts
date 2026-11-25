import assert from 'node:assert/strict'
import { test } from 'node:test'
import { compareRows, csvFileName, extraSeriesRows, roundTripRows, type RoundTripData } from './seriesCSV.ts'
import { icmpErrorName, sendReasonName } from './api.ts'
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
  received: [18, 0, 0, 19],
  loss: [0.1, 1, NaN, 0.05],
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
  assert.deepEqual(rows[0].slice(0, 5), ['interval_start_utc', 'median_us', 'sent', 'received', 'loss_percent'])
  assert.deepEqual(rows[1].slice(0, 5), ['2023-11-14T22:13:20Z', 1500, 20, 18, 10])
})

test('an ICMP error and a send error are named in their own columns', () => {
  const rows = roundTripRows({ ...data, icmpErrors: [null, 0x0301, null, null], sendErrors: [null, null, 1, null] }, FLAGS, 60)
  const icmp = rows[0].indexOf('icmp_error')
  const send = rows[0].indexOf('send_error')
  assert.ok(Number.isNaN(rows[1][icmp]) && Number.isNaN(rows[1][send]))
  assert.equal(rows[2][icmp], icmpErrorName(0x0301))
  assert.ok(Number.isNaN(rows[2][send]))
  assert.equal(rows[3][send], sendReasonName(1))
  assert.ok(Number.isNaN(rows[3][icmp]))
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
  assert.deepEqual(rows[0].slice(0, 4), ['column_start_utc', 'column_end_utc', 'intervals', 'covered_percent_estimate'])
  // A pooled read has no per-interval details: no column that would read as "none".
  assert.ok(!rows[0].includes('icmp_error') && !rows[0].includes('send_error'))
  assert.equal(rows[0].length, rows[1].length)
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

test('a pooled extra series names its columns', () => {
  const rows = extraSeriesRows({ ts: [T0], ends: [T0 + 60], median: [5], measured: [1] })
  assert.deepEqual(rows[0], ['column_start_utc', 'column_end_utc', 'median_us', 'status'])
  assert.deepEqual(rows[1], ['2023-11-14T22:13:20Z', '2023-11-14T22:14:20Z', 5, 'measured'])
})

test('compare gives each series a median and a status, and tells no row, no replies and nothing sent apart', () => {
  const rows = compareRows([
    { label: 'a', ts: [T0, T0 + 60, T0 + 120, T0 + 300], med: [1000, null, null, 1200], sent: [20, 20, 0, 20] },
    { label: 'b', ts: [T0 + 60, T0 + 120], med: [2000, 2100], sent: [20, 20] },
  ])
  assert.deepEqual(csv(rows), [
    '"start_utc","a median_us","a status","b median_us","b status"',
    '"2023-11-14T22:13:20Z",1000,"measured",,"no row"',
    '"2023-11-14T22:14:20Z",,"no replies",2000,"measured"',
    '"2023-11-14T22:15:20Z",,"nothing sent",2100,"measured"',
    '"2023-11-14T22:18:20Z",1200,"measured",,"no row"',
  ])
})

test('compare orders rows by time when the series are not in order across each other', () => {
  const rows = compareRows([
    { label: 'a', ts: [T0 + 120], med: [1], sent: [1] },
    { label: 'b', ts: [T0, T0 + 60], med: [2, 3], sent: [1, 1] },
  ])
  assert.deepEqual(rows.slice(1).map((r) => r[0]), ['2023-11-14T22:13:20Z', '2023-11-14T22:14:20Z', '2023-11-14T22:15:20Z'])
})

test('the file name carries the target and the window', () => {
  assert.equal(csvFileName(['/Lab/my target', undefined, 'IPv6'], T0, T0 + 3600), 'Lab_my_target-IPv6-20231114T221320Z-20231114T231320Z.csv')
})
