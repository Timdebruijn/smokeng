/**
 * Plot geometry in CSS pixels, shared by the worker renderer (which scales it
 * by devicePixelRatio) and the main-thread overlay (which draws in CSS px).
 * Keeping one source of truth is what makes the crosshair line up with the
 * density it points at.
 */
export const AXIS_W = 56
export const AXIS_H = 18
export const RAIL_H = 10
export const RAIL_GAP = 4
export const PLOT_HEIGHT = 240

/** Height of the density area itself, above the loss rail and time axis. */
export function densityHeight(cssHeight: number): number {
  return cssHeight - RAIL_H - RAIL_GAP - AXIS_H
}

export function fmtUs(us: number): string {
  if (us < 1000) return `${Math.round(us)}µs`
  if (us < 1_000_000) return `${(us / 1000).toFixed(us < 10_000 ? 1 : 0)}ms`
  return `${(us / 1_000_000).toFixed(1)}s`
}

/**
 * The same, for a signed measure. Zero is written bare: a jitter axis reads
 * better with one unambiguous origin than with "+0µs".
 */
export function fmtSigned(us: number): string {
  const a = Math.abs(us)
  // Sub-microsecond precision below 10µs. These are axis positions, not
  // measurements, and rounding every one of them to whole microseconds gave a
  // low-jitter target five gridlines labelled "\u22121µs, \u22121µs, 0, +1µs,
  // +1µs" — a scale that says less the closer you look at it.
  if (a < 10) {
    const v = Math.round(us * 10) / 10
    if (v === 0) return '0'
    return (v > 0 ? '+' : '\u2212') + Math.abs(v).toFixed(1) + 'µs'
  }
  return (us > 0 ? '+' : '\u2212') + fmtUs(a)
}

export function fmtClock(unixSeconds: number): string {
  const d = new Date(unixSeconds * 1000)
  const p = (n: number) => String(n).padStart(2, '0')
  return `${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`
}

/** Median gap between consecutive rows: the interval, inferred from the data. */
export function inferSpan(ts: Float64Array | ArrayLike<number>, n: number): number {
  if (n < 2) return 60
  const diffs: number[] = []
  for (let i = 1; i < n; i++) diffs.push(ts[i] - ts[i - 1])
  diffs.sort((a, b) => a - b)
  return diffs[Math.floor(diffs.length / 2)] || 60
}
