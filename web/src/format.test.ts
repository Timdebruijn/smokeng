import assert from 'node:assert/strict'
import { test } from 'node:test'
import { fmtPositions, positionalTitle, replyLines } from './format.ts'

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
