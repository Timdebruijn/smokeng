import { Fragment, useEffect, useRef, useState } from 'react'
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
import { Stat, StatusDot, ErrorText, LivePill, downloadCSV, useTimeWindow } from './shared'
import {
  RANGES,
  measurementLabel,
  seriesLabel,
  shownFamilies,
  summarise,
  type Family,
  type FamilyChoice,
} from './util'

// `types` limits a row to the probe types it means something for, the same way
// the admin form does. A packet size on an http target is not a setting with
// an unusual value; it is a setting that does nothing, and listing it under
// "effective settings" would claim otherwise.
// The key of one family's figures: empty for the one series of a single-family target.
const familyKey = (f?: Family) => f ?? ''

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
  {
    key: 'positional_loss',
    label: 'Positional loss',
    render: (v) => (v === 'flag_only' ? 'flag only' : 'excluded from loss rules'),
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
  family,
  onBack,
  onEdit,
}: {
  target: Target
  agents: AgentInfo[]
  family: FamilyChoice
  onBack: () => void
  onEdit: () => void
}) {
  const { rangeS, from, to, refreshKey, live, zoom, setZoom, pickRange, toggleLive } = useTimeWindow()
  // Per family, under the key familyKey gives: a both target has two sets of figures.
  const [stats, setStats] = useState<Record<string, ReturnType<typeof summarise> | null>>({})
  const [rules, setRules] = useState<AlertRule[]>([])

  const probeType = String(target.settings.probe_type.effective || 'icmp')
  const agentNames = (target.settings.agents.effective || 'local').split(/\s+/).filter(Boolean)
  const byName = new Map(agents.map((a) => [a.name, a.id]))
  // Null rather than 0: falling back to the local prober would show one
  // vantage point's measurements under another's name.
  const agentId = byName.get(agentNames[0]) ?? null
  // One entry (family unset) for a single-family target; v4 then v6 for a both
  // target, less what the filter hides.
  const families = shownFamilies(target, family)
  const familiesKey = families.map(familyKey).join()

  useEffect(() => {
    if (agentId === null) return
    let cancelled = false
    for (const f of families) {
      // One column: the server pools the whole window, so these are taken over
      // every sample in it (its 256 quantiles past that many), at any length.
      fetchSeries(target.id, agentId, from, to, 1, f)
        .then((s) => !cancelled && setStats((o) => ({ ...o, [familyKey(f)]: summarise(s) })))
        .catch(() => !cancelled && setStats((o) => ({ ...o, [familyKey(f)]: null })))
    }
    return () => {
      cancelled = true
    }
    // familiesKey stands for families, which is a new array on every render.
  }, [target.id, agentId, familiesKey, from, to])
  const shownStats = agentId === null ? [] : families.map((f) => stats[familyKey(f)]).filter((s) => s != null)

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
        <StatusDot
          background={
            shownStats.some((s) => s.sent > 0)
              ? shownStats.some((s) => s.loss > 0)
                ? 'var(--warn)'
                : 'var(--good)'
              : 'var(--dim)'
          }
          label={measurementLabel({
            sent: shownStats.some((s) => s.sent > 0) ? 1 : 0,
            loss: shownStats.some((s) => s.loss > 0) ? 1 : 0,
          })}
        />
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

      {families.length === 0 ? (
        <p className="hint">This target's series are hidden by the family filter.</p>
      ) : (
        families.map((f) => {
          const st = agentId === null ? null : stats[familyKey(f)]
          return (
            <Fragment key={familyKey(f)}>
              {f && <p className="family-label">{f}</p>}
              <div className="stat-grid">
                <Stat label="Median" value={st?.median != null ? fmtUs(st.median) : '—'} />
                <Stat label="p95" value={st?.p95 != null ? fmtUs(st.p95) : '—'} />
                <Stat
                  label="Spread (p95−p5)"
                  value={st?.spread != null ? fmtUs(st.spread) : '—'}
                  hint="The width of the distribution. A link whose median is unchanged but whose spread has tripled is degrading."
                />
                <Stat
                  label="Loss"
                  // Nothing sent in the window (no address, no route) is no data, not a
                  // loss of 0%.
                  value={st && st.sent > 0 ? fmtLoss(st.loss) : '—'}
                  tone={st && st.loss > 0 ? 'bad' : undefined}
                />
              </div>
            </Fragment>
          )
        })
      )}

      {families.length > 0 && (
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
          <div className="range-group" role="group" aria-label="Time range">
            {RANGES.map((r) => (
              <button
                key={r.label}
                className={!zoom && r.seconds === rangeS ? 'pill active' : 'pill'}
                aria-pressed={!zoom && r.seconds === rangeS}
                onClick={() => pickRange(r.seconds)}
              >
                {r.label}
              </button>
            ))}
          </div>
          <LivePill live={live} onToggle={toggleLive} />
          {zoom && (
            <button className="pill accent" onClick={() => setZoom(null)}>
              reset zoom
            </button>
          )}
        </div>
        {agentId === null ? (
          <p className="hint">
            Assigned to <code>{agentNames.join(' ')}</code>, which is not enrolled — so nothing is
            measuring this target.
          </p>
        ) : (
          // The families one under the other, v4 first, each with its own
          // extra series and route marks; they share the window and, through
          // the crosshair bus, the cursor.
          families.map((f) => (
            <Fragment key={familyKey(f)}>
              {/* Named on the plot itself: the stat rows above carry the
                  family too, but two plots told apart only by which comes
                  first are not told apart. */}
              {f && <p className="family-label">{f}</p>}
              <Plot
                target={target}
                agentId={agentId}
                family={f}
                from={from}
                to={to}
                refreshKey={refreshKey}
                logScale
                onZoom={(zf, zt) => setZoom({ from: zf, to: zt })}
              />
              {selectedSeries(target).map((name) => (
                <SeriesPlot
                  key={name}
                  targetId={target.id}
                  targetName={target.title ?? target.path}
                  agentId={agentId}
                  family={f}
                  name={name}
                  from={from}
                  to={to}
                />
              ))}
            </Fragment>
          ))
        )}
      </div>
      )}

      {families.length > 0 && <AvailabilityPanel target={target} families={families} />}

      <div className="detail-columns">
        <div className="card panel">
          <h2>Effective settings</h2>
          <dl className="plain-list">
            {SETTING_LABELS.filter(
              (s) => !s.types || s.types.includes(probeType),
            ).map((s) => {
              const v = target.settings[s.key] as SettingValue<number | string | boolean>
              return (
                <div key={s.key} className="setting-line">
                  <dt className="setting-label">{s.label}</dt>
                  <dd>
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
                            : `inherited from ${v.source.path}`}
                    </span>
                  </dd>
                </div>
              )
            })}
          </dl>
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
                  <StatusDot
                    background={r.enabled ? 'var(--good)' : 'var(--dim)'}
                    label={r.enabled ? 'enabled' : 'disabled'}
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

function AvailabilityPanel({ target, families }: { target: Target; families: (Family | undefined)[] }) {
  const [periodS, setPeriodS] = useState(7 * 86400)
  const [strict, setStrict] = useState(true) // strict = down only at 100% loss
  // One response per family: a single unset one for a single-family target.
  const [data, setData] = useState<{ family?: Family; res: AvailabilityResponse }[] | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [loading, setLoading] = useState(false)
  const familiesKey = families.map(familyKey).join()
  const reportOf = useRef(familiesKey)

  useEffect(() => {
    let cancelled = false
    setLoading(true)
    // When the families shown change, the report on screen is of the ones
    // shown a moment ago: drop it. For another period or threshold the old
    // report stays up until the new one is in, as it always did.
    if (reportOf.current !== familiesKey) {
      reportOf.current = familiesKey
      setData(null)
    }
    const to = Math.floor(Date.now() / 1000)
    const from = to - periodS
    Promise.all(
      families.map((family) =>
        fetchAvailability(target.id, from, to, strict ? 100 : 50, family).then((res) => ({ family, res })),
      ),
    )
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
    // familiesKey stands for families, which is a new array on every render.
  }, [target.id, periodS, strict, familiesKey])

  const exportCSV = () => {
    if (!data) return
    // A family column only where there is more than one family to tell apart,
    // so the file of a single-family target is what it always was.
    const withFamily = data.some((d) => d.family)
    const rows: (string | number)[][] = [
      ['target', ...(withFamily ? ['family'] : []), 'agent', 'availability', 'coverage', 'up_s', 'down_s', 'unknown_s', 'outages'],
    ]
    for (const { family, res } of data) {
      for (const a of res.agents) {
        const r = a.report
        rows.push([
          res.target,
          ...(withFamily ? [family ?? ''] : []),
          a.agent,
          r.availability,
          r.coverage,
          r.up_s,
          r.down_s,
          r.unknown_s,
          r.downtime.length,
        ])
      }
    }
    const name = `availability-${data[0].res.target.replace(/[^\w-]+/g, '_')}.csv`
    downloadCSV(name, rows)
  }

  return (
    <section className="card section-card" aria-busy={loading}>
      <div className="section-card-head">
        <h2 className="section-card-title">Availability</h2>
        <span className="spacer" />
        <span className="pill-row" role="group" aria-label="Availability period">
          {AVAIL_PERIODS.map((p) => (
            <button
              key={p.label}
              className={p.seconds === periodS ? 'pill active' : 'pill'}
              aria-pressed={p.seconds === periodS}
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
          <button className="pill" onClick={exportCSV} disabled={!data || data.length === 0}>
            Download CSV
          </button>
        </span>
      </div>

      {error && <ErrorText>{error}</ErrorText>}
      {loading && !data ? (
        <p className="hint">Loading…</p>
      ) : !data || data.every((d) => d.res.agents.length === 0) ? (
        <p className="hint">No vantage point is measuring this target.</p>
      ) : (
        data.flatMap(({ family, res }) =>
          res.agents.map((a) => ({ family, a })),
        ).map(({ family, a }) => {
          const r = a.report
          return (
            <div key={`${a.agent_id}${family ? `-${family}` : ''}`} className="avail-agent">
              <div className="avail-head">
                <span className="agent">{seriesLabel(a.agent, family)}</span>
                {!r.has_data && <span className="hint small">no data in this period</span>}
              </div>
              {r.has_data && (
                <>
                  <div className="stat-grid">
                    <Stat
                      label="Availability"
                      value={pct(r.availability)}
                      hint="Uptime over the intervals there is data for"
                      tone={r.availability < 0.999 ? 'bad' : undefined}
                    />
                    <Stat
                      label="Coverage"
                      value={pct(r.coverage)}
                      hint="How much of the period was actually measured — the rest is unknown, not up"
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
                        <caption className="visually-hidden">Outages</caption>
                        <thead className="visually-hidden">
                          <tr>
                            <th scope="col">Start</th>
                            <th scope="col">End</th>
                            <th scope="col">Duration</th>
                          </tr>
                        </thead>
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
