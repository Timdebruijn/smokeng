import assert from 'node:assert/strict'
import { test } from 'node:test'
import { compareColumns, compareReadout, gapsBefore, lineStyle, stepRow } from './layout.ts'

test('the keyboard steps one row, stops at the ends and starts from the right side', () => {
  assert.equal(stepRow('ArrowRight', 2, 5), 3)
  assert.equal(stepRow('ArrowRight', 4, 5), 4)
  assert.equal(stepRow('ArrowLeft', 0, 5), 0)
  assert.equal(stepRow('ArrowLeft', 3, 5), 2)
  assert.equal(stepRow('ArrowRight', -1, 5), 0)
  assert.equal(stepRow('ArrowLeft', -1, 5), 4)
  assert.equal(stepRow('Home', 3, 5), 0)
  assert.equal(stepRow('End', 1, 5), 4)
  assert.equal(stepRow('ArrowRight', -1, 0), null)
  assert.equal(stepRow('a', 1, 5), null)
})

const at = (gaps: boolean[]) => gaps.flatMap((g, j) => (g ? [j] : []))

test('a raw read breaks across missed intervals, not across jitter', () => {
  // 60 s intervals with a few seconds of jitter, then six hours with no rows.
  const ts = [0, 61, 119, 181, 240, 240 + 6 * 3600, 240 + 6 * 3600 + 59]
  assert.deepEqual(at(gapsBefore(ts)), [5])
  // One missed interval is a gap too.
  assert.deepEqual(at(gapsBefore([0, 60, 180, 240, 300])), [2])
})

test('a pooled read breaks where the server omitted columns', () => {
  // Contiguous 36-minute columns; the two after 4320 hold no interval.
  assert.deepEqual(at(gapsBefore([0, 2160, 4320, 10800, 12960])), [3])
})

test('a pooled read with fewer intervals than columns joins neighbours', () => {
  // Pooled for its sample count: one 300 s interval per 100 s column, so two
  // empty columns lie between neighbours. Only the missed interval breaks.
  assert.deepEqual(at(gapsBefore([0, 300, 600, 1200, 1500])), [3])
})

test('the first Compare line is solid and no two of the eight share a colour or a dash', () => {
  assert.deepEqual(lineStyle(0).dash, [])
  const seen = (f: (i: number) => string) => new Set(Array.from({ length: 8 }, (_, i) => f(i))).size
  assert.equal(seen((i) => lineStyle(i).colour), 8)
  assert.equal(seen((i) => lineStyle(i).dash.join()), 8)
  assert.deepEqual(lineStyle(8), lineStyle(0))
})

test('the Compare cursor visits a column one series lacks and reads it as no data, not zero', () => {
  const a = { label: 'A', ts: [0, 60, 120, 180, 240, 300], med: [10000, null, 14000, 9000, 8000, 7000] }
  const b = { label: 'B', ts: [0, 60, 120, 180, 300], med: [5000, 6000, 7000, 8000, 9000] }
  assert.deepEqual(compareColumns([a.ts, b.ts]), [0, 60, 120, 180, 240, 300])
  assert.ok(compareReadout(240, [a, b]).endsWith(' · A: median 8.0ms · B: no data'))
  assert.ok(compareReadout(60, [a, b]).endsWith(' · A: no replies · B: median 6.0ms'))
})
