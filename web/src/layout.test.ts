import assert from 'node:assert/strict'
import { test } from 'node:test'
import { gapsBefore } from './layout.ts'

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
