import assert from 'node:assert/strict'
import { test } from 'node:test'
import { chartSummary, compareSummary, fmtPositions, positionalTitle, replyLines } from './format.ts'

test('a plot says what it shows: window, typical and highest median, loss', () => {
  const base = { name: 'router v4', when: '10:00 – 11:00' }
  assert.equal(
    chartSummary({ ...base, medians: [1000, 3000, 2000], intervals: 3, sent: [10, 10, 10], received: [10, 9, 10] }),
    'router v4, 10:00 – 11:00: 3 intervals, typical median 2.0ms, highest median 3.0ms, loss 3.3%.',
  )
})

test('an empty plot says there is no data, and an all-lost one says no replies', () => {
  const base = { name: 'router v4', when: '10:00 – 11:00' }
  assert.equal(
    chartSummary({ ...base, medians: [], intervals: 0, sent: [], received: [] }),
    'router v4, 10:00 – 11:00: no data in this window.',
  )
  assert.equal(
    chartSummary({ ...base, medians: [NaN, NaN], intervals: 2, sent: [5, 5], received: [0, 0] }),
    'router v4, 10:00 – 11:00: 2 intervals, no replies, loss 100.0%.',
  )
  assert.equal(
    chartSummary({ ...base, medians: [NaN], intervals: 1, sent: [0], received: [0] }),
    'router v4, 10:00 – 11:00: 1 interval, no replies, nothing sent.',
  )
})

test('the compare summary names every line, also one without replies', () => {
  assert.equal(
    compareSummary('10:00 – 11:00', [
      { label: 'a', medians: [1000, null, 3000] },
      { label: 'b', medians: [null] },
    ]),
    'Compare pooled medians, 10:00 – 11:00. a: typical median 3.0ms, highest 3.0ms. b: no replies.',
  )
  assert.equal(compareSummary('w', []), 'Compare, w: no series selected.')
})

test('send positions are written as ranges, runs joined and gaps kept', () => {
  assert.equal(fmtPositions([16, 17, 18, 19, 20]), '16–20')
  assert.equal(fmtPositions([8]), '8')
  assert.equal(fmtPositions([4, 8, 12, 16, 20]), '4, 8, 12, 16, 20')
  assert.equal(fmtPositions([1, 2, 5, 6, 7, 9]), '1–2, 5–7, 9')
  assert.equal(fmtPositions([]), '')
})

test('the badge title says the pattern, with and without a position that stands out', () => {
  assert.equal(
    positionalTitle([16, 17, 18, 19, 20], 43),
    'Loss concentrated at probes 16–20 of each interval, over the last 43 intervals with loss. That does not fit loss that falls where it happens to: it is consistent with rate limiting by the target, and not necessarily network loss.',
  )
  assert.equal(
    positionalTitle([], 43),
    'Loss leans to one end of the send order, over the last 43 intervals with loss. That does not fit loss that falls where it happens to: it is consistent with rate limiting by the target, and not necessarily network loss.',
  )
})

// Protects the cursor readout from describing an interval that sent nothing as
// "0/0 replies" or "no reply", which would say the target was asked.
test('the readout says no data for an interval that sent nothing, and the replies otherwise', () => {
  assert.deepEqual(replyLines(0, 0, NaN, NaN), ['no data'])
  assert.deepEqual(replyLines(10, 10, 2500, 0), ['median 2.5ms', '10/10 replies'])
  assert.deepEqual(replyLines(10, 0, NaN, 1), ['no reply', '0/10 replies · 100.0% loss'])
})
