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
