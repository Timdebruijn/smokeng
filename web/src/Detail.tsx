import { useEffect, useMemo, useState } from 'react'
import Plot from './Plot'
import {
  fetchAlertRules,
  fetchSeries,
  type AgentInfo,
  type AlertRule,
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

const SETTING_LABELS: { key: keyof Target['settings']; label: string; unit?: string }[] = [
  { key: 'interval_s', label: 'Interval', unit: 's' },
  { key: 'pings_per_interval', label: 'Pings per interval' },
  { key: 'probe_mode', label: 'Probe mode' },
  { key: 'burst_gap_ms', label: 'Burst gap', unit: 'ms' },
  { key: 'timeout_ms', label: 'Timeout', unit: 'ms' },
  { key: 'packet_size', label: 'Packet size', unit: 'bytes' },
  { key: 'dscp', label: 'DSCP' },
  { key: 'agents', label: 'Agents' },
  { key: 'trace_interval_s', label: 'Path discovery', unit: 's' },
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
        <code className="host">
          {target.host} · {target.address_family}
        </code>
        <span className="spacer" />
        <button className="pill" onClick={onEdit}>
          Edit in Targets
        </button>
      </div>

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
      </div>

      <div className="detail-columns">
        <div className="card panel">
          <h2>Effective settings</h2>
          {SETTING_LABELS.map((s) => {
            const v = target.settings[s.key] as SettingValue<number | string>
            return (
              <div key={s.key} className="setting-line">
                <span className="setting-label">{s.label}</span>
                <span className="mono">
                  {String(v.effective)}
                  {s.unit && <span className="unit">{s.unit}</span>}
                </span>
                <span className="spacer" />
                <span className={v.source === 'local' ? 'chip local' : 'chip'}>
                  {v.source === 'local'
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
                  <span className="hint small">
                    {r.metric} {r.op} {r.threshold}
                    {r.metric === 'loss' ? '%' : 'ms'}
                  </span>
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

