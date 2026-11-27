// The rows of the CSV each plot downloads: what the plot draws, with the figures
// its readout gives, and nothing it does not know. A missing figure is NaN, which
// csvCell writes as an empty cell; a gap is a row that is not there.
import { icmpErrorName, probeErrorName, sendReasonName } from './api.ts'

export type Row = (string | number)[]

/** Unix seconds as ISO 8601 in UTC. */
export function isoUTC(s: number): string {
  return new Date(s * 1000).toISOString().replace(/\.000Z$/, 'Z')
}

const round6 = (x: number) => Math.round(x * 1e6) / 1e6

/** `<parts>-<from>-<to>.csv`, the window in UTC. A target's path loses its leading slash. */
export function csvFileName(parts: (string | undefined)[], from: number, to: number): string {
  const stamp = (s: number) => isoUTC(s).replace(/[-:]/g, '')
  const safe = (p: string) => p.replace(/[^\w-]+/g, '_').replace(/^_+|_+$/g, '')
  return `${parts.filter(Boolean).map((p) => safe(p!)).join('-')}-${stamp(from)}-${stamp(to)}.csv`
}

export interface RoundTripData {
  ts: ArrayLike<number>
  sent: ArrayLike<number>
  received: ArrayLike<number>
  /** Fraction lost, NaN where nothing was sent. */
  loss: ArrayLike<number>
  /** Median round trip in µs, NaN where no probe was answered. */
  median: ArrayLike<number>
  flags: ArrayLike<number>
  icmpErrors: (number | null)[]
  sendErrors: (number | null)[]
  probeErrors: (number | null)[]
  lossPositions: (number[] | null)[]
  /** A pooled read: each row is a pixel column [ts, ends) of `rounds` intervals. */
  pooled?: { ends: ArrayLike<number>; rounds: ArrayLike<number>; positional: ArrayLike<number> }
}

/**
 * The round-trip plot. A row is one interval, or one pixel column of a pooled
 * read; the first column's name says which. `flags` names every quality flag the
 * interval carried (in a pooled column: that any of its intervals carried), as
 * the badges beside the plot do. `intervalS` is the target's interval now, with
 * which a pooled column's coverage is estimated as the readout does (0: unknown).
 * A pooled read carries no per-interval details, so its file has no `icmp_error`,
 * `send_error` or `probe_error` column: an empty cell there would read as "none".
 * `probe_error` is why the interval's failed probes got no successful answer.
 */
export function roundTripRows(d: RoundTripData, flagDefs: { bit: number; label: string }[], intervalS: number): Row[] {
  const p = d.pooled
  const rows: Row[] = [
    [
      ...(p ? ['column_start_utc', 'column_end_utc', 'intervals', 'covered_percent_estimate'] : ['interval_start_utc']),
      'median_us',
      'sent',
      'received',
      'loss_percent',
      ...(p ? [] : ['icmp_error', 'send_error', 'probe_error']),
      'flags',
    ],
  ]
  for (let i = 0; i < d.ts.length; i++) {
    const labels = flagDefs.filter((f) => d.flags[i] & f.bit).map((f) => f.label)
    if (p ? p.positional[i] > 0 : d.lossPositions[i] !== null) labels.push('positional loss')
    const err = d.icmpErrors[i]
    const se = d.sendErrors[i]
    const pe = d.probeErrors[i]
    const span = p ? p.ends[i] - d.ts[i] : 0
    rows.push([
      isoUTC(d.ts[i]),
      ...(p
        ? [
            isoUTC(p.ends[i]),
            p.rounds[i],
            intervalS > 0 && span > 0 ? Math.round(Math.min(1, (p.rounds[i] * intervalS) / span) * 100) : NaN,
          ]
        : []),
      d.median[i],
      d.sent[i],
      d.received[i],
      round6(d.loss[i] * 100),
      ...(p
        ? []
        : [
            err === null || err === undefined ? NaN : icmpErrorName(err),
            se === null || se === undefined ? NaN : sendReasonName(se),
            pe === null || pe === undefined ? NaN : probeErrorName(pe),
          ]),
      labels.join('; '),
    ])
  }
  return rows
}

export interface ExtraSeriesData {
  ts: ArrayLike<number>
  /** Median in µs, NaN where there is none. */
  median: ArrayLike<number>
  /** 1 where the interval measured this series at all. */
  measured: ArrayLike<number>
  ends?: ArrayLike<number>
}

/**
 * One of the extra per-packet series. An empty median_us is either "nothing to
 * compare" (measured, but no pair of replies to difference) or "not measured";
 * `status` says which.
 */
export function extraSeriesRows(d: ExtraSeriesData): Row[] {
  const rows: Row[] = [
    [...(d.ends ? ['column_start_utc', 'column_end_utc'] : ['interval_start_utc']), 'median_us', 'status'],
  ]
  for (let i = 0; i < d.ts.length; i++) {
    const v = d.median[i]
    rows.push([
      isoUTC(d.ts[i]),
      ...(d.ends ? [isoUTC(d.ends[i])] : []),
      v,
      !Number.isNaN(v) ? 'measured' : d.measured[i] === 1 ? 'nothing to compare' : 'not measured',
    ])
  }
  return rows
}

/**
 * The comparison: one row per start time that any series has a row at; per
 * series a median and a status. An empty median is one of three facts, which
 * `status` tells apart: "no row" (the series has no row at that time), "no
 * replies" (probes were sent and none came back) and "nothing sent". Nothing is
 * filled in between.
 */
export function compareRows(
  lines: { label: string; ts: ArrayLike<number>; med: (number | null)[]; sent: ArrayLike<number> }[],
): Row[] {
  const at = lines.map((l) => new Map(Array.from(l.ts, (t, i) => [t, i] as const)))
  const times = [...new Set(lines.flatMap((l) => Array.from(l.ts)))].sort((a, b) => a - b)
  return [
    ['start_utc', ...lines.flatMap((l) => [`${l.label} median_us`, `${l.label} status`])],
    ...times.map((t) => [
      isoUTC(t),
      ...lines.flatMap((l, k) => {
        const i = at[k].get(t)
        if (i === undefined) return [NaN, 'no row']
        const v = l.med[i]
        if (v !== null) return [v, 'measured']
        return [NaN, l.sent[i] > 0 ? 'no replies' : 'nothing sent']
      }),
    ]),
  ]
}
