import { tableFromIPC } from '@uwdata/flechette'

export interface Me {
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
  metric: 'loss' | 'median' | 'p95' | 'spread'
  op: '>' | '<'
  threshold: number
  for_intervals: number
  clear_intervals: number
  enabled: boolean
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
}

export async function fetchAlertRules(): Promise<AlertRule[]> {
  const r = await fetch('/api/v1/alert-rules', { cache: 'no-store' })
  if (!r.ok) throw new Error(`alert rules: HTTP ${r.status}`)
  return ((await r.json()) as { rules: AlertRule[] }).rules ?? []
}

export async function fetchFiringAlerts(): Promise<{ alerts: FiringAlert[]; enabled: boolean }> {
  const r = await fetch('/api/v1/alerts', { cache: 'no-store' })
  if (!r.ok) throw new Error(`alerts: HTTP ${r.status}`)
  return (await r.json()) as { alerts: FiringAlert[]; enabled: boolean }
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
  pings_per_interval: SettingValue<number>
  probe_mode: SettingValue<string>
  burst_gap_ms: SettingValue<number>
  timeout_ms: SettingValue<number>
  packet_size: SettingValue<number>
  dscp: SettingValue<number>
  agents: SettingValue<string>
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
  settings?: Partial<Record<SettingKey, number | string | null>>
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

export async function fetchSeries(
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
  return { ts, sent, received, flags, offsets, values, icmpErrors }
}
