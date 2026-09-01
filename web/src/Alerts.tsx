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
  fetchSilences,
  createSilence,
  deleteSilence,
  type Silence,
  fetchAlertBaselines,
  captureBaseline,
  clearBaseline,
  type AlertBaseline,
} from './api'
import ShapeOverlay from './ShapeOverlay'

const METRICS: { value: AlertRule['metric']; label: string; unit: string }[] = [
  { value: 'loss', label: 'Packet loss', unit: '%' },
  { value: 'median', label: 'Median RTT', unit: 'ms' },
  { value: 'p95', label: 'p95 RTT', unit: 'ms' },
  { value: 'spread', label: 'Spread (p95 − p5)', unit: 'ms' },
  { value: 'shape', label: 'Shape shift', unit: 'ms' },
  { value: 'bimodality', label: 'Bimodality', unit: '' },
]

const isShapeMetric = (m: AlertRule['metric']) => m === 'shape' || m === 'bimodality'

/**
 * The unit a metric's value is measured in. Not every metric has one: a
 * bimodality coefficient is a bare number between 0 and 1, and a shape rule in
 * auto mode reports a z-score — both read as nonsense with "ms" appended, which
 * is what a blanket "loss is %, everything else is ms" produced.
 */
function metricUnit(metric: string): string {
  switch (metric) {
    case 'loss':
      return '%'
    case 'bimodality':
      return ''
    case 'shape':
      // A tunable shape rule is a distance in ms; an auto one is a z-score. The
      // firing list does not carry the mode, so say neither rather than one that
      // is wrong half the time.
      return ''
    default:
      return 'ms'
  }
}
const REFRESH_MS = 15_000

export default function Alerts({ isAdmin }: { isAdmin: boolean }) {
  const [rules, setRules] = useState<AlertRule[]>([])
  const [firing, setFiring] = useState<FiringAlert[]>([])
  const [delivering, setDelivering] = useState(true)
  const [targets, setTargets] = useState<Target[]>([])
  const [silences, setSilences] = useState<Silence[]>([])
  const [baselines, setBaselines] = useState<AlertBaseline[]>([])
  const [error, setError] = useState<string | null>(null)
  const [adding, setAdding] = useState(false)

  const reload = useCallback(async () => {
    try {
      const [r, f, t, s, b] = await Promise.all([
        fetchAlertRules(),
        fetchFiringAlerts(),
        fetchTargets(),
        fetchSilences(),
        fetchAlertBaselines(),
      ])
      setRules(r)
      setFiring(f.alerts ?? [])
      setDelivering(f.delivering)
      setTargets(t)
      setSilences(s)
      setBaselines(b)
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
              {firing.map((a) => {
                const muted = a.acked || a.silenced
                return (
                  <tr key={`${a.target}-${a.rule}`} className={muted ? 'disabled-row' : undefined}>
                    <td>
                      <span className="dot-label">
                        <span
                          className="dot"
                          style={{ background: muted ? 'var(--dim)' : 'var(--bad)' }}
                        />
                        <span className="chip firing">{a.rule}</span>
                      </span>
                    </td>
                    <td>
                      <code>{a.target}</code>
                    </td>
                    <td>
                      {a.metric} is {a.value.toFixed(a.metric === 'loss' ? 0 : 2)}
                      {metricUnit(a.metric)}
                    </td>
                    <td className="dim">
                      {a.since ? (
                        <>
                          since <span className="mono">{new Date(a.since * 1000).toLocaleString()}</span>
                        </>
                      ) : (
                        ''
                      )}
                      {a.silenced ? (
                        <span className="hint small">
                          {' '}
                          · silenced
                          {a.silenced_until
                            ? ` until ${new Date(a.silenced_until * 1000).toLocaleString()}`
                            : ''}
                        </span>
                      ) : a.acked && a.acked_by ? (
                        <span className="hint small"> · acknowledged by {a.acked_by}</span>
                      ) : (
                        ''
                      )}
                    </td>
                    <td>
                      <span className="pill-row">
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
                        {!a.silenced && (
                          <SilenceQuick
                            onSilence={(duration_s) =>
                              void run(() =>
                                createSilence({
                                  target_id: a.target_id,
                                  rule_id: a.rule_id,
                                  agent_id: a.agent_id,
                                  duration_s,
                                  reason: `silenced from the firing list`,
                                }),
                              )
                            }
                          />
                        )}
                      </span>
                    </td>
                  </tr>
                )
              })}
              {/* The evidence behind a fired shape alert, on its own row: a
                  z-score is a claim, the two distributions are the thing
                  itself. */}
              {firing
                .filter((a) => a.metric === 'shape')
                .map((a) => (
                  <tr key={`ov-${a.target}-${a.rule}`}>
                    <td colSpan={5}>
                      <details>
                        <summary className="hint small">
                          What changed in <code>{a.target}</code>: reference vs current
                        </summary>
                        <ShapeOverlay
                          ruleId={a.rule_id}
                          targetId={a.target_id}
                          agentId={a.agent_id}
                        />
                      </details>
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
                  <td>
                    {r.describes}
                    {r.baseline === 'golden' && <GoldenNote rule={r} baselines={baselines} />}
                  </td>
                  <td className="dim">clears after {r.clear_intervals}</td>
                  {isAdmin && (
                    <td>
                      <button
                        className={r.enabled ? 'pill active' : 'pill'}
                        onClick={() => void run(() => updateAlertRule(r.id, { enabled: !r.enabled }))}
                      >
                        {r.enabled ? 'Disable' : 'Enable'}
                      </button>
                      {r.baseline === 'golden' && (
                        <>
                          <button
                            className="pill"
                            title="Capture the last hour as this rule's reference — what the path looks like when it is healthy"
                            onClick={() => void run(() => captureBaseline(r.id, {}))}
                          >
                            Capture reference
                          </button>
                          {baselines.some((b) => b.rule_id === r.id) && (
                            <button
                              className="pill"
                              title="Drop the captured reference; the rule then has nothing to compare against and stays quiet"
                              onClick={() => void run(() => clearBaseline(r.id))}
                            >
                              Clear
                            </button>
                          )}
                        </>
                      )}
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

      <SilencesSection
        silences={silences}
        targets={targets}
        onCreate={(body) => run(() => createSilence(body))}
        onCancel={(id) => run(() => deleteSilence(id))}
      />

      <AlertHistory />
    </>
  )
}

/**
 * What a golden-baseline rule is actually comparing against. A rule with no
 * captured reference cannot fire, and saying so plainly is better than leaving
 * an operator to wonder why a rule they created never triggers.
 */
function GoldenNote({ rule, baselines }: { rule: AlertRule; baselines: AlertBaseline[] }) {
  const b = baselines.find((x) => x.rule_id === rule.id)
  if (!b) {
    return (
      <span className="hint small warn-line">
        {' '}
        · no reference captured yet, so this rule cannot fire
      </span>
    )
  }
  return (
    <span className="hint small">
      {' '}
      · reference from {new Date(b.from_ts * 1000).toLocaleString()} ({b.intervals} intervals,{' '}
      {b.samples} samples
      {b.captured_by ? `, by ${b.captured_by}` : ''})
    </span>
  )
}

// A compact "silence for…" control on a firing row: one click to reveal a few
// durations, one more to book a silence scoped to exactly that alert.
function SilenceQuick({ onSilence }: { onSilence: (durationS: number) => void }) {
  const [open, setOpen] = useState(false)
  const CHOICES: { label: string; s: number }[] = [
    { label: '1h', s: 3600 },
    { label: '4h', s: 4 * 3600 },
    { label: '24h', s: 24 * 3600 },
  ]
  if (!open) {
    return (
      <button
        className="pill small"
        title="Silence this alert for a while — suppresses delivery and mutes it, without touching the rule"
        onClick={() => setOpen(true)}
      >
        silence…
      </button>
    )
  }
  return (
    <span className="pill-row">
      {CHOICES.map((c) => (
        <button
          key={c.s}
          className="pill small"
          onClick={() => {
            setOpen(false)
            onSilence(c.s)
          }}
        >
          {c.label}
        </button>
      ))}
      <button className="pill small" onClick={() => setOpen(false)}>
        ✕
      </button>
    </span>
  )
}

/**
 * Silences: windows during which matching alerts are suppressed — delivery and
 * attention both — without changing the rule. A silence that starts now is the
 * quick "mute this for a bit"; one that starts later is a maintenance window
 * booked ahead, so planned work does not page.
 */
function SilencesSection({
  silences,
  targets,
  onCreate,
  onCancel,
}: {
  silences: Silence[]
  targets: Target[]
  onCreate: (body: Parameters<typeof createSilence>[0]) => Promise<boolean>
  onCancel: (id: number) => Promise<boolean>
}) {
  const [adding, setAdding] = useState(false)
  const now = Date.now() / 1000
  // Active first, then upcoming, then the spent ones for reference.
  const live = silences.filter((s) => s.active || s.upcoming)
  const past = silences.filter((s) => !s.active && !s.upcoming)

  return (
    <section className="card section-card">
      <div className="section-card-head">
        <span className="section-card-title">Silences &amp; maintenance windows</span>
      </div>
      <p className="hint">
        A silence suppresses delivery and mutes matching alerts over a time window, without
        touching the rule. Scope it to a node and its subtree, or leave it global. A window that
        starts in the future is a maintenance window: planned work stops paging without anyone
        editing a rule.
      </p>

      {live.length === 0 ? (
        <p className="hint">No active or upcoming silences.</p>
      ) : (
        <table className="alerts">
          <tbody>
            {live.map((s) => (
              <tr key={s.id}>
                <td>
                  <span className="dot-label">
                    <span
                      className="dot"
                      style={{ background: s.active ? 'var(--warn)' : 'var(--dim)' }}
                    />
                    <span className="chip">{s.active ? 'active' : 'upcoming'}</span>
                  </span>
                </td>
                <td>
                  <code>{s.target ?? 'everything'}</code>
                  {s.agent_id !== undefined ? <span className="hint small"> · agent {s.agent_id}</span> : ''}
                </td>
                <td className="dim">
                  {s.upcoming && now < s.starts_at ? (
                    <>from <span className="mono">{new Date(s.starts_at * 1000).toLocaleString()}</span> </>
                  ) : null}
                  until <span className="mono">{new Date(s.ends_at * 1000).toLocaleString()}</span>
                </td>
                <td className="hint">{s.reason}</td>
                <td>
                  <button className="pill small danger" onClick={() => void onCancel(s.id)}>
                    cancel
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}

      {past.length > 0 && (
        <details className="hint">
          <summary>{past.length} past silence(s)</summary>
          <table className="alerts">
            <tbody>
              {past.map((s) => (
                <tr key={s.id} className="disabled-row">
                  <td>
                    <code>{s.target ?? 'everything'}</code>
                  </td>
                  <td className="dim mono">{new Date(s.ends_at * 1000).toLocaleString()}</td>
                  <td className="hint">{s.reason}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </details>
      )}

      {adding ? (
        <SilenceForm
          targets={targets}
          onCancel={() => setAdding(false)}
          onSubmit={async (body) => {
            if (await onCreate(body)) setAdding(false)
          }}
        />
      ) : (
        <div className="pill-row">
          <button className="pill accent" onClick={() => setAdding(true)}>
            Schedule a silence…
          </button>
        </div>
      )}
    </section>
  )
}

function SilenceForm({
  targets,
  onSubmit,
  onCancel,
}: {
  targets: Target[]
  onSubmit: (body: Parameters<typeof createSilence>[0]) => void
  onCancel: () => void
}) {
  // "now" mode books a duration from this moment; "window" mode books an
  // explicit from/until, which is how a maintenance window is planned ahead.
  const [mode, setMode] = useState<'now' | 'window'>('now')
  const [scope, setScope] = useState<number>(0) // 0 = everything
  const [durationH, setDurationH] = useState('2')
  const [startsAt, setStartsAt] = useState('')
  const [endsAt, setEndsAt] = useState('')
  const [reason, setReason] = useState('')

  const submit = () => {
    const body: Parameters<typeof createSilence>[0] = { reason }
    if (scope > 0) body.target_id = scope
    if (mode === 'now') {
      body.duration_s = Math.round(Number(durationH) * 3600)
    } else {
      body.starts_at = Math.floor(new Date(startsAt).getTime() / 1000)
      body.ends_at = Math.floor(new Date(endsAt).getTime() / 1000)
    }
    onSubmit(body)
  }

  return (
    <form
      className="card rule-form"
      onSubmit={(e) => {
        e.preventDefault()
        submit()
      }}
    >
      <label className="field">
        <span>Scope</span>
        <select value={scope} onChange={(e) => setScope(Number(e.target.value))}>
          <option value={0}>Everything</option>
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
          <select value={mode} onChange={(e) => setMode(e.target.value as 'now' | 'window')}>
            <option value="now">For a duration from now</option>
            <option value="window">A window (maintenance)</option>
          </select>
        </span>
      </label>
      {mode === 'now' ? (
        <label className="field">
          <span>Duration</span>
          <span className="inline">
            <input value={durationH} size={4} onChange={(e) => setDurationH(e.target.value)} />
            <span className="unit">hours</span>
          </span>
        </label>
      ) : (
        <>
          <label className="field">
            <span>From</span>
            <input
              type="datetime-local"
              value={startsAt}
              onChange={(e) => setStartsAt(e.target.value)}
              required
            />
          </label>
          <label className="field">
            <span>Until</span>
            <input
              type="datetime-local"
              value={endsAt}
              onChange={(e) => setEndsAt(e.target.value)}
              required
            />
          </label>
        </>
      )}
      <label className="field">
        <span>Reason</span>
        <input
          value={reason}
          onChange={(e) => setReason(e.target.value)}
          placeholder="e.g. planned upgrade of the DB cluster"
        />
      </label>
      <div className="pill-row">
        <button className="pill accent" type="submit">
          Schedule
        </button>
        <button className="pill" type="button" onClick={onCancel}>
          Cancel
        </button>
      </div>
    </form>
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
  const [mode, setMode] = useState<'auto' | 'tunable'>('auto')
  const [baseline, setBaseline] = useState<'rolling' | 'golden'>('rolling')
  const shape = isShapeMetric(metric)
  const unit = metric === 'bimodality' ? '' : 'ms'

  return (
    <form
      className="card rule-form"
      onSubmit={(e) => {
        e.preventDefault()
        if (shape) {
          // A shape rule always fires on rising anomaly. In auto mode the
          // threshold is a built-in cutoff the operator does not set: a z-score
          // of 4 for a shift, the 0.6 bimodality mark for two clusters.
          const auto = mode === 'auto'
          onSubmit({
            name,
            target_id: targetId,
            metric,
            op: '>',
            mode,
            baseline: metric === 'shape' ? baseline : '',
            threshold: auto
              ? metric === 'bimodality'
                ? 0.6
                : 4
              : Number(threshold),
            for_intervals: Number(forIntervals),
            clear_intervals: Number(clearIntervals),
          })
          return
        }
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
          {shape ? (
            <>
              <select value={mode} onChange={(e) => setMode(e.target.value as 'auto' | 'tunable')}>
                <option value="auto">auto (self-calibrated)</option>
                <option value="tunable">tunable</option>
              </select>
              {mode === 'tunable' && (
                <>
                  <span className="unit">&gt;</span>
                  <input
                    value={threshold}
                    size={5}
                    onChange={(e) => setThreshold(e.target.value)}
                    required
                  />
                  <span className="unit">{unit}</span>
                </>
              )}
            </>
          ) : (
            <>
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
            </>
          )}
        </span>
      </label>
      {metric === 'shape' && (
        <label className="field">
          <span>Compared against</span>
          <span className="inline">
            <select
              value={baseline}
              onChange={(e) => setBaseline(e.target.value as 'rolling' | 'golden')}
            >
              <option value="rolling">its own recent history (rolling)</option>
              <option value="golden">a captured reference (golden)</option>
            </select>
          </span>
        </label>
      )}
      {shape && (
        <p className="hint small">
          {metric === 'shape'
            ? baseline === 'golden'
              ? 'Fires when the distribution drifts from a reference you capture once — "this is what good looks like". Capture it from the rule list after creating the rule; until then the rule has nothing to compare against and stays quiet.'
              : 'Fires when the distribution shifts from its recent history (Wasserstein distance). Auto scores the shift against the series’ own recent variability — no threshold to tune.'
            : 'Fires when the distribution splits into two clusters — a load-balanced or flapping path. Auto uses the 0.6 bimodality mark.'}
        </p>
      )}
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
