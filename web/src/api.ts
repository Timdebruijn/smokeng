import { tableFromIPC } from '@uwdata/flechette'

export interface Target {
  id: number
  path: string
  host: string | null
  address_family: string | null
  title: string | null
  hidden: boolean
  enabled: boolean
}

export async function fetchTargets(): Promise<Target[]> {
  const r = await fetch('/api/v1/targets')
  if (!r.ok) throw new Error(`targets: HTTP ${r.status}`)
  const body = (await r.json()) as { targets: Target[] }
  return body.targets
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
}

export async function fetchSeries(targetId: number, from: number, to: number): Promise<Series> {
  const r = await fetch(`/api/v1/measurements?target_id=${targetId}&from=${from}&to=${to}`)
  if (!r.ok) throw new Error(`measurements: HTTP ${r.status}`)
  const table = tableFromIPC(new Uint8Array(await r.arrayBuffer()))
  const n = table.numRows
  const tsCol = table.getChild('ts')!
  const sentCol = table.getChild('sent')!
  const recvCol = table.getChild('received')!
  const flagsCol = table.getChild('flags')!
  const samplesCol = table.getChild('samples')!

  const ts = new Float64Array(n)
  const sent = new Float64Array(n)
  const received = new Float64Array(n)
  const flags = new Uint8Array(n)
  const offsets = new Uint32Array(n + 1)
  const rows: Uint32Array[] = new Array(n)
  let total = 0
  for (let i = 0; i < n; i++) {
    // Flechette decodes timestamps as epoch milliseconds; normalize to seconds.
    const t = Number(tsCol.at(i))
    ts[i] = t > 1e11 ? t / 1000 : t
    sent[i] = Number(sentCol.at(i))
    received[i] = Number(recvCol.at(i))
    flags[i] = Number(flagsCol.at(i))
    const row = samplesCol.at(i) as ArrayLike<number> | null
    const arr = row instanceof Uint32Array ? row : Uint32Array.from(row ?? [])
    rows[i] = arr
    total += arr.length
    offsets[i + 1] = total
  }
  const values = new Uint32Array(total)
  for (let i = 0; i < n; i++) values.set(rows[i], offsets[i])
  return { ts, sent, received, flags, offsets, values }
}
