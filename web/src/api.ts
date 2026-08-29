import { tableFromIPC } from '@uwdata/flechette'

export interface Me {
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

/** Where an effective setting came from: this node, or the ancestor that set it. */
export type Provenance = 'local' | { id: number; name: string; path: string }

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

export async function fetchSeries(targetId: number, from: number, to: number): Promise<Series> {
  const r = await fetch(`/api/v1/measurements?target_id=${targetId}&from=${from}&to=${to}`, {
    cache: 'no-store',
  })
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
