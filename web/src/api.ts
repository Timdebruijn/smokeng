import { tableFromIPC } from '@uwdata/flechette'

export interface Me {
  /** The master's own version. */
  version?: string
  /** Where agents reach this instance, when a proxy sits in front. */
  external_url?: string
  /** Only present for an admin: what an authenticated user with no grant may do. */
  default_role?: 'viewer' | 'none'
  authenticated: boolean
  auth_enabled: boolean
  role?: 'viewer' | 'admin'
  email?: string
  name?: string
  subject?: string
}

export async function fetchMe(): Promise<Me> {
  const r = await fetch('/api/v1/me', { cache: 'no-store' })
  if (!r.ok) throw new Error(`me: HTTP ${r.status}`)
  return (await r.json()) as Me
}

export async function logout(): Promise<void> {
  await fetch('/auth/logout', { method: 'POST' })
}

export interface AlertRule {
  id: number
  target_id: number
  name: string
  metric: 'loss' | 'median' | 'p95' | 'spread' | 'shape' | 'bimodality'
  op: '>' | '<'
  threshold: number
  for_intervals: number
  clear_intervals: number
  enabled: boolean
  /** Shape metrics only: 'auto' self-calibrates, 'tunable' uses the threshold. */
  mode?: 'auto' | 'tunable' | ''
  /** Shape metric only: 'rolling' recent history, or 'golden' captured reference. */
  baseline?: 'rolling' | 'golden' | ''
  describes: string
}

export interface FiringAlert {
  rule: string
  metric: string
  target: string
  host: string
  agent: string
  value: number
  since?: number
  describes: string
  /** Identifiers the acknowledge call needs — names alone cannot address one. */
  rule_id: number
  target_id: number
  agent_id: number
  /** Acknowledged: still firing, but marked seen so it stops demanding attention. */
  acked: boolean
  acked_by?: string
  acked_at?: number
  /** Silenced: still firing, but delivery is suppressed and the UI shows it muted. */
  silenced: boolean
  silenced_until?: number
}

/** A silence: matching alerts are suppressed over [starts_at, ends_at). */
export interface Silence {
  id: number
  /** Scope. Absent means every target/agent/rule; target_id covers a subtree. */
  target_id?: number
  target?: string
  agent_id?: number
  rule_id?: number
  starts_at: number
  ends_at: number
  reason: string
  created_by: string
  created_at: number
  /** Derived: the window covers now, or is still in the future. */
  active: boolean
  upcoming: boolean
}

export async function fetchSilences(): Promise<Silence[]> {
  const r = await fetch('/api/v1/silences', { cache: 'no-store' })
  if (!r.ok) throw new Error(`silences: HTTP ${r.status}`)
  return ((await r.json()) as { silences: Silence[] }).silences ?? []
}

/**
 * Book a silence. Give a `duration_s` for the quick "silence for N hours" from
 * now, or an explicit `starts_at`/`ends_at` window for a maintenance window.
 */
export function createSilence(body: {
  target_id?: number
  agent_id?: number
  rule_id?: number
  starts_at?: number
  ends_at?: number
  duration_s?: number
  reason?: string
}): Promise<unknown> {
  return mutate('/api/v1/silences', 'POST', body)
}

export function deleteSilence(id: number): Promise<unknown> {
  return mutate(`/api/v1/silences/${id}`, 'DELETE')
}

/** A captured reference distribution for a golden-baseline shape rule. */
export interface AlertBaseline {
  rule_id: number
  target_id: number
  agent_id: number
  from_ts: number
  to_ts: number
  intervals: number
  samples: number
  captured_at: number
  captured_by: string
}

export async function fetchAlertBaselines(): Promise<AlertBaseline[]> {
  const r = await fetch('/api/v1/alert-baselines', { cache: 'no-store' })
  if (!r.ok) throw new Error(`baselines: HTTP ${r.status}`)
  return ((await r.json()) as { baselines: AlertBaseline[] }).baselines ?? []
}

/** Capture what a window measured as a shape rule's reference. */
export function captureBaseline(
  ruleId: number,
  body: { target_id?: number; agent_id?: number; from?: number; to?: number },
): Promise<unknown> {
  return mutate(`/api/v1/alert-rules/${ruleId}/baseline`, 'POST', body)
}

export function clearBaseline(ruleId: number): Promise<unknown> {
  return mutate(`/api/v1/alert-rules/${ruleId}/baseline`, 'DELETE')
}

/** The two distributions a fired shape alert is about, for the overlay. */
export interface ShapeReference {
  rule_id: number
  target_id: number
  agent_id: number
  kind: 'golden' | 'rolling' | ''
  available: boolean
  reference: number[] | null
  current: number[] | null
}

export async function fetchShapeReference(
  ruleId: number,
  targetId: number,
  agentId: number,
): Promise<ShapeReference> {
  const p = new URLSearchParams({
    rule_id: String(ruleId),
    target_id: String(targetId),
    agent_id: String(agentId),
  })
  const r = await fetch(`/api/v1/shape-reference?${p.toString()}`, { cache: 'no-store' })
  if (!r.ok) throw new Error(`shape reference: HTTP ${r.status}`)
  return (await r.json()) as ShapeReference
}

/** One outage in an availability report: a contiguous run of down intervals. */
export interface DowntimeEpisode {
  start_ts: number
  end_ts: number
  duration_s: number
  intervals: number
}

/** Availability over a window, with coverage kept separate from availability. */
export interface AvailabilityReport {
  from: number
  to: number
  interval_s: number
  down_threshold_pct: number
  window_s: number
  up_s: number
  down_s: number
  unknown_s: number
  covered_s: number
  up_intervals: number
  down_intervals: number
  has_data: boolean
  /** up_s / covered_s — over measured time only. 0 when there is no data. */
  availability: number
  /** covered_s / window_s — how much of the window was actually measured. */
  coverage: number
  downtime: DowntimeEpisode[]
}

export interface AvailabilityResponse {
  target: string
  target_id: number
  from: number
  to: number
  interval_s: number
  down_threshold_pct: number
  agents: { agent_id: number; agent: string; report: AvailabilityReport }[]
}

export async function fetchAvailability(
  targetId: number,
  from: number,
  to: number,
  downThreshold?: number,
): Promise<AvailabilityResponse> {
  const p = new URLSearchParams({
    target_id: String(targetId),
    from: String(from),
    to: String(to),
  })
  if (downThreshold !== undefined) p.set('down_threshold', String(downThreshold))
  const r = await fetch(`/api/v1/availability?${p.toString()}`, { cache: 'no-store' })
  if (!r.ok) throw new Error(`availability: HTTP ${r.status}`)
  return (await r.json()) as AvailabilityResponse
}

/**
 * Acknowledge a firing alert, or clear the acknowledgement with `ack: false`.
 * The alert keeps firing; this only quiets the UI's attention.
 */
export function acknowledgeAlert(
  a: Pick<FiringAlert, 'rule_id' | 'target_id' | 'agent_id'>,
  ack: boolean,
): Promise<unknown> {
  return mutate('/api/v1/alerts/ack', 'POST', {
    rule_id: a.rule_id,
    target_id: a.target_id,
    agent_id: a.agent_id,
    ack,
  })
}

export async function fetchAlertRules(): Promise<AlertRule[]> {
  const r = await fetch('/api/v1/alert-rules', { cache: 'no-store' })
  if (!r.ok) throw new Error(`alert rules: HTTP ${r.status}`)
  return ((await r.json()) as { rules: AlertRule[] }).rules ?? []
}

export async function fetchFiringAlerts(): Promise<{
  alerts: FiringAlert[]
  /** Rules are being evaluated. */
  enabled: boolean
  /** Transitions are posted somewhere as well as recorded. */
  delivering: boolean
}> {
  const r = await fetch('/api/v1/alerts', { cache: 'no-store' })
  if (!r.ok) throw new Error(`alerts: HTTP ${r.status}`)
  return (await r.json()) as {
    alerts: FiringAlert[]
    enabled: boolean
    delivering: boolean
  }
}

/** One transition: a rule started firing, or stopped. */
export interface AlertEvent {
  id: number
  ts: number
  firing: boolean
  rule: string
  describes: string
  value: number
  target: string
  agent_id: number
}

export async function fetchAlertEvents(limit = 50): Promise<AlertEvent[]> {
  const r = await fetch(`/api/v1/alert-events?limit=${limit}`, { cache: 'no-store' })
  // Absent on an instance whose store cannot keep history; an empty list reads
  // the same as "nothing has happened yet", which is the honest fallback.
  if (r.status === 404) return []
  if (!r.ok) throw new Error(`alert events: HTTP ${r.status}`)
  return ((await r.json()) as { events: AlertEvent[] }).events ?? []
}

export function createAlertRule(body: Partial<AlertRule>): Promise<unknown> {
  return mutate('/api/v1/alert-rules', 'POST', body)
}

export function updateAlertRule(id: number, body: Partial<AlertRule>): Promise<unknown> {
  return mutate(`/api/v1/alert-rules/${id}`, 'PATCH', body)
}

export function deleteAlertRule(id: number): Promise<unknown> {
  return mutate(`/api/v1/alert-rules/${id}`, 'DELETE')
}

/**
 * Where an effective setting came from: this node, the ancestor that set it,
 * or — for a caller scoped to a subtree — an ancestor above their scope, whose
 * path they may not know (DESIGN.md §7.4). The value is still reported; only
 * the path it came from is withheld.
 */
export type Provenance = 'local' | 'outside' | { id: number; name: string; path: string }

/**
 * True when no node in the tree sets this value at all.
 *
 * Only the optional per-probe-type settings can be in this state — the root is
 * required to set every other inheritable default — and it arrives as a zero
 * provenance with no id and no path. Rendering that as "inherited from" would
 * name an ancestor that does not exist, so the two cases are told apart here
 * rather than in each screen that shows a setting.
 */
export function isUnset(v: SettingValue<unknown>): boolean {
  return v.local === null && typeof v.source === 'object' && v.source.id === 0
}

/**
 * One resolved setting (DESIGN.md §4.2). `local` is null when the value is
 * inherited, which is exactly what the override and revert controls act on —
 * the server never flattens this away.
 */
export interface SettingValue<T> {
  local: T | null
  effective: T
  source: Provenance
}

export interface TargetSettings {
  interval_s: SettingValue<number>
  trace_interval_s: SettingValue<number>
  /** How long raw measurements are kept, in seconds; 0 keeps them forever. */
  retention_s: SettingValue<number>
  pings_per_interval: SettingValue<number>
  probe_mode: SettingValue<string>
  burst_gap_ms: SettingValue<number>
  timeout_ms: SettingValue<number>
  packet_size: SettingValue<number>
  dscp: SettingValue<number>
  agents: SettingValue<string>
  /** What the N probes of an interval are: icmp, dns, tcp, http, https, irtt. */
  probe_type: SettingValue<string>
  /** 0 means "the default for this probe type"; tcp has none and requires one. */
  probe_port: SettingValue<number>
  dns_query: SettingValue<string>
  dns_rr_type: SettingValue<string>
  http_path: SettingValue<string>
  /** Off by default; trusting the issuing CA is the better answer. */
  tls_skip_verify: SettingValue<boolean>
  /**
   * Which extra per-packet distributions are drawn: a space-separated list of
   * series names, "all" for every one that has data, or "" for none. Display
   * only — everything measured is kept whatever this says, so switching a
   * graph on shows its history rather than starting it from that moment.
   */
  graph_series: SettingValue<string>
}

export type SettingKey = keyof TargetSettings

export interface Target {
  id: number
  parent_id: number | null
  name: string
  path: string
  host: string | null
  address_family: string | null
  title: string | null
  notes: string | null
  hidden: boolean
  enabled: boolean
  sort_order: number
  is_group: boolean
  settings: TargetSettings
}

export async function fetchTargets(): Promise<Target[]> {
  const r = await fetch('/api/v1/targets', { cache: 'no-store' })
  if (!r.ok) throw new Error(`targets: HTTP ${r.status}`)
  const body = (await r.json()) as { targets: Target[] }
  return body.targets
}

/** Surfaces the server's validation message, which names the offending field. */
async function mutate(url: string, method: string, body?: unknown): Promise<unknown> {
  const r = await fetch(url, {
    method,
    headers: body === undefined ? {} : { 'Content-Type': 'application/json' },
    body: body === undefined ? undefined : JSON.stringify(body),
  })
  const text = await r.text()
  const parsed: unknown = text ? JSON.parse(text) : null
  if (!r.ok) {
    const msg = (parsed as { error?: string } | null)?.error
    throw new Error(msg ?? `HTTP ${r.status}`)
  }
  return parsed
}

export interface TargetPatch {
  parent_id?: number
  name?: string
  host?: string | null
  address_family?: string | null
  title?: string | null
  notes?: string | null
  hidden?: boolean
  enabled?: boolean
  sort_order?: number
  // A setting set to null is cleared, reverting the node to inheritance.
  settings?: Partial<Record<SettingKey, number | string | boolean | null>>
}

export function createTarget(body: TargetPatch): Promise<unknown> {
  return mutate('/api/v1/targets', 'POST', body)
}

export function updateTarget(id: number, body: TargetPatch): Promise<unknown> {
  return mutate(`/api/v1/targets/${id}`, 'PATCH', body)
}

export function deleteTarget(id: number, recursive: boolean): Promise<unknown> {
  return mutate(`/api/v1/targets/${id}${recursive ? '?recursive=true' : ''}`, 'DELETE')
}

/**
 * A grant gives an OIDC group a role on `target_id` and everything beneath
 * it — isolation is total, so a group scoped to a subtree cannot learn that
 * anything outside it exists. `path` is the target's full path, for display.
 * The global `admin` role is separate: it comes from the identity provider
 * and is never granted here.
 */
export interface Grant {
  id: number
  group: string
  target_id: number
  role: 'viewer' | 'editor'
  path: string
}

export async function fetchGrants(): Promise<Grant[]> {
  const r = await fetch('/api/v1/grants', { cache: 'no-store' })
  if (!r.ok) throw new Error(`grants: HTTP ${r.status}`)
  return ((await r.json()) as { grants: Grant[] }).grants ?? []
}

/** Posting the same (group, target_id) again re-roles the existing grant rather than duplicating it. */
export function createGrant(body: {
  group: string
  target_id: number
  role: 'viewer' | 'editor'
}): Promise<unknown> {
  return mutate('/api/v1/grants', 'POST', body)
}

export function deleteGrant(id: number): Promise<unknown> {
  return mutate(`/api/v1/grants/${id}`, 'DELETE')
}

/**
 * One series, columnar and worker-transferable: parallel arrays plus a
 * flattened samples buffer with row offsets (offsets[i]..offsets[i+1]).
 * RTTs are microseconds, sorted ascending within each row.
 */
export interface Series {
  ts: Float64Array // unix seconds, interval start
  sent: Float64Array
  received: Float64Array
  flags: Uint8Array
  offsets: Uint32Array // length rows+1
  values: Uint32Array // all samples, concatenated
  /** ICMP type<<8|code per row, or null where nothing was refused. */
  icmpErrors: (number | null)[]
  /**
   * The extra per-packet distributions measured beside the round trip, keyed
   * by series name. Absent from the map when no interval in the window carried
   * one, which is how "this target does not measure it" reads.
   */
  extra: Record<string, ExtraSeries>
}

/**
 * One extra per-packet distribution, in the same columnar shape as the round
 * trip. Values are signed: inter-packet delay variation is negative exactly
 * when a packet arrived sooner than the one before it.
 */
export interface ExtraSeries {
  offsets: Uint32Array // length rows+1
  values: Int32Array // all samples, concatenated, µs
  /**
   * 1 where this interval measured the series, 0 where it did not. Without it
   * a row the peer returned no timestamps for would be indistinguishable from
   * one where every packet arrived on schedule — both hold zero samples, and
   * only one of them is a measurement.
   */
  measured: Uint8Array
}

/** Series names the API can return, in the order the UI offers them. */
export const SERIES_NAMES = ['ipdv_send', 'ipdv_receive', 'server_processing'] as const
export type SeriesName = (typeof SERIES_NAMES)[number]

/** What each extra series is, in the words the graph uses. */
export const SERIES_LABELS: Record<string, { title: string; help: string }> = {
  ipdv_send: {
    title: 'Jitter towards the target',
    help:
      'How much later or earlier each packet reached the far end than the one before it. ' +
      'Measured as a difference between consecutive packets, so the two clocks need not be ' +
      'synchronised for it to mean something. Negative means a packet caught up with its predecessor.',
  },
  ipdv_receive: {
    title: 'Jitter on the way back',
    help:
      'The same measure in the other direction. The two are separate because the paths are: ' +
      'a round trip cannot say which half of it got worse.',
  },
  server_processing: {
    title: 'Time held by the far end',
    help:
      'How long the target held each packet between receiving it and replying. ' +
      'Separates a slow peer from a slow path.',
  },
}

/**
 * Names for the ICMP errors worth telling apart. Anything else is shown as
 * its raw type and code rather than guessed at.
 */
const ICMP_NAMES: Record<number, string> = {
  0x0300: 'network unreachable',
  0x0301: 'host unreachable',
  0x0302: 'protocol unreachable',
  0x0303: 'port unreachable',
  0x0304: 'fragmentation needed',
  0x0309: 'network prohibited',
  0x030a: 'host prohibited',
  0x030d: 'administratively filtered',
  0x0500: 'redirect',
  0x0b00: 'TTL exceeded',
  0x0b01: 'fragment reassembly timeout',
}

export function icmpErrorName(packed: number): string {
  return ICMP_NAMES[packed] ?? `ICMP type ${packed >> 8} code ${packed & 0xff}`
}

/** One recorded route, and when it took effect. */
export interface PathChange {
  ts: number
  hops: string[]
}

export async function fetchPathChanges(
  targetId: number,
  agentId: number,
  from: number,
  to: number,
): Promise<PathChange[]> {
  const r = await fetch(
    `/api/v1/paths?target_id=${targetId}&agent_id=${agentId}&from=${from}&to=${to}`,
    { cache: 'no-store' },
  )
  if (!r.ok) throw new Error(`paths: HTTP ${r.status}`)
  return ((await r.json()) as { changes: PathChange[] }).changes ?? []
}

export interface AgentInfo {
  /** What the agent said it was running when it last reported. */
  version?: string
  id: number
  name: string
  enabled: boolean
  is_local: boolean
  last_seen?: number
  pubkey?: string
}

export async function fetchAgents(): Promise<AgentInfo[]> {
  const r = await fetch('/api/v1/agents', { cache: 'no-store' })
  if (!r.ok) throw new Error(`agents: HTTP ${r.status}`)
  return ((await r.json()) as { agents: AgentInfo[] }).agents ?? []
}

/** The local prober (id 0) refuses both fields — the UI should not offer them for it. */
export function updateAgent(id: number, body: { name?: string; enabled?: boolean }): Promise<unknown> {
  return mutate(`/api/v1/agents/${id}`, 'PATCH', body)
}

/** Refused, with an explanatory error, if the agent has ever reported a measurement. */
export function deleteAgent(id: number): Promise<unknown> {
  return mutate(`/api/v1/agents/${id}`, 'DELETE')
}

export interface AgentToken {
  id: number
  name: string
  created_at: number
  expires_at: number
  used: boolean
  expired: boolean
  used_at?: number
  agent_id?: number
}

export async function fetchAgentTokens(): Promise<AgentToken[]> {
  const r = await fetch('/api/v1/agent-tokens', { cache: 'no-store' })
  if (!r.ok) throw new Error(`agent tokens: HTTP ${r.status}`)
  return ((await r.json()) as { tokens: AgentToken[] }).tokens ?? []
}

/** The plaintext `token` is returned only in this response — it is never shown again. */
export interface NewAgentToken {
  id: number
  name: string
  token: string
  created_at: number
  expires_at: number
}

export function createAgentToken(name: string, ttlS?: number): Promise<NewAgentToken> {
  return mutate('/api/v1/agent-tokens', 'POST', ttlS ? { name, ttl_s: ttlS } : { name }) as Promise<NewAgentToken>
}

/** Only an unspent token can be revoked. */
export function deleteAgentToken(id: number): Promise<unknown> {
  return mutate(`/api/v1/agent-tokens/${id}`, 'DELETE')
}

/**
 * Requests for the same window that are already in flight, so the detail page
 * asks once instead of once per plot.
 *
 * A target drawing the round trip and three extra series mounts four
 * components, each of which fetched and decoded the whole Arrow payload —
 * four identical requests, four full decodes, for one window. Only *concurrent*
 * requests are shared: nothing is cached after it settles, so a refresh or a
 * range change still goes to the server and there is no staleness to reason
 * about.
 *
 * Each caller gets its own copy of the buffers. They are transferred to a
 * render worker, which neuters them, so handing the same arrays to four
 * consumers would leave three of them holding empty views.
 */
const inFlight = new Map<string, { at: number; p: Promise<Series> }>()

/**
 * How long a settled response stays shareable.
 *
 * Sharing only *concurrent* requests was not enough: the four plots mount a few
 * milliseconds apart, so the first often settles before the last asks, and the
 * page still made two round trips. The key carries the exact time range, and
 * the shortest interval a target can be configured with is ten seconds, so a
 * window this brief cannot serve anything the caller would not have received
 * from its own request.
 */
const SHARE_MS = 2000

function copySeries(s: Series): Series {
  const extra: Record<string, ExtraSeries> = {}
  for (const [k, v] of Object.entries(s.extra)) {
    extra[k] = {
      offsets: v.offsets.slice(),
      values: v.values.slice(),
      measured: v.measured.slice(),
    }
  }
  return {
    ts: s.ts.slice(),
    sent: s.sent.slice(),
    received: s.received.slice(),
    flags: s.flags.slice(),
    offsets: s.offsets.slice(),
    values: s.values.slice(),
    icmpErrors: s.icmpErrors.slice(),
    extra,
  }
}

export function fetchSeries(
  targetId: number,
  agentId: number,
  from: number,
  to: number,
): Promise<Series> {
  const key = `${targetId}/${agentId}/${from}/${to}`
  const now = Date.now()
  const hit = inFlight.get(key)
  if (hit && now - hit.at < SHARE_MS) return hit.p.then(copySeries)
  const p = fetchSeriesUncached(targetId, agentId, from, to).catch((e: unknown) => {
    // A failure is never shared: the next caller must be able to try again.
    // Guarded like the timeout below, and for the same reason — a slow request
    // that has already been evicted must not delete the entry that replaced
    // it, which would cost the next caller its coalescing.
    if (inFlight.get(key)?.p === p) inFlight.delete(key)
    throw e
  })
  inFlight.set(key, { at: now, p })
  // Bounded: entries are dropped once they can no longer be shared, so a long
  // session scrubbing through ranges does not accumulate them.
  setTimeout(() => {
    if (inFlight.get(key)?.p === p) inFlight.delete(key)
  }, SHARE_MS)
  return p.then(copySeries)
}

async function fetchSeriesUncached(
  targetId: number,
  agentId: number,
  from: number,
  to: number,
): Promise<Series> {
  const r = await fetch(
    `/api/v1/measurements?target_id=${targetId}&agent_id=${agentId}&from=${from}&to=${to}`,
    { cache: 'no-store' },
  )
  if (!r.ok) throw new Error(`measurements: HTTP ${r.status}`)
  const table = tableFromIPC(new Uint8Array(await r.arrayBuffer()))
  const n = table.numRows
  const tsCol = table.getChild('ts')!
  const sentCol = table.getChild('sent')!
  const recvCol = table.getChild('received')!
  const flagsCol = table.getChild('flags')!
  const samplesCol = table.getChild('samples')!
  const icmpCol = table.getChild('icmp_error')

  const ts = new Float64Array(n)
  const sent = new Float64Array(n)
  const received = new Float64Array(n)
  const flags = new Uint8Array(n)
  const offsets = new Uint32Array(n + 1)
  const rows: Uint32Array[] = new Array(n)
  const icmpErrors: (number | null)[] = new Array(n)
  let total = 0
  for (let i = 0; i < n; i++) {
    // Flechette decodes timestamps as epoch milliseconds; normalize to seconds.
    const t = Number(tsCol.at(i))
    ts[i] = t > 1e11 ? t / 1000 : t
    sent[i] = Number(sentCol.at(i))
    received[i] = Number(recvCol.at(i))
    flags[i] = Number(flagsCol.at(i))
    const icmp = icmpCol?.at(i)
    icmpErrors[i] = icmp === null || icmp === undefined ? null : Number(icmp)
    const row = samplesCol.at(i) as ArrayLike<number> | null
    const arr = row instanceof Uint32Array ? row : Uint32Array.from(row ?? [])
    rows[i] = arr
    total += arr.length
    offsets[i + 1] = total
  }
  const values = new Uint32Array(total)
  for (let i = 0; i < n; i++) values.set(rows[i], offsets[i])

  // The extra series are optional columns: a master older than them, or a
  // probe that does not produce them, simply does not send them. A column that
  // is present but null on every row is dropped too — it means this target
  // measures nothing of the sort, and an empty graph would invite the reader
  // to conclude the jitter was zero.
  const extra: Record<string, ExtraSeries> = {}
  for (const name of SERIES_NAMES) {
    const col = table.getChild(name)
    if (!col) continue
    const eOffsets = new Uint32Array(n + 1)
    const measured = new Uint8Array(n)
    const eRows: (ArrayLike<number> | null)[] = new Array(n)
    let eTotal = 0
    let any = false
    for (let i = 0; i < n; i++) {
      const row = col.at(i) as ArrayLike<number> | null
      if (row === null || row === undefined) {
        eRows[i] = null
      } else {
        eRows[i] = row
        measured[i] = 1
        eTotal += row.length
        any = true
      }
      eOffsets[i + 1] = eTotal
    }
    if (!any) continue
    const eValues = new Int32Array(eTotal)
    for (let i = 0; i < n; i++) {
      const row = eRows[i]
      if (row) eValues.set(row instanceof Int32Array ? row : Int32Array.from(row), eOffsets[i])
    }
    extra[name] = { offsets: eOffsets, values: eValues, measured }
  }
  return { ts, sent, received, flags, offsets, values, icmpErrors, extra }
}

/** The whole target tree as TOML — the same text `config export` writes. */
export async function exportConfig(): Promise<string> {
  const r = await fetch('/api/v1/config', { cache: 'no-store' })
  if (!r.ok) throw new Error(`config export: HTTP ${r.status}`)
  return r.text()
}

export interface ImportResult {
  summary: string
  warnings?: string[]
}

/**
 * Apply a TOML file declaratively. Never prunes: absence disables here, which
 * is recoverable by importing again. Pruning deletes, and stays on the command
 * line.
 */
export async function importConfig(toml: string, allowUnknownAgents = false): Promise<ImportResult> {
  const r = await fetch(`/api/v1/config${allowUnknownAgents ? '?allow_unknown_agents=1' : ''}`, {
    method: 'PUT',
    headers: { 'Content-Type': 'application/toml' },
    body: toml,
    cache: 'no-store',
  })
  const body = (await r.json().catch(() => ({}))) as { error?: string } & ImportResult
  if (!r.ok) throw new Error(body.error ?? `config import: HTTP ${r.status}`)
  return body
}
