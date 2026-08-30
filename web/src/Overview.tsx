import { useEffect, useState } from 'react'
import {
  fetchAgents,
  fetchAlertEvents,
  fetchFiringAlerts,
  fetchSeries,
  fetchTargets,
  type AgentInfo,
  type AlertEvent,
  type FiringAlert,
  type Target,
} from './api'
import { fmtLoss, fmtUs } from './format'

const WINDOW_S = 3600
// Matches the Graphs screen's REFRESH_MS (App.tsx) — the same "live" claim
// should mean the same thing everywhere it's made.
const REFRESH_MS = 10_000

interface Row {
  target: Target
  // null means the target's configured agent name(s) never resolved to an
  // enrolled agent — there is nothing to query, not just nothing queried yet.
  agentId: number | null
  agentName?: string
  median: number
  p95: number
  loss: number
  // Totals behind `loss`, kept alongside it because loss alone can't tell a
  // reader apart from "we have nothing" — see the row rendering below.
  sent: number
  received: number
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
  const [delivering, setDelivering] = useState(true)
  const [recent, setRecent] = useState<AlertEvent[]>([])
  const [error, setError] = useState<string | null>(null)

  // Bumped on a timer to re-run the effect below. Relying on the effect's own
  // cleanup (which React runs before the next tick's effect body) to flip
  // `cancelled` on the previous tick is what stops a slow, stale request from
  // landing after a faster, newer one already painted the screen — the same
  // pattern Graphs (App.tsx) uses for the same reason.
  const [refreshTick, setRefreshTick] = useState(0)

  useEffect(() => {
    const id = setInterval(() => setRefreshTick((t) => t + 1), REFRESH_MS)
    return () => clearInterval(id)
  }, [])

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
          // A name that never resolved to an enrolled agent is not "the local
          // agent" — querying agent 0 here would silently show one vantage
          // point's numbers labelled as another's. The server counts this
          // target as smokeng_targets_unmeasured; say the same thing rather
          // than inventing a series for it.
          if (resolved.length === 0) {
            return [{ target: t, agentId: null, agentName: names.join(', ') }]
          }
          return resolved.map((a) => ({
            target: t,
            agentId: a.id as number | null,
            agentName: resolved.length > 1 ? a.name : undefined,
          }))
        })

        const built = await Promise.all(
          series.map(async (x) => {
            if (x.agentId === null) {
              return { ...x, median: 0, p95: 0, loss: 0, sent: 0, received: 0, flags: 0, spark: [] }
            }
            try {
              const s = await fetchSeries(x.target.id, x.agentId, now - WINDOW_S, now)
              return { ...x, ...summarise(s) }
            } catch {
              return { ...x, median: 0, p95: 0, loss: 0, sent: 0, received: 0, flags: 0, spark: [] }
            }
          }),
        )
        if (!cancelled) setRows(built)
      } catch (e) {
        if (!cancelled) setError((e as Error).message)
      }
    }

    void load()
    void fetchAlertEvents(6)
      .then((e) => !cancelled && setRecent(e))
      .catch(() => undefined)
    void fetchFiringAlerts()
      .then((r) => {
        if (cancelled) return
        setFiring(r.alerts)
        setDelivering(r.delivering)
      })
      .catch(() => undefined)

    return () => {
      cancelled = true
    }
  }, [refreshTick])

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
          value={String(firing.length)}
          // An acknowledged alert is still firing, so it counts here — but it
          // no longer "needs attention", and the tone drops to neutral once
          // every firing alert has been seen, so the overview stops shouting
          // about something an operator is already handling.
          sub={
            firing.length === 0
              ? 'all quiet'
              : firing.every((a) => a.acked)
                ? 'all acknowledged'
                : `${firing.filter((a) => !a.acked).length} need attention`
          }
          tone={firing.some((a) => !a.acked) ? 'bad' : undefined}
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
            {/* Said once, here, rather than implied differently per column. */}
            <span className="hint small">every figure over the last hour</span>
          </div>
          {rows === null ? (
            <p className="hint panel-pad">Measuring…</p>
          ) : rows.length === 0 ? (
            <p className="hint panel-pad">No targets yet.</p>
          ) : (
            <>
              {rows.map((r) => {
                // Three states, not two. `spark.length` used to stand in for
                // "do we have anything to show", but a target with total loss
                // has sent probes and gotten zero replies back — its spark is
                // empty (there is no RTT to plot a shape from) even though the
                // loss figure itself is exactly known and is the single most
                // important thing this row can say. Read `sent`/`received`
                // instead of the spark to tell "never measured" apart from
                // "measured, and everything was lost".
                const noData = r.sent === 0
                const noReplies = !noData && r.received === 0
                return (
                  <button
                    key={`${r.target.id}-${r.agentId ?? 'unresolved'}`}
                    className="health-row"
                    onClick={() => onOpenDetail(r.target.id)}
                  >
                    <span
                      className="dot"
                      style={{
                        background: noData
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
                        {r.agentId === null ? (
                          <span className="from-agent">{r.agentName} not enrolled</span>
                        ) : (
                          r.agentName && <span className="from-agent">from {r.agentName}</span>
                        )}
                      </span>
                      <span className="health-host">
                        {r.target.host}
                        {/* Only when it is not the default. On a dense list a
                            badge reading "icmp" on every row carries no
                            information; one reading "https" tells you why this
                            row's numbers are an order of magnitude larger than
                            its neighbour's. The detail page always says. */}
                        {r.target.settings.probe_type.effective !== 'icmp' && (
                          <span className="badge probe-type">
                            {r.target.settings.probe_type.effective}
                          </span>
                        )}
                      </span>
                    </span>
                    <Spark values={r.spark} />
                    <span className="mono">{noData || noReplies ? '—' : fmtUs(r.median)}</span>
                    <span className="mono dimmed">{noData || noReplies ? '—' : fmtUs(r.p95)}</span>
                    <span className={!noData && r.loss > 0 ? 'mono bad' : 'mono dimmed'}>
                      {noData ? '—' : fmtLoss(r.loss)}
                    </span>
                  </button>
                )
              })}
              {/* Every column on this row covers the same hour, so only the
                  shape column needs saying what it plots — labelling one of
                  them "1h" implied the others were something else. */}
              <p className="health-legend">
                <span />
                <span>target</span>
                <span>median over time</span>
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
            {!delivering && (
              <p className="hint small">
                Evaluated and recorded, but not posted anywhere: no <code>--alert-webhook</code>.
              </p>
            )}
            {firing.length === 0 ? (
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
            <h2>Recent alert activity</h2>
            {recent.length === 0 ? (
              <p className="hint small">
                Nothing has fired or cleared yet. Transitions are recorded as they happen.
              </p>
            ) : (
              recent.map((e) => (
                <div key={e.id} className="activity-line">
                  <span
                    className="dot"
                    style={{ background: e.firing ? 'var(--bad)' : 'var(--good)' }}
                  />
                  <span className="activity-what">
                    <strong>{e.firing ? 'fired' : 'cleared'}</strong> · {e.rule} on{' '}
                    <code>{e.target}</code>
                  </span>
                  <span className="activity-when">{rel(e.ts)}</span>
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
    // Exposed alongside loss so the row rendering can tell "nothing sent"
    // apart from "sent and lost" without going back to the spark, which is
    // empty in both cases and can't make that distinction.
    sent,
    received,
    flags,
    spark,
  }
}

/** Coarse and readable: this is a sense of when, not a timestamp. */
function rel(ts: number): string {
  const d = Math.floor(Date.now() / 1000) - ts
  if (d < 60) return `${Math.max(d, 0)}s ago`
  const m = Math.round(d / 60)
  if (m < 60) return `${m}m ago`
  const h = Math.round(m / 60)
  if (h < 24) return `${h}h ago`
  return `${Math.round(h / 24)}d ago`
}

