import { useEffect, useState } from 'react'
import {
  fetchAgents,
  fetchFiringAlerts,
  fetchSeries,
  fetchTargets,
  type AgentInfo,
  type FiringAlert,
  type Target,
} from './api'

const WINDOW_S = 3600

interface Row {
  target: Target
  agentId: number
  agentName?: string
  median: number
  p95: number
  loss: number
  flags: number
  spark: number[]
}

/**
 * The state of everything, at a glance, over the last hour.
 *
 * Every figure here is computed from the samples themselves rather than from
 * anything the server summarised: the whole point of keeping distributions is
 * that a p95 remains available afterwards, so an overview that fell back to
 * averages would be arguing against the tool it belongs to.
 */
export default function Overview({ onOpenDetail }: { onOpenDetail: (id: number) => void }) {
  const [rows, setRows] = useState<Row[] | null>(null)
  const [firing, setFiring] = useState<FiringAlert[]>([])
  const [alertsEnabled, setAlertsEnabled] = useState(true)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    let cancelled = false

    const load = async () => {
      try {
        const [targets, agents] = await Promise.all([fetchTargets(), fetchAgents()])
        const byName = new Map(agents.map((a: AgentInfo) => [a.name, a.id]))
        const leaves = targets.filter((t) => t.host !== null && t.enabled && !t.hidden)
        const now = Math.floor(Date.now() / 1000)

        // One row per (target, agent), not per target. A target measured from
        // two places is two measurements, and collapsing them here would hide
        // exactly the disagreement a second vantage point exists to reveal —
        // including its quality flags.
        const series = leaves.flatMap((t) => {
          const names = (t.settings.agents.effective || 'local').split(/\s+/).filter(Boolean)
          const resolved = names
            .map((n) => ({ name: n, id: byName.get(n) }))
            .filter((a): a is { name: string; id: number } => a.id !== undefined)
          if (resolved.length === 0) return [{ target: t, agentId: 0, agentName: undefined }]
          return resolved.map((a) => ({
            target: t,
            agentId: a.id,
            agentName: resolved.length > 1 ? a.name : undefined,
          }))
        })

        const built = await Promise.all(
          series.map(async (x) => {
            try {
              const s = await fetchSeries(x.target.id, x.agentId, now - WINDOW_S, now)
              return { ...x, ...summarise(s) }
            } catch {
              return { ...x, median: 0, p95: 0, loss: 0, flags: 0, spark: [] }
            }
          }),
        )
        if (!cancelled) setRows(built)
      } catch (e) {
        if (!cancelled) setError((e as Error).message)
      }
    }

    void load()
    void fetchFiringAlerts()
      .then((r) => {
        if (cancelled) return
        setFiring(r.alerts)
        setAlertsEnabled(r.enabled)
      })
      .catch(() => undefined)

    return () => {
      cancelled = true
    }
  }, [])

  const worst = rows?.reduce((a, r) => Math.max(a, r.loss), 0) ?? 0
  const flagged = rows?.filter((r) => r.flags !== 0).length ?? 0
  const measured = rows?.filter((r) => r.spark.length > 0).length ?? 0

  return (
    <section>
      <div className="page-head">
        <h1>Network overview</h1>
        <span className="live-note">
          <span className="dot pulse" style={{ background: 'var(--good)' }} />
          live · last hour
        </span>
      </div>

      {error && <p className="error">{error}</p>}

      <div className="stat-grid">
        <Kpi label="Series" value={rows ? String(rows.length) : '—'} sub={`${measured} reporting`} />
        <Kpi
          label="Firing"
          value={alertsEnabled ? String(firing.length) : 'off'}
          sub={alertsEnabled ? (firing.length === 0 ? 'all quiet' : 'needs attention') : 'no webhook'}
          tone={firing.length > 0 ? 'bad' : undefined}
        />
        <Kpi
          label="Worst loss"
          value={rows ? `${worst.toFixed(1)}%` : '—'}
          sub="in the last hour"
          tone={worst > 0 ? 'bad' : undefined}
        />
        <Kpi
          label="Flagged"
          value={rows ? String(flagged) : '—'}
          sub="series with quality flags"
          tone={flagged > 0 ? 'warn' : undefined}
        />
      </div>

      <div className="overview-columns">
        <div className="card">
          <div className="panel-head">
            <h2>Targets</h2>
          </div>
          {rows === null ? (
            <p className="hint panel-pad">Measuring…</p>
          ) : rows.length === 0 ? (
            <p className="hint panel-pad">No targets yet.</p>
          ) : (
            <>
              {rows.map((r) => (
                <button
                  key={`${r.target.id}-${r.agentId}`}
                  className="health-row"
                  onClick={() => onOpenDetail(r.target.id)}
                >
                  <span
                    className="dot"
                    style={{
                      background:
                        r.spark.length === 0
                          ? 'var(--dim)'
                          : r.loss > 0
                            ? 'var(--bad)'
                            : r.flags !== 0
                              ? 'var(--warn)'
                              : 'var(--good)',
                    }}
                  />
                  <span className="health-name">
                    <span className="health-title">
                      {r.target.title ?? r.target.name}
                      {r.agentName && <span className="from-agent">from {r.agentName}</span>}
                    </span>
                    <span className="health-host">{r.target.host}</span>
                  </span>
                  <Spark values={r.spark} />
                  <span className="mono">{r.spark.length ? fmtUs(r.median) : '—'}</span>
                  <span className="mono dimmed">{r.spark.length ? fmtUs(r.p95) : '—'}</span>
                  <span className={r.loss > 0 ? 'mono bad' : 'mono dimmed'}>
                    {r.spark.length ? `${r.loss.toFixed(1)}%` : '—'}
                  </span>
                </button>
              ))}
              <p className="health-legend">
                <span />
                <span>target</span>
                <span>1h median</span>
                <span>median</span>
                <span>p95</span>
                <span>loss</span>
              </p>
            </>
          )}
        </div>

        <div className="overview-side">
          <div className="card panel">
            <h2>Firing now</h2>
            {!alertsEnabled ? (
              <p className="hint small">
                Rules are stored but never evaluated: no <code>--alert-webhook</code> is configured.
              </p>
            ) : firing.length === 0 ? (
              <p className="quiet-line">
                <span className="dot" style={{ background: 'var(--good)' }} />
                All quiet — no alerts firing.
              </p>
            ) : (
              firing.map((a, i) => (
                <div key={i} className="firing-card">
                  <span className="firing-title">
                    <span className="dot pulse" style={{ background: 'var(--bad)' }} />
                    {a.rule}
                  </span>
                  <span className="hint small">
                    {a.describes} on <code>{a.target}</code>
                  </span>
                </div>
              ))
            )}
          </div>

          <div className="card panel">
            <h2>Measurement quality</h2>
            <p className="hint small">
              A number is only worth the conditions it was taken under. Series carrying flags in the
              last hour:
            </p>
            {flagged === 0 ? (
              <p className="quiet-line">
                <span className="dot" style={{ background: 'var(--good)' }} />
                Nothing flagged — every series was measured cleanly.
              </p>
            ) : (
              <p className="hint small">
                <strong>{flagged}</strong> of {rows?.length} — open a graph to see which flags and
                why.
              </p>
            )}
          </div>
        </div>
      </div>
    </section>
  )
}

function Kpi({
  label,
  value,
  sub,
  tone,
}: {
  label: string
  value: string
  sub: string
  tone?: 'bad' | 'warn'
}) {
  return (
    <div className="card stat">
      <p className="stat-label">{label}</p>
      <p className="stat-value">
        <span className={tone ? tone : undefined}>{value}</span>
        <span className="stat-sub">{sub}</span>
      </p>
    </div>
  )
}

/** A shape, not a chart: enough to see whether the last hour was level. */
function Spark({ values }: { values: number[] }) {
  if (values.length < 2) return <span className="spark" />
  const lo = Math.min(...values)
  const hi = Math.max(...values)
  const span = hi - lo || 1
  const pts = values
    .map((v, i) => `${(i / (values.length - 1)) * 70 + 1},${24 - ((v - lo) / span) * 20}`)
    .join(' ')
  return (
    <svg className="spark" viewBox="0 0 72 26" preserveAspectRatio="none" aria-hidden="true">
      <polyline points={pts} fill="none" stroke="var(--accent)" strokeWidth="1.2" />
    </svg>
  )
}

function summarise(s: {
  values: Uint32Array
  offsets: Uint32Array
  sent: Float64Array
  received: Float64Array
  flags: Uint8Array
}): Omit<Row, 'target' | 'agentId'> {
  const all = Array.from(s.values).sort((a, b) => a - b)
  const q = (p: number) =>
    all.length ? all[Math.min(all.length - 1, Math.floor(p * all.length))] : 0
  let sent = 0
  let received = 0
  let flags = 0
  for (let i = 0; i < s.sent.length; i++) {
    sent += s.sent[i]
    received += s.received[i]
    flags |= s.flags[i]
  }
  // One median per row, thinned to at most 40 points.
  const rows = s.offsets.length - 1
  const step = Math.max(1, Math.ceil(rows / 40))
  const spark: number[] = []
  for (let i = 0; i < rows; i += step) {
    const a = s.offsets[i]
    const b = s.offsets[i + 1]
    if (b > a) spark.push(s.values[a + ((b - a) >> 1)])
  }
  return {
    median: q(0.5),
    p95: q(0.95),
    loss: sent > 0 ? 100 * (1 - received / sent) : 0,
    flags,
    spark,
  }
}

function fmtUs(us: number): string {
  if (us < 1000) return `${Math.round(us)}µs`
  if (us < 1_000_000) return `${(us / 1000).toFixed(us < 10_000 ? 1 : 0)}ms`
  return `${(us / 1_000_000).toFixed(1)}s`
}
