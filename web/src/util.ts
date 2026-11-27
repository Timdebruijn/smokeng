// Small functions with a rule worth testing, kept free of React so a test can
// load them with nothing but Node.
import { icmpErrorName, probeErrorName, sendReasonLabel, type Target } from './api.ts'

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
 * 21600 -> "6 hours": the spoken form of a window's length, exact to the
 * second. A RANGES button comes out as one unit; a window reached by zooming
 * out says every unit it has: 4440 -> "1 hour 14 minutes".
 */
export function rangeWords(seconds: number): string {
  let rest = Math.round(seconds)
  const parts: string[] = []
  for (const [size, unit] of [[86400, 'day'], [3600, 'hour'], [60, 'minute'], [1, 'second']] as const) {
    const n = Math.floor(rest / size)
    rest -= n * size
    if (n) parts.push(`${n} ${unit}${n === 1 ? '' : 's'}`)
  }
  return parts.join(' ') || '0 seconds'
}

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

/** Heading for the group a target sits in: its parent path without the
 *  leading slash, "Top level" directly under the root. */
export function graphGroupLabel(path: string): string {
  const parent = path.slice(0, path.lastIndexOf('/')).replace(/^\//, '')
  return parent === '' ? 'Top level' : parent
}

/** The items by group, in tree order (groups by first appearance). `grouped`
 *  is false when everything sits at the top level, so the page can stay flat. */
export function graphGroups(items: Target[], tree: Target[]): { grouped: boolean; groups: { label: string; items: Target[] }[] } {
  const rank = new Map(orderedTargets(tree).map((o, i) => [o.target.id, i]))
  const sorted = [...items].sort((a, b) => (rank.get(a.id) ?? 0) - (rank.get(b.id) ?? 0))
  const byLabel = new Map<string, Target[]>()
  for (const t of sorted) {
    const label = graphGroupLabel(t.path)
    byLabel.set(label, [...(byLabel.get(label) ?? []), t])
  }
  const groups = [...byLabel].map(([label, items]) => ({ label, items }))
  return { grouped: groups.some((g) => g.label !== 'Top level'), groups }
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

/** Alerts that still need attention: neither acknowledged nor silenced. */
export function countUnacked(alerts: { acked?: boolean; silenced?: boolean }[]): number {
  return alerts.filter((a) => !a.acked && !a.silenced).length
}

/**
 * What to say when the unacknowledged firing count is polled. `prev` is null
 * before the first poll. Only a rise is news: a fall is the user's own
 * acknowledge or an alert clearing, and an unchanged count is the 30 s poll
 * repeating itself. The first poll counts as a rise from zero, because the bell
 * button's name changes after the page was read and nothing else would tell
 * someone who just opened the page that alerts are firing.
 */
export function firingAnnouncement(prev: number | null, now: number): string | null {
  if (now <= (prev ?? 0)) return null
  return now === 1 ? '1 alert firing' : `${now} alerts firing`
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

// mayChange says whether a field on a node takes edits from this caller, the
// way the server decides it: write access comes per node from a grant, and a
// setting whose effect leaves the subtree stays with a global admin on every
// node. The global role in /api/v1/me cannot stand in for the first half: it is
// viewer for an editor of one subtree.
export function mayChange(target: Pick<Target, 'writable'>, isAdmin: boolean, adminOnly = false): boolean {
  return target.writable && (isAdmin || !adminOnly)
}

/** The group a target is listed under: its path minus the last segment, '' for the top level. */
export function groupOfPath(path: string): string {
  return path.split('/').filter(Boolean).slice(0, -1).join('/')
}

/** Splits rows into groups by their target's parent, keeping the order given (first appearance). */
export function groupRows<T extends { target: { path: string } }>(rows: T[]): { path: string; rows: T[] }[] {
  const groups = new Map<string, T[]>()
  for (const r of rows) {
    const p = groupOfPath(r.target.path)
    const list = groups.get(p)
    if (list) list.push(r)
    else groups.set(p, [r])
  }
  return [...groups].map(([path, rows]) => ({ path, rows }))
}

export type RowStatus = 'nodata' | 'ok' | 'flagged' | 'loss'

/** How a row is coloured: nothing sent, loss, quality flags, or fine. */
export function rowStatus(r: { sent: number; loss: number }, flagged: number): RowStatus {
  return r.sent === 0 ? 'nodata' : r.loss > 0 ? 'loss' : flagged !== 0 ? 'flagged' : 'ok'
}

export const STATUS_COLOR: Record<RowStatus, string> = { nodata: 'var(--dim)', loss: 'var(--bad)', flagged: 'var(--warn)', ok: 'var(--good)' }

const STATUS_RANK: RowStatus[] = ['nodata', 'ok', 'flagged', 'loss']

/** The worst of several statuses: no data < ok < flagged < loss. */
export function worstStatus(statuses: RowStatus[]): RowStatus {
  return statuses.reduce<RowStatus>((a, s) => (STATUS_RANK.indexOf(s) > STATUS_RANK.indexOf(a) ? s : a), 'nodata')
}

/**
 * Measurement-quality flags (store.Flag* in Go). A measurement is only worth
 * as much as the conditions it was taken under, so anything that weakens it
 * is surfaced next to the plot instead of being left to be inferred.
 */
/** store.FlagICMPError: the plot names each ICMP error rather than this bit alone. */
export const FLAG_ICMP_ERROR = 1 << 4

/** store.FlagSendFailed: probes that never went out. */
export const FLAG_SEND_FAILED = 1 << 6

export const QUALITY_FLAGS: { bit: number; label: string; title: string }[] = [
  { bit: 1 << 0, label: 'userspace TX', title: 'Send timestamps taken in userspace; scheduler jitter widens the smoke.' },
  { bit: 1 << 1, label: 'userspace RX', title: 'Receive timestamps taken in userspace; scheduler jitter widens the smoke.' },
  { bit: 1 << 2, label: 'raw socket', title: 'Unprivileged datagram ICMP was unavailable; running on raw sockets.' },
  {
    bit: 1 << 3,
    label: 'dropped replies',
    title: 'The receive queue overflowed: some loss shown here is ours, not the network’s.',
  },
  {
    bit: 1 << 5,
    label: 'clock step',
    title: 'The wall clock jumped during these intervals; affected RTTs are unreliable.',
  },
  {
    bit: 1 << 7,
    label: 'cut short',
    title:
      'This interval was finalized before its window closed — a restart or a settings change. Probes still in flight were abandoned rather than counted as lost, so its loss figure is not comparable with a whole interval.',
  },
  {
    bit: 1 << 6,
    label: 'not sent',
    // Deliberately generic. The reason badges beside the graph carry the
    // detail, and they now cover cases this title used to exclude: a far end
    // refusing the traffic, which arrives as a local error, and this prober's
    // own deadline expiring, where nothing was asked of the target at all.
    // Worded so it also reads right in a list away from those badges.
    title: 'These probes never went out, and count as lost. The reason badges beside the graph say why — and whether the cause was here or at the far end.',
  },
  {
    bit: FLAG_ICMP_ERROR,
    label: 'ICMP errors',
    title: 'Probes were refused with an ICMP error rather than going unanswered.',
  },
]

/** The quality flags in an OR of flags that count against this probe type (countedFlags). */
export function flagsIn(flags: number, probeType: unknown): typeof QUALITY_FLAGS {
  return QUALITY_FLAGS.filter((f) => countedFlags(flags & f.bit, probeType) !== 0)
}

/**
 * Per counted flag in an interval-by-interval series: how many intervals carried
 * it, and when the last of them started. Not for a pooled read, whose rows are
 * columns of many intervals.
 */
export function flagCounts(
  s: { ts: ArrayLike<number>; flags: ArrayLike<number>; pooled?: unknown },
  probeType: unknown,
): { label: string; title: string; count: number; last: number }[] {
  // A pooled row is a column of many intervals: counting rows would count columns.
  if (s.pooled) return []
  return flagsIn(QUALITY_FLAGS.reduce((a, f) => a | f.bit, 0), probeType).flatMap((f) => {
    let count = 0
    let last = 0
    for (let i = 0; i < s.flags.length; i++) {
      if (!(s.flags[i] & f.bit)) continue
      count++
      last = Math.max(last, s.ts[i])
    }
    return count > 0 ? [{ label: f.label, title: f.title, count, last }] : []
  })
}

/** One flag on an overview row: "cut short in 1 of 12 intervals, last at 22:25". */
export function flagNote(f: { label: string; count: number }, intervals: number, lastAt: string): string {
  return f.count === intervals
    ? `${f.label} in every interval`
    : `${f.label} in ${f.count} of ${intervals} ${intervals === 1 ? 'interval' : 'intervals'}, last at ${lastAt}`
}

/**
 * What to tell an admin about webhook delivery, or null when there is nothing
 * to say. Words, not a colour, and with when, so "failing" can be told from
 * "failed once last week". A batch dropped in the last day is said even after
 * deliveries recovered: it is not recovered with them, and a resolve in it is
 * never announced again.
 */
export function deliveryWarning(
  d: { ok: number; failed: number; dropped: number; failing: boolean; last_ok?: number; last_failed_at?: number; last_error?: string; last_dropped_at?: number },
  at: (unixSeconds: number) => string = (ts) => new Date(ts * 1000).toLocaleString(),
  now = Date.now() / 1000,
): string | null {
  const drops = d.dropped > 0 ? ` ${d.dropped} ${d.dropped === 1 ? 'batch' : 'batches'} dropped since start.` : ''
  if (!d.failing) {
    if (!d.last_dropped_at || now - d.last_dropped_at > 86400) return null
    return `Alert webhook deliveries are going through again, but the queue was full at ${at(d.last_dropped_at)} and dropped a batch unsent; a transition in it was never delivered.${drops}`
  }
  const parts: string[] = []
  if (d.last_failed_at && (!d.last_dropped_at || d.last_failed_at >= d.last_dropped_at)) {
    parts.push(`The last delivery, at ${at(d.last_failed_at)}, failed: ${d.last_error ?? 'no reason given'}.`)
  } else if (d.last_dropped_at) {
    parts.push(`The delivery queue was full at ${at(d.last_dropped_at)} and dropped the oldest batch unsent.`)
  }
  parts.push(d.last_ok ? `The last one that went through was at ${at(d.last_ok)}.` : 'None has gone through since the master started.')
  return `Alert webhook deliveries are failing. ${parts.join(' ')}${drops}`
}

/** How long ago, in words: "just now", "40s ago", "3 hours ago". */
export function relativeTime(unixSeconds: number | undefined, now = Date.now() / 1000): string {
  if (unixSeconds === undefined) return 'never'
  const deltaS = Math.floor(now) - unixSeconds
  if (deltaS < 5) return 'just now'
  if (deltaS < 60) return `${deltaS}s ago`
  const m = Math.round(deltaS / 60)
  if (m < 60) return `${m} minute${m === 1 ? '' : 's'} ago`
  const h = Math.round(m / 60)
  if (h < 24) return `${h} hour${h === 1 ? '' : 's'} ago`
  const d = Math.round(h / 24)
  return `${d} day${d === 1 ? '' : 's'} ago`
}

/**
 * Why an agent's targets may have no recent data, when its silence is the
 * reason: it has never reported, or not as often as its targets say it should
 * (state and report_every_s from /api/v1/agents). null while it is reporting,
 * or when its silence means nothing.
 */
export function agentQuietNote(
  a: { name: string; state?: string; report_every_s?: number; last_seen?: number },
  now = Date.now() / 1000,
): string | null {
  if (a.state === 'never') return `${a.name} has never reported`
  if ((a.state === 'late' || a.state === 'silent') && a.report_every_s) {
    return `${a.name} is ${a.state}: last reported ${relativeTime(a.last_seen, now)}, expected every ${rangeWords(a.report_every_s)}`
  }
  return null
}

/** What a probe-error badge or note means, wherever it is shown. */
export const PROBE_ERROR_TITLE =
  'Why probes failed at the service — a refused connection, an HTTP status, a DNS response code — so they count as lost.'

/** Per probe error in a series (Series.probeErrors): see codeCounts. */
export function probeErrorCounts(s: { ts: ArrayLike<number>; probeErrors: (number | null)[] }) {
  return codeCounts(s.ts, s.probeErrors)
}

/**
 * Per code in one of a series' per-row codes (probe errors, ICMP errors, send
 * reasons): how many rows carried it, and when the last of them started;
 * commonest first, then by code.
 */
export function codeCounts(
  ts: ArrayLike<number>,
  codes: (number | null)[],
): { code: number; count: number; last: number }[] {
  const by = new Map<number, { code: number; count: number; last: number }>()
  for (let i = 0; i < codes.length; i++) {
    const code = codes[i]
    if (code === null || code === undefined) continue
    const e = by.get(code) ?? { code, count: 0, last: 0 }
    e.count++
    e.last = Math.max(e.last, ts[i])
    by.set(code, e)
  }
  return [...by.values()].sort((a, b) => b.count - a.count || a.code - b.code)
}

/** What a positional-loss note means, wherever it is shown. */
export const POSITIONAL_TITLE =
  'Probes were lost at fixed places in the send order, which is consistent with rate limiting by the target, not proof of it.'

/** How many rows of a raw read were flagged as positional loss, and when the last started. */
export function positionalCount(s: { ts: ArrayLike<number>; lossPositions: (number[] | null)[] }): { count: number; last: number } {
  let count = 0
  let last = 0
  for (let i = 0; i < s.lossPositions.length; i++) {
    if (s.lossPositions[i] === null) continue
    count++
    last = Math.max(last, s.ts[i])
  }
  return { count, last }
}

/** Whether a series' ICMP errors are ICMPv6: its own family, else its target's. */
export function isV6(family: string | undefined, target: { address_family: string | null }): boolean {
  return (family ?? target.address_family) === 'v6'
}

/**
 * The flags as a list names them, less the two a list names more exactly when
 * it has their codes: "ICMP errors" when it names the ICMP error, "not sent"
 * when it names the send reason.
 */
export function namedFlags<F extends { label: string }>(flags: F[], icmpNamed: boolean, sendNamed: boolean): F[] {
  return flags.filter((f) => !(f.label === 'ICMP errors' && icmpNamed) && !(f.label === 'not sent' && sendNamed))
}

/**
 * What an overview row says under its host, from an interval-by-interval read:
 * each counted flag, with the ICMP error and the send reason named rather than
 * left as "ICMP errors" and "not sent" when the read says which; the probe
 * errors; and positional loss. Each as "<what> in N of M intervals, last at T".
 * Intervals flagged without a code (stored before codes were, or by an older
 * agent) keep the bare flag, counted on their own.
 */
export function seriesNotes(
  s: {
    ts: ArrayLike<number>
    flags: ArrayLike<number>
    icmpErrors: (number | null)[]
    sendErrors: (number | null)[]
    probeErrors: (number | null)[]
    lossPositions: (number[] | null)[]
  },
  probeType: unknown,
  at: (unixSeconds: number) => string,
  v6 = false,
): string[] {
  const n = s.ts.length
  const note = (label: string, e: { count: number; last: number }) => flagNote({ label, count: e.count }, n, at(e.last))
  const icmp = codeCounts(s.ts, s.icmpErrors)
  const send = codeCounts(s.ts, s.sendErrors)
  const positional = positionalCount(s)
  // Flagged, and no code to name it by.
  const uncoded = (bit: number, codes: (number | null)[]) => {
    let count = 0
    let last = 0
    for (let i = 0; i < n; i++) {
      if (!(s.flags[i] & bit) || codes[i] !== null) continue
      count++
      last = Math.max(last, s.ts[i])
    }
    return { count, last }
  }
  const remainder = (label: string, e: { count: number; last: number }) => (e.count > 0 ? [note(label, e)] : [])
  return [
    ...namedFlags(flagCounts(s, probeType), icmp.length > 0, send.length > 0).map((f) => note(f.label, f)),
    ...icmp.map((e) => note(icmpErrorName(e.code, v6), e)),
    ...(icmp.length > 0 ? remainder('ICMP errors', uncoded(FLAG_ICMP_ERROR, s.icmpErrors)) : []),
    ...send.map((e) => note(`not sent: ${sendReasonLabel(e.code)}`, e)),
    ...(send.length > 0 ? remainder('not sent', uncoded(FLAG_SEND_FAILED, s.sendErrors)) : []),
    // Why probes failed at the service, which the loss column cannot say.
    ...probeErrorCounts(s).map((e) => note(probeErrorName(e.code), e)),
    ...(positional.count > 0 ? [note('positional loss', positional)] : []),
  ]
}
