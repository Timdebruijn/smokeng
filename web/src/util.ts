// Small functions with a rule worth testing, kept free of React so a test can
// load them with nothing but Node.

/**
 * The smallest and largest of the numbers in values, or null if there are none.
 * Not a number is not a value and is skipped, where Math.min would make the
 * whole answer NaN.
 *
 * Math.min(...xs) is not an option for data: each element becomes an argument,
 * and the engine throws a RangeError past about 125,000, which a golden
 * reference can exceed. Rendering threw, and the whole page went blank.
 */
export function minMax(values: ArrayLike<number>): { lo: number; hi: number } | null {
  let lo = Infinity
  let hi = -Infinity
  let any = false
  for (let i = 0; i < values.length; i++) {
    const v = values[i]
    if (v !== v) continue // NaN
    any = true
    if (v < lo) lo = v
    if (v > hi) hi = v
  }
  return any ? { lo, hi } : null
}

/**
 * One CSV cell. A number is written as itself, and as nothing at all if it is
 * not finite. Text is quoted, with a leading apostrophe where it starts with a
 * character a spreadsheet reads as the start of a formula (= + - @, tab,
 * return): a target name is whatever an editor typed, and a formula can run a
 * command or send the sheet's contents somewhere.
 */
export function csvCell(v: string | number): string {
  if (typeof v === 'number') return Number.isFinite(v) ? String(v) : ''
  const safe = /^[=+\-@\t\r]/.test(v) ? `'${v}` : v
  return `"${safe.replace(/"/g, '""')}"`
}
