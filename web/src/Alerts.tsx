import { useCallback, useEffect, useState } from 'react'
import {
  createAlertRule,
  deleteAlertRule,
  fetchAlertRules,
  fetchFiringAlerts,
  fetchTargets,
  updateAlertRule,
  type AlertRule,
  type FiringAlert,
  type Target,
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
  const [enabled, setEnabled] = useState(true)
  const [targets, setTargets] = useState<Target[]>([])
  const [error, setError] = useState<string | null>(null)
  const [adding, setAdding] = useState(false)

  const reload = useCallback(async () => {
    try {
      const [r, f, t] = await Promise.all([fetchAlertRules(), fetchFiringAlerts(), fetchTargets()])
      setRules(r)
      setFiring(f.alerts ?? [])
      setEnabled(f.enabled)
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
      {!enabled && (
        <p className="hint">
          Alerting is off: rules are stored but not evaluated. Start smokeng with{' '}
          <code>--alert-webhook URL</code> to enable it.
        </p>
      )}

      <h2 className="section">Firing</h2>
      {firing.length === 0 ? (
        <p className="hint">Nothing is firing.</p>
      ) : (
        <table className="alerts">
          <tbody>
            {firing.map((a) => (
              <tr key={`${a.target}-${a.rule}`}>
                <td>
                  <span className="chip firing">{a.rule}</span>
                </td>
                <td>
                  <code>{a.target}</code>
                </td>
                <td>
                  {a.metric} is {a.value.toFixed(a.metric === 'loss' ? 0 : 2)}
                  {a.metric === 'loss' ? '%' : 'ms'}
                </td>
                <td className="dim">
                  {a.since ? `since ${new Date(a.since * 1000).toLocaleString()}` : ''}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}

      <h2 className="section">Rules</h2>
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
                <td>{r.name}</td>
                <td>
                  <code>{pathOf(r.target_id)}</code>
                </td>
                <td>{r.describes}</td>
                <td className="dim">clears after {r.clear_intervals}</td>
                {isAdmin && (
                  <td>
                    <button onClick={() => void run(() => updateAlertRule(r.id, { enabled: !r.enabled }))}>
                      {r.enabled ? 'Disable' : 'Enable'}
                    </button>
                    <button className="danger" onClick={() => void run(() => deleteAlertRule(r.id))}>
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
          <div className="actions">
            <button onClick={() => setAdding(true)}>Add rule…</button>
          </div>
        ))}
    </>
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
      className="rule-form"
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
      <div className="actions">
        <button type="submit" disabled={!name}>
          Create
        </button>
        <button type="button" onClick={onCancel}>
          Cancel
        </button>
      </div>
    </form>
  )
}
