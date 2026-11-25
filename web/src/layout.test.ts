import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { test } from 'node:test'
import { gapsBefore, lineStyle, stepRow } from './layout.ts'

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
  assert.equal(seen((i) => lineStyle(i).prop), 8)
  assert.equal(seen((i) => lineStyle(i).dash.join()), 8)
  assert.deepEqual(lineStyle(8), lineStyle(0))
  // Each property is defined in the stylesheet, for both themes, with a pair of its own.
  const css = readFileSync(new URL('./styles.css', import.meta.url), 'utf8')
  const values = Array.from({ length: 8 }, (_, i) => {
    const m = css.match(new RegExp(`${lineStyle(i).prop}: (light-dark\\(#\\w+, #\\w+\\));`))
    assert.ok(m, `${lineStyle(i).prop} is not defined with light-dark()`)
    return m[1]
  })
  assert.equal(new Set(values).size, 8)
})
