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

// How often the live screens refresh. One value, so "live" means the same thing
// on Overview as on Graphs.
export const REFRESH_MS = 10_000

export const RANGES: { label: string; seconds: number }[] = [
  { label: '15m', seconds: 900 },
  { label: '1h', seconds: 3600 },
  { label: '6h', seconds: 21600 },
  { label: '24h', seconds: 86400 },
  { label: '7d', seconds: 7 * 86400 },
  { label: '30d', seconds: 30 * 86400 },
]

/**
 * How often a trailing window of `rangeS` seconds refreshes: every REFRESH_MS,
 * or about once per pixel column of a 1000-pixel plot if that is longer. A month
 * moves by one column in 43 minutes, and redrawing it every ten seconds would
 * only re-read the same month.
 */
export function refreshMs(rangeS: number): number {
  // One column of a thousand is rangeS/1000 seconds, which is rangeS milliseconds.
  return Math.max(REFRESH_MS, rangeS)
}

/**
 * The window twice as wide around the same centre. One that would reach past
 * `now` becomes a trailing window of that width instead (rangeS), so zooming
 * out of the present keeps it live rather than freezing it.
 */
export function zoomOut(from: number, to: number, now: number): { from: number; to: number } | { rangeS: number } {
  const span = to - from
  const centre = from + span / 2
  if (centre + span >= now) return { rangeS: 2 * span }
  return { from: Math.floor(centre - span), to: Math.ceil(centre + span) }
}

/** Unix seconds as the value of a datetime-local input, in local time. */
export function toLocalInput(unixSeconds: number): string {
  const d = new Date(unixSeconds * 1000)
  const p = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}T${p(d.getHours())}:${p(d.getMinutes())}`
}

/** A datetime-local value (local time) as unix seconds, or null if it is not one. */
export function fromLocalInput(value: string): number | null {
  if (!/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}(:\d{2})?$/.test(value)) return null
  const ms = new Date(value).getTime()
  return Number.isFinite(ms) ? Math.floor(ms / 1000) : null
}

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

export type Family = 'v4' | 'v6'

/** What the header's family filter can say: one family, or both. */
export type FamilyChoice = Family | 'both'

/**
 * A stored or typed choice as a choice. Anything else, including nothing at
 * all, is "both": a filter that hid series because of a garbled value would be
 * a page that silently shows less than is measured.
 */
export function parseFamilyChoice(raw: string | null | undefined): FamilyChoice {
  return raw === 'v4' || raw === 'v6' ? raw : 'both'
}

/**
 * The families a target is drawn in, one entry per series family: `v4` then
 * `v6` for a target measured over both, and a single unset entry for any other,
 * whose one series needs no family on the wire or on screen.
 */
export function seriesFamilies(t: Target): (Family | undefined)[] {
  return t.address_family === 'both' ? ['v4', 'v6'] : [undefined]
}

/**
 * Whether the filter lets a series of this target through. A series without a
 * family belongs to the one family of a single-family target (v6 only for
 * `v6`), or, when it stands for a `both` target as a whole, to both of them.
 */
export function showsFamily(choice: FamilyChoice, t: Target, family?: Family): boolean {
  if (choice === 'both') return true
  if (family) return family === choice
  if (t.address_family === 'both') return true
  return (t.address_family === 'v6' ? 'v6' : 'v4') === choice
}

/** The families of `t` that the filter lets through; see seriesFamilies. */
export function shownFamilies(t: Target, choice: FamilyChoice): (Family | undefined)[] {
  return seriesFamilies(t).filter((f) => showsFamily(choice, t, f))
}

/** What identifies one series on screen: React keys and legend keys. */
export function seriesKey(targetId: number, agentId: number | null, family?: Family): string {
  return `${targetId}-${agentId ?? 'unresolved'}${family ? `-${family}` : ''}`
}

/**
 * One target seen from one agent, in one family. `agentId` is null when the
 * target's configured agent name(s) name nobody enrolled: there is no series to
 * draw, and that absence is itself the thing to show.
 */
export interface PlotSeries {
  target: Target
  agentId: number | null
  /** The agent to attribute the series to, or the unresolved names verbatim. */
  agentName?: string
  /** Set only for a target measured over both families. */
  family?: Family
}

/** The series the filter lets through. */
export function filterByFamily(series: PlotSeries[], choice: FamilyChoice): PlotSeries[] {
  return series.filter((s) => showsFamily(choice, s.target, s.family))
}

/**
 * One series per (target, agent, family): the same target seen from two vantage
 * points is two different measurements, and averaging them would destroy the
 * very thing that makes a second vantage point worth having. The same goes for
 * the two families of a target measured over both: a host that answers over v4
 * and not over v6 is the finding.
 *
 * No fallback to the local agent. Querying agent 0 for a target assigned to
 * someone else attributes one vantage point's measurements to another, and it
 * made a target nobody measures look measured. The server counts these in
 * smokeng_targets_unmeasured; the screen should agree with it. Dropping the
 * target instead would only move the lie: a missing plot reads as "not
 * selected", and the misconfiguration that nothing measures this target is
 * precisely what the operator needs to see. So it keeps its place, with no
 * agent and a reason, and once: it is the same absence in either family.
 */
export function resolveSeries(t: Target, byName: Map<string, number>, sep = ' '): PlotSeries[] {
  const names = (t.settings.agents.effective || 'local').split(/\s+/).filter(Boolean)
  const resolved = names
    .map((n) => ({ name: n, id: byName.get(n) }))
    .filter((a): a is { name: string; id: number } => a.id !== undefined)
  if (resolved.length === 0) return [{ target: t, agentId: null, agentName: names.join(sep) }]
  return resolved.flatMap((a) =>
    seriesFamilies(t).map((family) => ({
      target: t,
      agentId: a.id,
      agentName: resolved.length > 1 ? a.name : undefined,
      family,
    })),
  )
}

export type ScreenName = 'overview' | 'graphs' | 'targets' | 'alerts' | 'agents' | 'access' | 'detail' | 'signin'

/** The document title of a screen. Detail is named after its target; until that
 *  is loaded it says "Target" rather than a title that would be wrong. */
export function screenTitle(screen: ScreenName, detailName?: string | null): string {
  const name =
    screen === 'detail' ? detailName || 'Target' : screen === 'signin' ? 'Sign in' : screen[0].toUpperCase() + screen.slice(1)
  return `${name} - smokeng`
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

/**
 * What identifies one firing alert. A target measured over both families can
 * fire one rule twice, so the family is part of the key: without it two rows
 * collide and an action on one reads as an action on the other.
 */
export function alertKey(a: {
  rule_id: number
  target_id: number
  agent_id: number
  family: string
}): string {
  return `${a.rule_id}/${a.target_id}/${a.agent_id}/${a.family}`
}

/** "local · v4": the vantage point and family as text, either part optional. */
export function seriesLabel(agent: string | undefined, family: string | undefined): string {
  return [agent, family].filter((x) => x).join(' · ')
}

/**
 * A label as what follows a name, " · local · v4", and nothing at all when
 * there is no label, so that a row never ends in a separator with nothing
 * after it.
 */
export function afterName(label: string | undefined): string {
  return label ? ` · ${label}` : ''
}

/** "loss on /Production/web, local · v4": one alert, named for a control that acts on it. */
export function alertName(a: { rule: string; target: string; agent?: string; family?: string }): string {
  const series = seriesLabel(a.agent, a.family)
  return `${a.rule} on ${a.target}${series ? `, ${series}` : ''}`
}

/** store.FlagUserspaceTX | store.FlagUserspaceRX. */
const USERSPACE_FLAGS = 0b11

/**
 * The flags that count against how cleanly a series was measured: all of them,
 * less the userspace pair on a probe type that carries it on every interval
 * with a reply.
 * tcp, http(s) and irtt are timed around a userspace call or handed a duration,
 * so the pair is how they work, and a dot that can never be green says
 * nothing. icmp and dns are stamped by the kernel: there the pair means that
 * failed, and it counts.
 */
export function countedFlags(flags: number, probeType: unknown): number {
  return ['tcp', 'http', 'https', 'irtt'].includes(String(probeType)) ? flags & ~USERSPACE_FLAGS : flags
}

/**
 * What a measurement status dot says in words. `sent` and `loss` are left out
 * where the dot does not know them (a plot header knows only its flags).
 */
export function measurementLabel(s: { sent?: number; loss?: number; flagged?: number }): string {
  if (s.sent === 0) return 'no data'
  if ((s.loss ?? 0) > 0) return 'loss'
  if ((s.flagged ?? 0) > 0) return 'flagged'
  return 'clean'
}
