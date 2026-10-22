// Small functions with a rule worth testing, kept free of React so a test can
// load them with nothing but Node.
import type { Target } from './api'

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

export const RANGES: { label: string; seconds: number }[] = [
  { label: '15m', seconds: 900 },
  { label: '1h', seconds: 3600 },
  { label: '6h', seconds: 21600 },
  { label: '24h', seconds: 86400 },
]

/**
 * Depth-first, indented order, so a tree reads as a tree even where it is
 * drawn as a flat list or a <select>.
 */
export function orderedTargets(targets: Target[]): { target: Target; depth: number }[] {
  const children = new Map<number | null, Target[]>()
  for (const t of targets) {
    const list = children.get(t.parent_id) ?? []
    list.push(t)
    children.set(t.parent_id, list)
  }
  for (const list of children.values()) {
    list.sort((a, b) => a.sort_order - b.sort_order || a.name.localeCompare(b.name))
  }
  const out: { target: Target; depth: number }[] = []
  const walk = (parent: number | null, depth: number) => {
    for (const t of children.get(parent) ?? []) {
      out.push({ target: t, depth })
      walk(t.id, depth + 1)
    }
  }
  walk(null, 0)
  return out
}

/**
 * One target seen from one agent. `agentId` is null when the target's
 * configured agent name(s) name nobody enrolled: there is no series to draw,
 * and that absence is itself the thing to show.
 */
export interface PlotSeries {
  target: Target
  agentId: number | null
  /** The agent to attribute the series to, or the unresolved names verbatim. */
  agentName?: string
}

/**
 * One series per (target, agent) pair: the same target seen from two vantage
 * points is two different measurements, and averaging them would destroy the
 * very thing that makes a second vantage point worth having.
 *
 * No fallback to the local agent. Querying agent 0 for a target assigned to
 * someone else attributes one vantage point's measurements to another, and it
 * made a target nobody measures look measured. The server counts these in
 * smokeng_targets_unmeasured; the screen should agree with it. Dropping the
 * target instead would only move the lie: a missing plot reads as "not
 * selected", and the misconfiguration that nothing measures this target is
 * precisely what the operator needs to see. So it keeps its place, with no
 * agent and a reason.
 */
export function resolveSeries(t: Target, byName: Map<string, number>, sep = ' '): PlotSeries[] {
  const names = (t.settings.agents.effective || 'local').split(/\s+/).filter(Boolean)
  const resolved = names
    .map((n) => ({ name: n, id: byName.get(n) }))
    .filter((a): a is { name: string; id: number } => a.id !== undefined)
  if (resolved.length === 0) return [{ target: t, agentId: null, agentName: names.join(sep) }]
  return resolved.map((a) => ({
    target: t,
    agentId: a.id,
    agentName: resolved.length > 1 ? a.name : undefined,
  }))
}

/**
 * Pooled over the whole window, from the samples themselves. With no replies
 * every quantile would otherwise read as 0 — not "unknown", but a specific,
 * wrong number that looks exactly like a suspiciously fast link — so the
 * latency figures are null then. Loss is still known (it is the whole story).
 */
export function summarise(s: {
  values: Uint32Array
  sent: Float64Array
  received: Float64Array
  flags: Uint8Array
}) {
  const all = s.values.toSorted()
  const q = (p: number) =>
    all.length ? all[Math.min(all.length - 1, Math.floor(p * all.length))] : null
  let sent = 0
  let received = 0
  let flags = 0
  for (let i = 0; i < s.sent.length; i++) {
    sent += s.sent[i]
    received += s.received[i]
    flags |= s.flags[i]
  }
  const p5 = q(0.05)
  const p95 = q(0.95)
  return {
    median: q(0.5),
    p95,
    spread: p5 === null || p95 === null ? null : p95 - p5,
    loss: sent > 0 ? 100 * (1 - received / sent) : 0,
    // Kept alongside loss: loss alone cannot tell "nothing sent" from "sent and
    // lost".
    sent,
    received,
    flags,
  }
}
