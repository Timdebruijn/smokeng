/**
 * How measurements are written down. Shared, because the same number rendered
 * two ways in two places is how a screen ends up contradicting itself.
 */

/** Microseconds, at the precision the magnitude deserves. */
export function fmtUs(us: number): string {
  if (us < 1000) return `${Math.round(us)}µs`
  if (us < 1_000_000) return `${(us / 1000).toFixed(us < 10_000 ? 1 : 0)}ms`
  return `${(us / 1_000_000).toFixed(1)}s`
}

/**
 * Loss as a percentage, never rounding a real loss down to nothing.
 *
 * One packet in seven thousand is 0.014%, which `toFixed(1)` writes as "0.0%"
 * — while the cell beside it was coloured red, because the underlying number
 * is not zero. The reader is then looking at a screen that says no loss and
 * means loss. Zero is zero; anything smaller than the precision on show says
 * so as an inequality.
 */
export function fmtLoss(pct: number): string {
  if (pct <= 0) return '0.0%'
  if (pct < 0.1) return '<0.1%'
  return `${pct.toFixed(1)}%`
}

/**
 * What the cursor readout says about the replies of one interval or pooled
 * column. An interval that sent nothing (the prober could not ask: no address,
 * no route) is not "0/0 replies" and not "no reply", which would say the target
 * was asked and did not answer; it is no data.
 */
export function replyLines(sent: number, received: number, median: number, loss: number): string[] {
  if (sent === 0) return ['no data']
  return [
    Number.isNaN(median) ? 'no reply' : `median ${fmtUs(median)}`,
    `${received}/${sent} replies${loss > 0 ? ` · ${fmtLoss(loss * 100)} loss` : ''}`,
  ]
}

/**
 * Send positions (1-based, ascending) the way an operator reads them, runs as
 * ranges: [16, 17, 18, 19, 20] is "16–20", [4, 8, 12] is "4, 8, 12". The same
 * notation `smokeng diag loss-pattern` prints.
 */
export function fmtPositions(at: readonly number[]): string {
  const parts: string[] = []
  for (let i = 0; i < at.length; i++) {
    let j = i
    while (j + 1 < at.length && at[j + 1] === at[j] + 1) j++
    parts.push(j > i ? `${at[i]}–${at[j]}` : `${at[i]}`)
    i = j
  }
  return parts.join(', ')
}

/**
 * What the positional-loss badge says on hover, for one flagged interval's
 * finding. Deliberately a statement about the pattern, not its cause: the test
 * says the loss does not fall where chance would put it, and rate limiting by
 * the target is one thing that fits that, not something established.
 */
export function positionalTitle(positions: readonly number[], window: number): string {
  const where =
    positions.length > 0
      ? `Loss concentrated at probes ${fmtPositions(positions)} of each interval`
      : 'Loss leans to one end of the send order'
  return (
    `${where}, over the last ${window} intervals with loss. That does not fit loss that falls ` +
    'where it happens to: it is consistent with rate limiting by the target, and not necessarily network loss.'
  )
}
