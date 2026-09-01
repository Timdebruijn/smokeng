import { useEffect, useMemo, useState } from 'react'
import Plot from './Plot'
import SeriesPlot from './SeriesPlot'
import {
  SERIES_NAMES,
  fetchAlertRules,
  fetchAvailability,
  fetchSeries,
  isUnset,
  type AgentInfo,
  type SeriesName,
  type AlertRule,
  type AvailabilityResponse,
  type SettingValue,
  type Target,
} from './api'
import { fmtLoss, fmtUs } from './format'

const RANGES: { label: string; seconds: number }[] = [
  { label: '15m', seconds: 900 },
  { label: '1h', seconds: 3600 },
  { label: '6h', seconds: 21600 },
  { label: '24h', seconds: 86400 },
]

// `types` limits a row to the probe types it means something for, the same way
// the admin form does. A packet size on an http target is not a setting with
// an unusual value; it is a setting that does nothing, and listing it under
// "effective settings" would claim otherwise.
const SETTING_LABELS: {
  key: keyof Target['settings']
  label: string
  unit?: string
  types?: string[]
  // When present, renders the effective value itself (unit included); used where
  // a bare number would mislead, e.g. retention 0 meaning "forever", not "0s".
  //
  // Typed to the union a setting's effective value can actually hold. It was
  // (v: number) => string with an `as number` at the call site, which made the
  // compiler agree with a claim that was false the moment a string-valued
  // setting got a renderer — the assertion silenced exactly the check that
  // would have caught it.
  render?: (v: number | string | boolean) => string
}[] = [
  { key: 'probe_type', label: 'Probe type' },
  { key: 'interval_s', label: 'Interval', unit: 's' },
  { key: 'pings_per_interval', label: 'Pings per interval' },
  { key: 'probe_mode', label: 'Probe mode' },
  { key: 'burst_gap_ms', label: 'Burst gap', unit: 'ms' },
  { key: 'timeout_ms', label: 'Timeout', unit: 'ms' },
  { key: 'packet_size', label: 'Packet size', unit: 'bytes', types: ['icmp', 'irtt'] },
  { key: 'probe_port', label: 'Port', types: ['dns', 'tcp', 'http', 'https', 'irtt'] },
  { key: 'dns_query', label: 'DNS query', types: ['dns'] },
  { key: 'dns_rr_type', label: 'DNS record type', types: ['dns'] },
  { key: 'http_path', label: 'HTTP path', types: ['http', 'https'] },
  { key: 'tls_skip_verify', label: 'Skip TLS verification', types: ['https'] },
  { key: 'dscp', label: 'DSCP' },
  { key: 'agents', label: 'Agents' },
  { key: 'trace_interval_s', label: 'Path discovery', unit: 's' },
  {
    key: 'retention_s',
    label: 'Retention',
    render: (v) => (v === 0 ? 'forever' : `${v} s`),
  },
  {
    key: 'graph_series',
    label: 'Extra graphs',
    render: (v) => {
      const raw = String(v).trim()
      if (raw === 'all') return 'every series that has data'
      if (raw === '') return 'none'
      return raw.split(/\s+/).join(', ')
    },
  },
]

/**
 * One target, at length: what it measured, under what settings, watched by
 * which rules, from where.
 *
 * The four figures at the top are computed from the same samples the plot
 * draws — the pooled distribution over the window on screen, not a summary the
 * server pre-chewed. p95 and the spread are there because they are the two
 * numbers a median cannot tell you, and the reason this project keeps whole
 * distributions in the first place.
 */
export default function Detail({
  target,
  agents,
  onBack,
  onEdit,
}: {
  target: Target
  agents: AgentInfo[]
  onBack: () => void
  onEdit: () => void
}) {
  const [rangeS, setRangeS] = useState(3600)
  const [refreshKey] = useState(0)
  const [stats, setStats] = useState<Stats | null>(null)
  const [rules, setRules] = useState<AlertRule[]>([])

  const probeType = String(target.settings.probe_type.effective || 'icmp')
  const agentNames = (target.settings.agents.effective || 'local').split(/\s+/).filter(Boolean)
  const byName = new Map(agents.map((a) => [a.name, a.id]))
  // Null rather than 0: falling back to the local prober would show one
  // vantage point's measurements under another's name.
  const agentId = byName.get(agentNames[0]) ?? null

  const [from, to] = useMemo(() => {
    const now = Math.floor(Date.now() / 1000)
    return [now - rangeS, now]
  }, [rangeS, refreshKey])

  useEffect(() => {
    if (agentId === null) {
      setStats(null)
      return
    }
    let cancelled = false
    fetchSeries(target.id, agentId, from, to)
      .then((s) => !cancelled && setStats(summarise(s)))
      .catch(() => !cancelled && setStats(null))
    return () => {
      cancelled = true
    }
  }, [target.id, agentId, from, to])

  useEffect(() => {
    fetchAlertRules()
      .then(setRules)
      .catch(() => setRules([]))
  }, [])

  // A rule on an ancestor covers this target too, which is the whole point of
  // hanging them on the tree — so show those as well, and say where from.
  const covering = rules.filter((r) => r.target_id === target.id)

  return (
    <section className="detail">
      <button className="pill" onClick={onBack}>
        ← Back to graphs
      </button>

      <div className="detail-head">
        <span className="dot" style={{ background: stats?.dot ?? 'var(--dim)' }} />
        <h1>{target.title ?? target.path}</h1>
        {/* The probe type belongs on the identity line, not buried in the
            settings table: two targets with the same host and family are
            different measurements if one is icmp and the other https, and the
            graphs beneath them are not comparable. */}
        <code className="host">
          {target.host} · {target.address_family} · {probeType}
        </code>
        <span className="spacer" />
        <button className="pill" onClick={onEdit}>
          Edit in Targets
        </button>
      </div>

      {/* On the page where the graph is actually read, because the number
          beside it means less than it looks: something answered, not
          necessarily the service this target names. */}
      {probeType === 'https' && target.settings.tls_skip_verify.effective === true && (
        <p className="hint warn-line">
          Certificates are not verified for this target.
        </p>
      )}

      <div className="stat-grid">
        <Stat label="Median" value={stats?.median != null ? fmtUs(stats.median) : '—'} />
        <Stat label="p95" value={stats?.p95 != null ? fmtUs(stats.p95) : '—'} />
        <Stat
          label="Spread (p95−p5)"
          value={stats?.spread != null ? fmtUs(stats.spread) : '—'}
          title="The width of the distribution. A link whose median is unchanged but whose spread has tripled is degrading."
        />
        <Stat
          label="Loss"
          value={stats ? fmtLoss(stats.loss) : '—'}
          tone={stats && stats.loss > 0 ? 'bad' : undefined}
        />
      </div>

      <div className="card detail-plot">
        <div className="detail-plot-head">
          <span className="hint small">
            {/* Not "measured" when nobody is measuring it: the notice below
                says the assigned agent is not enrolled, and a header claiming
                a measurement over it would contradict it. */}
            {agentId === null
              ? 'not measured'
              : `${agentNames.length > 1 ? `from ${agentNames[0]}` : 'measured'} · pooled over the window below`}
          </span>
          <span className="spacer" />
          {RANGES.map((r) => (
            <button
              key={r.label}
              className={r.seconds === rangeS ? 'pill active' : 'pill'}
              onClick={() => setRangeS(r.seconds)}
            >
              {r.label}
            </button>
          ))}
        </div>
        {agentId === null ? (
          <p className="hint">
            Assigned to <code>{agentNames.join(' ')}</code>, which is not enrolled — so nothing is
            measuring this target.
          </p>
        ) : (
          <Plot
            target={target}
            agentId={agentId}
            from={from}
            to={to}
            refreshKey={refreshKey}
            logScale
            onZoom={() => undefined}
          />
        )}
        {agentId !== null &&
          selectedSeries(target).map((name) => (
            <SeriesPlot
              key={name}
              targetId={target.id}
              targetPath={target.path}
              agentId={agentId}
              name={name}
              from={from}
              to={to}
              refreshKey={refreshKey}
            />
          ))}
      </div>

      <AvailabilityPanel target={target} />

      <div className="detail-columns">
        <div className="card panel">
          <h2>Effective settings</h2>
          {SETTING_LABELS.filter(
            (s) => !s.types || s.types.includes(probeType),
          ).map((s) => {
            const v = target.settings[s.key] as SettingValue<number | string | boolean>
            return (
              <div key={s.key} className="setting-line">
                <span className="setting-label">{s.label}</span>
                <span className="mono">
                  {/* An unset optional setting has no value to print; an empty
                      cell would read as a blank the page failed to fill. */}
                  {isUnset(v)
                    ? '—'
                    : s.render
                      ? s.render(v.effective)
                      : String(v.effective)}
                  {!isUnset(v) && s.unit && !s.render && <span className="unit">{s.unit}</span>}
                </span>
                <span className="spacer" />
                <span className={v.source === 'local' ? 'chip local' : 'chip'}>
                  {isUnset(v)
                    ? 'not set anywhere'
                    : v.source === 'local'
                      ? 'set here'
                      : v.source === 'outside'
                        ? 'from outside your scope'
                        : `inherited from ${v.source.path === '/' ? '/' : v.source.path}`}
                </span>
              </div>
            )
          })}
        </div>

        <div className="detail-side">
          <div className="card panel">
            <h2>Alert rules on this target</h2>
            {covering.length === 0 ? (
              <p className="hint small">
                None set here. Rules on an ancestor still cover this target — see{' '}
                <strong>Alerts</strong>.
              </p>
            ) : (
              covering.map((r) => (
                <div key={r.id} className="rule-line">
                  <span
                    className="dot"
                    style={{ background: r.enabled ? 'var(--good)' : 'var(--dim)' }}
                  />
                  <span className="rule-name">{r.name}</span>
                  {/* The server's own phrasing, not a unit guessed here: a
                      bimodality coefficient has no unit and a shape rule in auto
                      mode is a z-score, both of which read as nonsense with "ms"
                      stuck on the end. */}
                  <span className="hint small">{r.describes}</span>
                </div>
              ))
            )}
          </div>

          <div className="card panel">
            <h2>Measured by</h2>
            <div className="pill-row">
              {agentNames.map((n) => (
                <span key={n} className="agent">
                  {n}
                </span>
              ))}
            </div>
            <p className="hint small">
              Each vantage point is its own series. Two that disagree is the finding, not something
              to average away.
            </p>
          </div>
        </div>
      </div>
    </section>
  )
}

const AVAIL_PERIODS: { label: string; seconds: number }[] = [
  { label: '24h', seconds: 86400 },
  { label: '7d', seconds: 7 * 86400 },
  { label: '30d', seconds: 30 * 86400 },
]

// A duration in seconds as the coarse "2d 3h 12m" an operator reads at a glance.
function fmtDur(s: number): string {
  if (s <= 0) return '0'
  const d = Math.floor(s / 86400)
  const h = Math.floor((s % 86400) / 3600)
  const m = Math.floor((s % 3600) / 60)
  const parts: string[] = []
  if (d) parts.push(`${d}d`)
  if (h) parts.push(`${h}h`)
  if (m && !d) parts.push(`${m}m`)
  if (parts.length === 0) parts.push(`${s}s`)
  return parts.join(' ')
}

function pct(x: number): string {
  return `${(x * 100).toFixed(x >= 0.9995 ? 3 : 2)}%`
}

function downloadCSV(filename: string, rows: string[][]) {
  const text = rows.map((r) => r.map((c) => `"${c.replace(/"/g, '""')}"`).join(',')).join('\n')
  const url = URL.createObjectURL(new Blob([text], { type: 'text/csv' }))
  const a = document.createElement('a')
  a.href = url
  a.download = filename
  a.click()
  URL.revokeObjectURL(url)
}

/**
 * Availability over a period, per vantage point. It reports availability and
 * coverage as two numbers on purpose: uptime is computed only over intervals
 * there is data for, and coverage says how much of the period that was, so a
 * figure taken over a half-measured window cannot pass for one taken over a full
 * one. A gap is unknown time — never counted as up, and never as down.
 */
/**
 * Which extra distributions this target draws, from its resolved graph_series.
 *
 * "all" is the default and means every series that has data — which is decided
 * by the data, not here: a series nothing measured is dropped by the fetch, and
 * SeriesPlot says so rather than drawing an empty graph.
 *
 * An unrecognised name is dropped rather than treated as "all". The setting is
 * validated at every write path, so one reaching this component means the value
 * came from somewhere that skipped validation, and inventing panels the
 * operator did not ask for is not a safer answer than showing fewer.
 */
function selectedSeries(target: Target): SeriesName[] {
  // Only irtt measures any of these. Offering them on an ICMP target would
  // stack three panels saying "not measured" under every graph in the tree,
  // which reads as a fault rather than as a probe type that has no such
  // measure — and buries the one case where "not measured" is worth reading:
  // an irtt peer that returns no timestamps.
  if (String(target.settings.probe_type?.effective ?? 'icmp') !== 'irtt') return []
  const raw = (target.settings.graph_series?.effective ?? 'all').trim()
  if (raw === 'all') return [...SERIES_NAMES]
  if (raw === '') return []
  const want = new Set(raw.split(/\s+/))
  return SERIES_NAMES.filter((n) => want.has(n))
}

function AvailabilityPanel({ target }: { target: Target }) {
  const [periodS, setPeriodS] = useState(7 * 86400)
  const [strict, setStrict] = useState(true) // strict = down only at 100% loss
  const [data, setData] = useState<AvailabilityResponse | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [loading, setLoading] = useState(false)

  useEffect(() => {
    let cancelled = false
    setLoading(true)
    const to = Math.floor(Date.now() / 1000)
    const from = to - periodS
    fetchAvailability(target.id, from, to, strict ? 100 : 50)
      .then((d) => {
        if (cancelled) return
        setData(d)
        setError(null)
      })
      .catch((e: Error) => !cancelled && setError(e.message))
      .finally(() => !cancelled && setLoading(false))
    return () => {
      cancelled = true
    }
  }, [target.id, periodS, strict])

  const exportCSV = () => {
    if (!data) return
    const rows: string[][] = [
      ['target', 'agent', 'availability', 'coverage', 'up_s', 'down_s', 'unknown_s', 'outages'],
    ]
    for (const a of data.agents) {
      const r = a.report
      rows.push([
        data.target,
        a.agent,
        String(r.availability),
        String(r.coverage),
        String(r.up_s),
        String(r.down_s),
        String(r.unknown_s),
        String(r.downtime.length),
      ])
    }
    const name = `availability-${data.target.replace(/[^\w-]+/g, '_')}.csv`
    downloadCSV(name, rows)
  }

  return (
    <section className="card section-card">
      <div className="section-card-head">
        <span className="section-card-title">Availability</span>
        <span className="spacer" />
        <span className="pill-row">
          {AVAIL_PERIODS.map((p) => (
            <button
              key={p.label}
              className={p.seconds === periodS ? 'pill active' : 'pill'}
              onClick={() => setPeriodS(p.seconds)}
            >
              {p.label}
            </button>
          ))}
          <button
            className="pill"
            title="What counts as down: a total blackout, or heavy loss too"
            onClick={() => setStrict((v) => !v)}
          >
            down: {strict ? '100% loss' : '>50% loss'}
          </button>
          <button className="pill" onClick={exportCSV} disabled={!data}>
            Download CSV
          </button>
        </span>
      </div>

      {error && <p className="error">{error}</p>}
      {loading && !data ? (
        <p className="hint">Loading…</p>
      ) : !data || data.agents.length === 0 ? (
        <p className="hint">No vantage point is measuring this target.</p>
      ) : (
        data.agents.map((a) => {
          const r = a.report
          return (
            <div key={a.agent_id} className="avail-agent">
              <div className="avail-head">
                <span className="agent">{a.agent}</span>
                {!r.has_data && <span className="hint small">no data in this period</span>}
              </div>
              {r.has_data && (
                <>
                  <div className="stat-grid">
                    <Stat
                      label="Availability"
                      value={pct(r.availability)}
                      title="Uptime over the intervals there is data for"
                      tone={r.availability < 0.999 ? 'bad' : undefined}
                    />
                    <Stat
                      label="Coverage"
                      value={pct(r.coverage)}
                      title="How much of the period was actually measured — the rest is unknown, not up"
                      tone={r.coverage < 0.9 ? 'bad' : undefined}
                    />
                    <Stat label="Downtime" value={fmtDur(r.down_s)} />
                    <Stat label="Unknown" value={fmtDur(r.unknown_s)} />
                  </div>
                  {r.downtime.length === 0 ? (
                    <p className="hint small">No outages in this period.</p>
                  ) : (
                    <details className="hint">
                      <summary>
                        {r.downtime.length} outage(s), {fmtDur(r.down_s)} total
                      </summary>
                      <table className="alerts">
                        <tbody>
                          {r.downtime.map((e, i) => (
                            <tr key={i}>
                              <td className="mono">{new Date(e.start_ts * 1000).toLocaleString()}</td>
                              <td className="dim mono">→ {new Date(e.end_ts * 1000).toLocaleString()}</td>
                              <td>{fmtDur(e.duration_s)}</td>
                            </tr>
                          ))}
                        </tbody>
                      </table>
                    </details>
                  )}
                </>
              )}
            </div>
          )
        })
      )}
    </section>
  )
}

function Stat({
  label,
  value,
  title,
  tone,
}: {
  label: string
  value: string
  title?: string
  tone?: 'bad'
}) {
  return (
    <div className="card stat" title={title}>
      <p className="stat-label">{label}</p>
      <p className={tone === 'bad' ? 'stat-value bad' : 'stat-value'}>{value}</p>
    </div>
  )
}

interface Stats {
  // null when there are no replies to measure a latency from — distinct from
  // 0µs, which is a real (if implausible) measurement.
  median: number | null
  p95: number | null
  spread: number | null
  loss: number
  dot: string
}

/** Pooled over the whole window, from the samples themselves. */
function summarise(s: { values: Uint32Array; sent: Float64Array; received: Float64Array }): Stats {
  const all = Array.from(s.values).sort((a, b) => a - b)
  // With no replies, `all` is empty and every quantile would otherwise read
  // as 0 — not "unknown", but a specific, wrong number that looks exactly
  // like a suspiciously fast link. Loss is still known in that case (indeed
  // it's the whole story), so it's computed unconditionally below; the
  // latency figures are reported as null instead of being invented.
  const q = (p: number) => all[Math.min(all.length - 1, Math.floor(p * all.length))]
  let sent = 0
  let received = 0
  for (let i = 0; i < s.sent.length; i++) {
    sent += s.sent[i]
    received += s.received[i]
  }
  const loss = sent > 0 ? 100 * (1 - received / sent) : 0
  return {
    median: all.length ? q(0.5) : null,
    p95: all.length ? q(0.95) : null,
    spread: all.length ? q(0.95) - q(0.05) : null,
    loss,
    dot: loss > 0 ? 'var(--warn)' : 'var(--good)',
  }
}

