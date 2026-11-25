import { useCallback, useEffect, useState } from 'react'
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
import { Stat, StatusDot, ErrorText, LivePill, useLiveRefresh } from './shared'
import {
  afterName,
  alertKey,
  countedFlags,
  showsFamily,
  measurementLabel,
  minMax,
  seriesKey,
  seriesLabel,
  REFRESH_MS,
  resolveSeries,
  summarise,
  type Family,
  type FamilyChoice,
} from './util'

const WINDOW_S = 3600

interface Row {
  target: Target
  // null means the target's configured agent name(s) never resolved to an
  // enrolled agent — there is nothing to query, not just nothing queried yet.
  agentId: number | null
  agentName?: string
  family?: Family
  median: number | null
  p95: number | null
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
export default function Overview({
  family,
  onOpenDetail,
}: {
  family: FamilyChoice
  onOpenDetail: (id: number) => void
}) {
  // Every series, whatever the family filter says: the filter is applied to
  // what is drawn, so changing it shows the other rows at once instead of the
  // old ones until a refetch returns.
  const [all, setAll] = useState<Row[] | null>(null)
  // Targets that would have a row but for the family filter, to tell an empty
  // overview from one the filter emptied.
  const [targetCount, setTargetCount] = useState(0)
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

  const bump = useCallback(() => setRefreshTick((t) => t + 1), [])
  const { live, toggleLive } = useLiveRefresh(bump, REFRESH_MS)

  useEffect(() => {
    let cancelled = false

    const load = async () => {
      try {
        const [targets, agents] = await Promise.all([fetchTargets(), fetchAgents()])
        const byName = new Map(agents.map((a: AgentInfo) => [a.name, a.id]))
        const leaves = targets.filter((t) => t.host !== null && t.enabled && !t.hidden)
        const now = Math.floor(Date.now() / 1000)

        // One row per (target, agent, family), not per target. A target measured
        // from two places, or over two families, is two measurements, and collapsing them here would hide
        // exactly the disagreement a second vantage point exists to reveal —
        // including its quality flags.
        const series = leaves.flatMap((t) => resolveSeries(t, byName, ', '))

        const built = await Promise.all(
          series.map(async (x) => {
            const none = { median: null, p95: null, loss: 0, sent: 0, received: 0, flags: 0, spark: [] }
            if (x.agentId === null) return { ...x, ...none }
            try {
              const s = await fetchSeries(x.target.id, x.agentId, now - WINDOW_S, now, undefined, x.family)
              return { ...x, ...summarise(s), spark: sparkOf(s) }
            } catch {
              return { ...x, ...none }
            }
          }),
        )
        if (!cancelled) {
          setAll(built)
          setTargetCount(leaves.length)
        }
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

  const rows = all && all.filter((r) => showsFamily(family, r.target, r.family))
  const worst = rows?.reduce((a, r) => Math.max(a, r.loss), 0) ?? 0
  // The userspace pair on tcp, http(s) and irtt is not counted: see countedFlags.
  const counted = (r: { flags: number; target: Target }) =>
    countedFlags(r.flags, r.target.settings.probe_type.effective)
  const flagged = rows?.filter((r) => counted(r) !== 0).length ?? 0
  const measured = rows?.filter((r) => r.spark.length > 0).length ?? 0

  return (
    <section>
      <div className="page-head">
        <h1>Network overview</h1>
        <span className="live-note">
          <LivePill live={live} onToggle={toggleLive} />
          last hour
        </span>
      </div>

      {error && <ErrorText>{error}</ErrorText>}

      <div className="stat-grid">
        <Stat label="Series" value={rows ? String(rows.length) : '—'} sub={`${measured} reporting`} />
        <Stat
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
        <Stat
          label="Worst loss"
          value={rows ? fmtLoss(worst) : '—'}
          sub="in the last hour"
          tone={worst > 0 ? 'bad' : undefined}
        />
        <Stat
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
            <p className="hint panel-pad">
              {targetCount > 0 ? 'Every series is hidden by the family filter.' : 'No targets yet.'}
            </p>
          ) : (
            <>
              {rows.map((r) => {
                // `spark.length` is not "do we have anything to show": a target
                // with total loss has sent probes and gotten zero replies back,
                // so its spark and latencies are empty even though the loss
                // figure is exactly known and is the single most important
                // thing this row can say. `sent` tells "never measured" apart
                // from "measured, and everything was lost".
                const noData = r.sent === 0
                return (
                  <button
                    key={seriesKey(r.target.id, r.agentId, r.family)}
                    className="health-row"
                    onClick={() => onOpenDetail(r.target.id)}
                  >
                    <StatusDot
                      background={
                        noData
                          ? 'var(--dim)'
                          : r.loss > 0
                            ? 'var(--bad)'
                            : counted(r) !== 0
                              ? 'var(--warn)'
                              : 'var(--good)'
                      }
                      label={measurementLabel({ sent: r.sent, loss: r.loss, flagged: counted(r) })}
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
                        {/* The family comes first on the host line, so the two
                            rows of a target measured over both families differ
                            at the start of the line, not after a long host. */}
                        {r.family ? `${r.family} · ` : ''}
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
                    <span className="mono">{r.median === null ? '—' : fmtUs(r.median)}</span>
                    <span className="mono dimmed">{r.p95 === null ? '—' : fmtUs(r.p95)}</span>
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
                <span className="dot" style={{ background: 'var(--good)' }} aria-hidden="true" />
                All quiet — no alerts firing.
              </p>
            ) : (
              firing.map((a) => (
                <div key={alertKey(a)} className="firing-card">
                  <span className="firing-title">
                    <span className="dot pulse" style={{ background: 'var(--bad)' }} aria-hidden="true" />
                    {a.rule}
                  </span>
                  <span className="hint small">
                    {a.describes} on <code>{a.target}</code>
                    {afterName(seriesLabel(a.agent, a.family))}
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
                    aria-hidden="true"
                  />
                  <span className="activity-what">
                    <strong>{e.firing ? 'fired' : 'cleared'}</strong> · {e.rule} on{' '}
                    <code>{e.target}</code>
                    {afterName(e.family)}
                  </span>
                  <span className="activity-when">{rel(e.ts)}</span>
                </div>
              ))
            )}
          </div>

          <div className="card panel">
            <h2>Measurement quality</h2>
            <p className="hint small">
              A figure is only as good as the conditions it was measured under, and smokeng flags
              a measurement it cannot fully vouch for. Series that carried a flag in the last
              hour, not counting the userspace-timing flag that every tcp, http(s) and irtt probe
              has:
            </p>
            {flagged === 0 ? (
              <p className="quiet-line">
                <span className="dot" style={{ background: 'var(--good)' }} aria-hidden="true" />
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

/** A shape, not a chart: enough to see whether the last hour was level. */
function Spark({ values }: { values: number[] }) {
  if (values.length < 2) return <span className="spark" />
  const range = minMax(values)
  if (!range) return <span className="spark" />
  const { lo, hi } = range
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

/** One median per row, thinned to at most 40 points. */
function sparkOf(s: { values: Uint32Array; offsets: Uint32Array }): number[] {
  const rows = s.offsets.length - 1
  const step = Math.max(1, Math.ceil(rows / 40))
  const spark: number[] = []
  for (let i = 0; i < rows; i += step) {
    const a = s.offsets[i]
    const b = s.offsets[i + 1]
    if (b > a) spark.push(s.values[a + ((b - a) >> 1)])
  }
  return spark
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

