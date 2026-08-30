import { useCallback, useEffect, useState } from 'react'
import {
  createAlertRule,
  deleteAlertRule,
  fetchAlertRules,
  acknowledgeAlert,
  fetchFiringAlerts,
  fetchTargets,
  updateAlertRule,
  type AlertRule,
  type FiringAlert,
  type Target,
  fetchAlertEvents,
  type AlertEvent,
} from './api'

const METRICS: { value: AlertRule['metric']; label: string; unit: string }[] = [
  { value: 'loss', label: 'Packet loss', unit: '%' },
  { value: 'median', label: 'Median RTT', unit: 'ms' },
  { value: 'p95', label: 'p95 RTT', unit: 'ms' },
  { value: 'spread', label: 'Spread (p95 − p5)', unit: 'ms' },
]
const REFRESH_MS = 15_000

export default function Alerts({ isAdmin }: { isAdmin: boolean }) {
  const [rules, setRules] = useState<AlertRule[]>([])
  const [firing, setFiring] = useState<FiringAlert[]>([])
  const [delivering, setDelivering] = useState(true)
  const [targets, setTargets] = useState<Target[]>([])
  const [error, setError] = useState<string | null>(null)
  const [adding, setAdding] = useState(false)

  const reload = useCallback(async () => {
    try {
      const [r, f, t] = await Promise.all([fetchAlertRules(), fetchFiringAlerts(), fetchTargets()])
      setRules(r)
      setFiring(f.alerts ?? [])
      setDelivering(f.delivering)
      setTargets(t)
      setError(null)
    } catch (e) {
      setError((e as Error).message)
    }
  }, [])

  useEffect(() => {
    void reload()
    const id = setInterval(() => void reload(), REFRESH_MS)
    return () => clearInterval(id)
  }, [reload])

  const ackAlert = useCallback(
    async (a: FiringAlert) => {
      try {
        await acknowledgeAlert(a, !a.acked)
        await reload()
      } catch (e) {
        setError((e as Error).message)
      }
    },
    [reload],
  )

  const run = async (fn: () => Promise<unknown>) => {
    try {
      await fn()
      await reload()
      return true
    } catch (e) {
      setError((e as Error).message)
      return false
    }
  }

  const pathOf = (id: number) => targets.find((t) => t.id === id)?.path ?? `target ${id}`

  return (
    <>
      {error && <p className="error">{error}</p>}
      {!delivering && (
        <p className="hint warn">
          Rules are evaluated and their transitions recorded, but nothing is posted anywhere:
          no <code>--alert-webhook</code> is set. Firing state and the history below are still
          live.
        </p>
      )}

      <section className="card section-card">
        <div className="section-card-head">
          <span className="section-card-title">Firing</span>
        </div>
        {firing.length === 0 ? (
          <p className="hint">Nothing is firing.</p>
        ) : (
          <table className="alerts">
            <tbody>
              {firing.map((a) => (
                <tr key={`${a.target}-${a.rule}`} className={a.acked ? 'disabled-row' : undefined}>
                  <td>
                    <span className="dot-label">
                      <span
                        className="dot"
                        style={{ background: a.acked ? 'var(--dim)' : 'var(--bad)' }}
                      />
                      <span className="chip firing">{a.rule}</span>
                    </span>
                  </td>
                  <td>
                    <code>{a.target}</code>
                  </td>
                  <td>
                    {a.metric} is {a.value.toFixed(a.metric === 'loss' ? 0 : 2)}
                    {a.metric === 'loss' ? '%' : 'ms'}
                  </td>
                  <td className="dim">
                    {a.since ? (
                      <>
                        since <span className="mono">{new Date(a.since * 1000).toLocaleString()}</span>
                      </>
                    ) : (
                      ''
                    )}
                    {a.acked && a.acked_by ? (
                      <span className="hint small"> · acknowledged by {a.acked_by}</span>
                    ) : (
                      ''
                    )}
                  </td>
                  <td>
                    <button
                      className="pill small"
                      title={
                        a.acked
                          ? 'Un-acknowledge'
                          : 'Acknowledge — the alert keeps firing, it just stops demanding attention'
                      }
                      onClick={() => void ackAlert(a)}
                    >
                      {a.acked ? 'un-acknowledge' : 'acknowledge'}
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </section>

      <section className="card section-card">
        <div className="section-card-head">
          <span className="section-card-title">Rules</span>
        </div>
        <p className="hint">
          A rule applies to the node it is defined on and everything below it. A node with its own
          rules replaces the inherited set rather than adding to it. Rules only fire after the
          condition has held for the given number of consecutive intervals, and only clear after it
          has failed for as many — so a single bad interval never pages anyone.
        </p>
        {rules.length === 0 && <p className="hint">No rules defined.</p>}
        {rules.length > 0 && (
          <table className="alerts">
            <tbody>
              {rules.map((r) => (
                <tr key={r.id} className={r.enabled ? '' : 'disabled-row'}>
                  <td>
                    <span className="dot-label">
                      <span className="dot" style={{ background: r.enabled ? 'var(--good)' : 'var(--warn)' }} />
                      {r.name}
                    </span>
                  </td>
                  <td>
                    <code>{pathOf(r.target_id)}</code>
                  </td>
                  <td>{r.describes}</td>
                  <td className="dim">clears after {r.clear_intervals}</td>
                  {isAdmin && (
                    <td>
                      <button
                        className={r.enabled ? 'pill active' : 'pill'}
                        onClick={() => void run(() => updateAlertRule(r.id, { enabled: !r.enabled }))}
                      >
                        {r.enabled ? 'Disable' : 'Enable'}
                      </button>
                      <button className="pill danger" onClick={() => void run(() => deleteAlertRule(r.id))}>
                        Delete
                      </button>
                    </td>
                  )}
                </tr>
              ))}
            </tbody>
          </table>
        )}

        {isAdmin &&
          (adding ? (
            <RuleForm
              targets={targets}
              onCancel={() => setAdding(false)}
              onSubmit={async (body) => {
                if (await run(() => createAlertRule(body))) setAdding(false)
              }}
            />
          ) : (
            <div className="pill-row">
              <button className="pill accent" onClick={() => setAdding(true)}>
                Add rule…
              </button>
            </div>
          ))}
      </section>

      <AlertHistory />
    </>
  )
}

/**
 * What alerting has done, as opposed to what it is doing now.
 *
 * The rule name and description here are the ones recorded at the time, not
 * looked up from the rule as it stands: renaming or re-thresholding a rule
 * must not rewrite what the log says happened.
 */
function AlertHistory() {
  const [events, setEvents] = useState<AlertEvent[]>([])
  const [loaded, setLoaded] = useState(false)

  useEffect(() => {
    void fetchAlertEvents(50)
      .then(setEvents)
      .catch(() => setEvents([]))
      .finally(() => setLoaded(true))
  }, [])

  return (
    <section className="card section-card">
      <div className="section-card-head">
        <h2 className="section-card-title">History</h2>
      </div>
      {!loaded ? (
        <p className="hint">Loading…</p>
      ) : events.length === 0 ? (
        <p className="hint">
          Nothing has fired or cleared yet. Transitions are recorded as they happen, whether or not
          a webhook was reachable.
        </p>
      ) : (
        <table className="alerts">
          <tbody>
            {events.map((e) => (
              <tr key={e.id}>
                <td>
                  <span className="dot-label">
                    <span
                      className="dot"
                      style={{ background: e.firing ? 'var(--bad)' : 'var(--good)' }}
                    />
                    {e.firing ? 'fired' : 'cleared'}
                  </span>
                </td>
                <td>
                  <strong>{e.rule}</strong>
                </td>
                <td className="hint">{e.describes}</td>
                <td>
                  <code>{e.target}</code>
                </td>
                <td className="mono">{new Date(e.ts * 1000).toLocaleString()}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </section>
  )
}

function RuleForm({
  targets,
  onSubmit,
  onCancel,
}: {
  targets: Target[]
  onSubmit: (body: Partial<AlertRule>) => void
  onCancel: () => void
}) {
  const [name, setName] = useState('')
  const [targetId, setTargetId] = useState<number>(targets[0]?.id ?? 1)
  const [metric, setMetric] = useState<AlertRule['metric']>('loss')
  const [op, setOp] = useState<AlertRule['op']>('>')
  const [threshold, setThreshold] = useState('20')
  const [forIntervals, setForIntervals] = useState('3')
  const [clearIntervals, setClearIntervals] = useState('3')
  const unit = METRICS.find((m) => m.value === metric)?.unit ?? ''

  return (
    <form
      className="card rule-form"
      onSubmit={(e) => {
        e.preventDefault()
        onSubmit({
          name,
          target_id: targetId,
          metric,
          op,
          threshold: Number(threshold),
          for_intervals: Number(forIntervals),
          clear_intervals: Number(clearIntervals),
        })
      }}
    >
      <label className="field">
        <span>Name</span>
        <input value={name} onChange={(e) => setName(e.target.value)} required />
      </label>
      <label className="field">
        <span>Applies to</span>
        <select value={targetId} onChange={(e) => setTargetId(Number(e.target.value))}>
          {targets.map((t) => (
            <option key={t.id} value={t.id}>
              {t.path === '/' ? '/ (everything)' : t.path}
            </option>
          ))}
        </select>
      </label>
      <label className="field">
        <span>When</span>
        <span className="inline">
          <select value={metric} onChange={(e) => setMetric(e.target.value as AlertRule['metric'])}>
            {METRICS.map((m) => (
              <option key={m.value} value={m.value}>
                {m.label}
              </option>
            ))}
          </select>
          <select value={op} onChange={(e) => setOp(e.target.value as AlertRule['op'])}>
            <option value=">">is above</option>
            <option value="<">is below</option>
          </select>
          <input
            value={threshold}
            size={5}
            onChange={(e) => setThreshold(e.target.value)}
            required
          />
          <span className="unit">{unit}</span>
        </span>
      </label>
      <label className="field">
        <span>For</span>
        <span className="inline">
          <input value={forIntervals} size={3} onChange={(e) => setForIntervals(e.target.value)} />
          <span className="unit">consecutive intervals, clearing after</span>
          <input
            value={clearIntervals}
            size={3}
            onChange={(e) => setClearIntervals(e.target.value)}
          />
        </span>
      </label>
      <div className="pill-row">
        <button className="pill accent" type="submit" disabled={!name}>
          Create
        </button>
        <button className="pill" type="button" onClick={onCancel}>
          Cancel
        </button>
      </div>
    </form>
  )
}
