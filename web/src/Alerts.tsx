import { useCallback, useEffect, useId, useRef, useState } from 'react'
import { ConfirmButton, useAction, useFormError, useLiveRefresh, useParkedFocus, LivePill, StatusDot, ErrorText } from './shared'
import { afterName, alertKey, alertName, seriesLabel } from './util'
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
import { fmtLoss } from './format'
import ShapeOverlay from './ShapeOverlay'

const METRICS: { value: AlertRule['metric']; label: string }[] = [
  { value: 'loss', label: 'Packet loss' },
  { value: 'median', label: 'Median RTT' },
  { value: 'p95', label: 'p95 RTT' },
  { value: 'spread', label: 'Spread (p95 − p5)' },
  { value: 'shape', label: 'Shape shift' },
  { value: 'bimodality', label: 'Bimodality' },
]

const isShapeMetric = (m: AlertRule['metric']) => m === 'shape' || m === 'bimodality'

/**
 * The unit a metric's value is measured in. Not every metric has one: a
 * bimodality coefficient is a bare number between 0 and 1, and a shape rule in
 * auto mode reports a z-score — both read as nonsense with "ms" appended, which
 * is what a blanket "loss is %, everything else is ms" produced.
 */
// A tunable shape rule is a distance in ms; an auto one is a z-score. The firing
// list does not carry the mode, so say neither rather than one that is wrong
// half the time.
const metricUnit = (metric: string) =>
  metric === 'bimodality' || metric === 'shape' ? '' : 'ms'
const REFRESH_MS = 15_000

export default function Alerts({ isAdmin }: { isAdmin: boolean }) {
  const [rules, setRules] = useState<AlertRule[]>([])
  const [firing, setFiring] = useState<FiringAlert[]>([])
  const [delivering, setDelivering] = useState(true)
  const [targets, setTargets] = useState<Target[]>([])
  const [silences, setSilences] = useState<Silence[]>([])
  const [baselines, setBaselines] = useState<AlertBaseline[]>([])
  const [adding, setAdding] = useState(false)
  // The form replaces the button that opened it; give focus back when it closes.
  const addButton = useRef<HTMLButtonElement>(null)
  const wasAdding = useRef(false)
  useEffect(() => {
    if (wasAdding.current && !adding) addButton.current?.focus()
    wasAdding.current = adding
  }, [adding])

  const load = useCallback(async () => {
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
  }, [])
  const { error, busy, reload, run, attempt } = useAction(load)

  const refresh = useCallback(() => void reload(), [reload])
  const { live, toggleLive } = useLiveRefresh(refresh, REFRESH_MS)

  const ackAlert = (a: FiringAlert) =>
    run(() => acknowledgeAlert(a, !a.acked), `${a.acked ? 'Unacknowledged' : 'Acknowledged'} ${alertName(a)}`)

  const pathOf = (id: number) => targets.find((t) => t.id === id)?.path ?? `target ${id}`

  return (
    <>
      <h1 className="visually-hidden">Alerts</h1>
      {error && <ErrorText>{error}</ErrorText>}
      {!delivering && (
        <p className="hint warn">
          Rules are evaluated and their transitions recorded, but nothing is posted anywhere:
          no <code>--alert-webhook</code> is set. Firing state and the history below are still
          live.
        </p>
      )}

      <section className="card section-card">
        <div className="section-card-head">
          <h2 className="section-card-title">Firing</h2>
          <span className="spacer" />
          <LivePill live={live} onToggle={toggleLive} />
        </div>
        {firing.length === 0 ? (
          <p className="hint">Nothing is firing.</p>
        ) : (
          <table className="alerts">
            <caption className="visually-hidden">Firing alerts</caption>
            <thead className="visually-hidden">
              <tr>
                <th scope="col">Alert</th>
                <th scope="col">Target</th>
                <th scope="col">Condition</th>
                <th scope="col">Since</th>
                <th scope="col">Actions</th>
              </tr>
            </thead>
            <tbody>
              {firing.map((a) => {
                const muted = a.acked || a.silenced
                return (
                  <tr key={alertKey(a)} className={muted ? 'disabled-row' : undefined}>
                    <td>
                      <span className="dot-label">
                        <StatusDot
                          background={muted ? 'var(--dim)' : 'var(--bad)'}
                          label={muted ? 'acknowledged or silenced' : 'firing'}
                        />
                        <span className="chip firing">{a.rule}</span>
                      </span>
                    </td>
                    <td>
                      <code>{a.target}</code>
                      <span className="hint small" style={{ whiteSpace: 'nowrap' }}>
                        {afterName(seriesLabel(a.agent, a.family))}
                      </span>
                    </td>
                    <td>
                      {a.metric} is{' '}
                      {a.metric === 'loss' ? fmtLoss(a.value) : `${a.value.toFixed(2)}${metricUnit(a.metric)}`}
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
                              ? 'Unacknowledge'
                              : 'Acknowledge — the alert keeps firing, it just stops demanding attention'
                          }
                          aria-label={`${a.acked ? 'Unacknowledge' : 'Acknowledge'} ${alertName(a)}`}
                          onClick={() => void ackAlert(a)}
                        >
                          {a.acked ? 'unacknowledge' : 'acknowledge'}
                        </button>
                        {!a.silenced && (
                          <SilenceQuick
                            label={alertName(a)}
                            onSilence={(duration_s) =>
                              void run(() =>
                                createSilence({
                                  target_id: a.target_id,
                                  rule_id: a.rule_id,
                                  agent_id: a.agent_id,
                                  family: a.family,
                                  duration_s,
                                  reason: `silenced from the firing list`,
                                }),
                                `Silenced ${alertName(a)}`,
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
                  <tr key={`ov-${alertKey(a)}`}>
                    <td colSpan={5}>
                      <details>
                        <summary className="hint small">
                          What changed in <code>{a.target}</code>: reference vs current
                        </summary>
                        <ShapeOverlay
                          ruleId={a.rule_id}
                          targetId={a.target_id}
                          agentId={a.agent_id}
                          family={a.family}
                        />
                      </details>
                    </td>
                  </tr>
                ))}
            </tbody>
          </table>
        )}
        {firing.length > 0 && (
          <p className="hint small">
            Acknowledging marks an alert as seen: it keeps firing and keeps being delivered, but
            stops asking for attention here, until it clears. If it fires again, it asks again.
            Silencing stops its delivery for a while; the rule is not changed.
          </p>
        )}
      </section>

      <section className="card section-card">
        <div className="section-card-head">
          <h2 className="section-card-title">Rules</h2>
        </div>
        <p className="hint">
          A rule applies to the node it is defined on, a group or a target, and to everything
          below it. A node that has rules of its own uses those instead of the rules it would
          inherit; they are not added together. A rule fires only after its condition has held
          for as many intervals in a row as the rule asks, and clears only after the condition
          has failed for as many as it asks for clearing: two numbers, three each unless you
          change them. With more than one, a single bad interval pages nobody.
        </p>
        {rules.length === 0 && <p className="hint">No rules defined.</p>}
        {rules.length > 0 && (
          <table className="alerts">
            <caption className="visually-hidden">Alert rules</caption>
            <thead className="visually-hidden">
              <tr>
                <th scope="col">Rule</th>
                <th scope="col">Target</th>
                <th scope="col">Condition</th>
                <th scope="col">Clearing</th>
                {isAdmin && <th scope="col">Actions</th>}
              </tr>
            </thead>
            <tbody>
              {rules.map((r) => (
                <tr key={r.id} className={r.enabled ? '' : 'disabled-row'}>
                  <td>
                    <span className="dot-label">
                      <StatusDot background={r.enabled ? 'var(--good)' : 'var(--warn)'} label={r.enabled ? 'enabled' : 'disabled'} />
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
                  <td className="dim">
                    clears after {r.clear_intervals} · {r.severity}
                  </td>
                  {isAdmin && (
                    <td>
                      <button
                        className="pill"
                        aria-label={`${r.enabled ? 'Disable' : 'Enable'} ${r.name}`}
                        onClick={() => void run(() => updateAlertRule(r.id, { enabled: !r.enabled }), `Rule ${r.name} ${r.enabled ? 'disabled' : 'enabled'}`)}
                      >
                        {r.enabled ? 'Disable' : 'Enable'}
                      </button>
                      {r.baseline === 'golden' && (
                        <>
                          <button
                            className="pill"
                            aria-label={`Capture reference for ${r.name}`}
                            title="Capture the last hour as this rule's reference — what the path looks like when it is healthy"
                            onClick={() => void run(() => captureBaseline(r.id, {}), `Reference captured for rule ${r.name}`)}
                          >
                            Capture reference
                          </button>
                          {baselines.some((b) => b.rule_id === r.id) && (
                            <ConfirmButton
                              busy={busy}
                              label="Clear"
                              confirmLabel="Confirm clear reference"
                              subject={`reference of ${r.name}`}
                              confirmSubject={`of ${r.name}`}
                              onConfirm={() => run(() => clearBaseline(r.id), `Reference cleared for rule ${r.name}`)}
                            />
                          )}
                        </>
                      )}
                      <ConfirmButton busy={busy} label="Delete" subject={r.name} onConfirm={() => run(() => deleteAlertRule(r.id), `Rule ${r.name} deleted`)} />
                    </td>
                  )}
                </tr>
              ))}
            </tbody>
          </table>
        )}
        {isAdmin && rules.some((r) => r.baseline === 'golden') && (
          <p className="hint small">
            Capture reference stores the last hour as what the path looks like when it is healthy.
            Clear drops that reference; the rule then has nothing to compare with and stays quiet.
          </p>
        )}

        {isAdmin &&
          (adding ? (
            <RuleForm
              targets={targets}
              onCancel={() => setAdding(false)}
              onSubmit={async (body) => {
                const err = await attempt(() => createAlertRule(body), `Rule ${body.name} created`)
                if (!err) setAdding(false)
                return err
              }}
            />
          ) : (
            <div className="pill-row">
              <button ref={addButton} className="pill accent" onClick={() => setAdding(true)}>
                Add rule…
              </button>
            </div>
          ))}
      </section>

      <SilencesSection
        silences={silences}
        targets={targets}
        onCreate={(body) => attempt(() => createSilence(body), 'Silence created')}
        onCancel={(id) => run(() => deleteSilence(id), 'Silence cancelled')}
        busy={busy}
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
function SilenceQuick({
  label,
  onSilence,
}: {
  label: string
  onSilence: (durationS: number) => void
}) {
  const [open, setOpen] = useState(false)
  const firstChoice = useRef<HTMLButtonElement>(null)
  const openButton = useRef<HTMLButtonElement>(null)
  const moved = useRef(false)
  // Each swap removes the button that had focus; put focus on its replacement,
  // but not on first render.
  useEffect(() => {
    if (moved.current) (open ? firstChoice : openButton).current?.focus()
    moved.current = true
  }, [open])
  // A silence that goes through takes this control out of the row.
  const acted = useParkedFocus()
  const CHOICES: { label: string; s: number }[] = [
    { label: '1h', s: 3600 },
    { label: '4h', s: 4 * 3600 },
    { label: '24h', s: 24 * 3600 },
  ]
  if (!open) {
    return (
      <button
        ref={openButton}
        className="pill small"
        title="Silence this alert for a while — suppresses delivery and mutes it, without touching the rule"
        aria-label={`Silence ${label}`}
        onClick={() => setOpen(true)}
      >
        silence…
      </button>
    )
  }
  return (
    <span className="pill-row">
      {CHOICES.map((c, i) => (
        <button
          key={c.s}
          ref={i === 0 ? firstChoice : undefined}
          className="pill small"
          aria-label={`Silence ${label} for ${c.label}`}
          onClick={() => {
            acted.current = true
            setOpen(false)
            onSilence(c.s)
          }}
        >
          {c.label}
        </button>
      ))}
      <button className="pill small" aria-label={`Cancel silence for ${label}`} onClick={() => setOpen(false)}>
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
  busy,
}: {
  silences: Silence[]
  targets: Target[]
  busy: boolean
  onCreate: (body: Parameters<typeof createSilence>[0]) => Promise<string | null>
  onCancel: (id: number) => Promise<boolean>
}) {
  const [adding, setAdding] = useState(false)
  // The form replaces the button that opened it; give focus back when it closes.
  const addButton = useRef<HTMLButtonElement>(null)
  const wasAdding = useRef(false)
  useEffect(() => {
    if (wasAdding.current && !adding) addButton.current?.focus()
    wasAdding.current = adding
  }, [adding])
  const now = Date.now() / 1000
  // Active first, then upcoming, then the spent ones for reference.
  const live = silences.filter((s) => s.active || s.upcoming)
  const past = silences.filter((s) => !s.active && !s.upcoming)

  return (
    <section className="card section-card">
      <div className="section-card-head">
        <h2 className="section-card-title">Silences &amp; maintenance windows</h2>
      </div>
      <p className="hint">
        A silence stops matching alerts from being delivered during a time window and shows
        them as muted. The rule itself is not changed. Scope it to a node, which covers
        everything below that node too, or leave it global to cover every target. A silence
        that starts in the future is a maintenance window: planned work pages nobody, and
        nobody has to edit a rule.
      </p>

      {live.length === 0 ? (
        <p className="hint">No active or upcoming silences.</p>
      ) : (
        <table className="alerts">
          <caption className="visually-hidden">Silences and maintenance windows</caption>
          <thead className="visually-hidden">
            <tr>
              <th scope="col">Status</th>
              <th scope="col">Scope</th>
              <th scope="col">Period</th>
              <th scope="col">Reason</th>
              <th scope="col">Actions</th>
            </tr>
          </thead>
          <tbody>
            {live.map((s) => (
              <tr key={s.id}>
                <td>
                  <span className="dot-label">
                    <span
                      className="dot"
                      style={{ background: s.active ? 'var(--warn)' : 'var(--dim)' }}
                      aria-hidden="true"
                    />
                    <span className="chip">{s.active ? 'active' : 'upcoming'}</span>
                  </span>
                </td>
                <td>
                  <code>{s.target ?? 'everything'}</code>
                  {s.agent_id !== undefined ? <span className="hint small"> · agent {s.agent_id}</span> : ''}
                  {s.family ? <span className="hint small"> · {s.family}</span> : ''}
                </td>
                <td className="dim">
                  {s.upcoming && now < s.starts_at ? (
                    <>from <span className="mono">{new Date(s.starts_at * 1000).toLocaleString()}</span> </>
                  ) : null}
                  until <span className="mono">{new Date(s.ends_at * 1000).toLocaleString()}</span>
                </td>
                <td className="hint">{s.reason}</td>
                <td>
                  <ConfirmButton
                    className="pill small"
                    busy={busy}
                    label="cancel"
                    subject={`silence on ${s.target ?? 'everything'}`}
                    confirmSubject={`on ${s.target ?? 'everything'}`}
                    confirmLabel="Confirm cancel silence"
                    onConfirm={() => onCancel(s.id)}
                  />
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
            <caption className="visually-hidden">Past silences</caption>
            <thead className="visually-hidden">
              <tr>
                <th scope="col">Scope</th>
                <th scope="col">Ended</th>
                <th scope="col">Reason</th>
              </tr>
            </thead>
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
            const err = await onCreate(body)
            if (!err) setAdding(false)
            return err
          }}
        />
      ) : (
        <div className="pill-row">
          <button ref={addButton} className="pill accent" onClick={() => setAdding(true)}>
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
  onSubmit: (body: Parameters<typeof createSilence>[0]) => Promise<string | null>
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
  const { error, setError, errorProps, describedBy, onChange, field } = useFormError('silence')

  const submit = async () => {
    const body: Parameters<typeof createSilence>[0] = { reason }
    if (scope > 0) body.target_id = scope
    if (mode === 'now') {
      body.duration_s = Math.round(Number(durationH) * 3600)
    } else {
      body.starts_at = Math.floor(new Date(startsAt).getTime() / 1000)
      body.ends_at = Math.floor(new Date(endsAt).getTime() / 1000)
    }
    setError(null)
    setError(await onSubmit(body))
  }

  return (
    <form
      className="card rule-form"
      onChange={onChange}
      onSubmit={(e) => {
        e.preventDefault()
        submit()
      }}
    >
      <label className="field">
        <span>Scope</span>
        <select autoFocus value={scope} onChange={(e) => setScope(Number(e.target.value))}>
          <option value={0}>Everything</option>
          <TargetOptions targets={targets} />
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
          <span>Duration <span className="note">(required)</span></span>
          <span className="inline">
            {/* 0 passes here and the server says why not; a positive floor would
                be stricter than the server, which takes any duration of a second. */}
            <input
              type="number"
              min="0"
              step="any"
              required
              value={durationH}
              onChange={(e) => setDurationH(e.target.value)}
              {...field('end')}
            />
            <span className="unit">hours</span>
          </span>
        </label>
      ) : (
        <>
          <label className="field">
            <span>From <span className="note">(required)</span></span>
            <input
              type="datetime-local"
              value={startsAt}
              onChange={(e) => setStartsAt(e.target.value)}
              required
            />
          </label>
          <label className="field">
            <span>Until <span className="note">(required)</span></span>
            <input
              type="datetime-local"
              value={endsAt}
              min={startsAt || undefined}
              onChange={(e) => setEndsAt(e.target.value)}
              required
              {...field('end')}
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
      {error && <ErrorText {...errorProps}>{error}</ErrorText>}
      <div className="pill-row">
        <button className="pill accent" type="submit" aria-describedby={describedBy}>
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
    <section className="card section-card" aria-busy={!loaded}>
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
          <caption className="visually-hidden">Alert history</caption>
          <thead className="visually-hidden">
            <tr>
              <th scope="col">State</th>
              <th scope="col">Rule</th>
              <th scope="col">Condition</th>
              <th scope="col">Target</th>
              <th scope="col">Time</th>
            </tr>
          </thead>
          <tbody>
            {events.map((e) => (
              <tr key={e.id}>
                <td>
                  <span className="dot-label">
                    <span
                      className="dot"
                      style={{ background: e.firing ? 'var(--bad)' : 'var(--good)' }}
                      aria-hidden="true"
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
                  <span className="hint small">{afterName(e.family)}</span>
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
  onSubmit: (body: Partial<AlertRule>) => Promise<string | null>
  onCancel: () => void
}) {
  const id = useId()
  const { error, setError, errorProps, describedBy, onChange, field } = useFormError('rule')
  const [name, setName] = useState('')
  const [targetId, setTargetId] = useState<number>(targets[0]?.id ?? 1)
  const [metric, setMetric] = useState<AlertRule['metric']>('loss')
  const [op, setOp] = useState<AlertRule['op']>('>')
  const [threshold, setThreshold] = useState('20')
  const [forIntervals, setForIntervals] = useState('3')
  const [clearIntervals, setClearIntervals] = useState('3')
  const [mode, setMode] = useState<'auto' | 'tunable'>('auto')
  const [baseline, setBaseline] = useState<'rolling' | 'golden'>('rolling')
  const [severity, setSeverity] = useState<AlertRule['severity']>('warning')
  const shape = isShapeMetric(metric)
  const unit = metric === 'bimodality' ? '' : metric === 'loss' ? '%' : 'ms'

  return (
    <form
      className="card rule-form"
      onChange={onChange}
      onSubmit={(e) => {
        e.preventDefault()
        setError(null)
        // A shape rule always fires on rising anomaly. In auto mode the
        // threshold is a built-in cutoff the operator does not set: a z-score
        // of 4 for a shift, the 0.6 bimodality mark for two clusters.
        const auto = shape && mode === 'auto'
        void onSubmit({
          name,
          target_id: targetId,
          metric,
          op: shape ? '>' : op,
          ...(shape && { mode, baseline: metric === 'shape' ? baseline : '' }),
          threshold: auto ? (metric === 'bimodality' ? 0.6 : 4) : Number(threshold),
          for_intervals: Number(forIntervals),
          clear_intervals: Number(clearIntervals),
          severity,
        }).then(setError)
      }}
    >
      <label className="field">
        <span>Name <span className="note">(required)</span></span>
        <input value={name} autoFocus onChange={(e) => setName(e.target.value)} required {...field('name')} />
      </label>
      <label className="field">
        <span>Applies to</span>
        <select value={targetId} onChange={(e) => setTargetId(Number(e.target.value))}>
          <TargetOptions targets={targets} />
        </select>
      </label>
      <div className="field" role="group" aria-labelledby={`${id}-when`}>
        <span id={`${id}-when`}>
          When {(!shape || mode === 'tunable') && <span className="note">(required)</span>}
        </span>
        <span className="inline">
          <select aria-label="Metric" value={metric} onChange={(e) => setMetric(e.target.value as AlertRule['metric'])}>
            {METRICS.map((m) => (
              <option key={m.value} value={m.value}>
                {m.label}
              </option>
            ))}
          </select>
          {shape ? (
            <>
              <select
                aria-label="Threshold mode"
                value={mode}
                onChange={(e) => setMode(e.target.value as 'auto' | 'tunable')}
              >
                <option value="auto">auto (self-calibrated)</option>
                <option value="tunable">tunable</option>
              </select>
              {mode === 'tunable' && (
                <>
                  <span className="unit">&gt;</span>
                  <input
                    aria-label="Threshold"
                    type="number"
                    step="any"
                    value={threshold}
                    onChange={(e) => setThreshold(e.target.value)}
                    required
                    {...field('threshold')}
                  />
                  <span className="unit">{unit}</span>
                </>
              )}
            </>
          ) : (
            <>
              <select
                aria-label="Condition"
                value={op}
                onChange={(e) => setOp(e.target.value as AlertRule['op'])}
              >
                <option value=">">is above</option>
                <option value="<">is below</option>
              </select>
              <input
                aria-label="Threshold"
                type="number"
                step="any"
                min={metric === 'loss' ? 0 : undefined}
                max={metric === 'loss' ? 100 : undefined}
                value={threshold}
                onChange={(e) => setThreshold(e.target.value)}
                required
                {...field('threshold')}
              />
              <span className="unit">{unit}</span>
            </>
          )}
        </span>
      </div>
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
              ? 'Fires when the distribution drifts away from a reference that you capture once: "this is what good looks like". Capture the reference from the rule list after you create the rule. Until then the rule has nothing to compare with and stays quiet.'
              : 'Fires when the distribution shifts away from its own recent history. The shift is the Wasserstein distance: the average distance the measurements would have to move to turn one distribution into the other. Auto scores that shift against how much the series normally varies, so there is no threshold to tune.'
            : 'Fires when the distribution splits into two clusters, as on a path that is load-balanced over two routes or that flaps between them. Auto uses the mark of 0.6 on the bimodality coefficient, which runs from 0 to 1.'}
        </p>
      )}
      <div className="field" role="group" aria-labelledby={`${id}-for`}>
        <span id={`${id}-for`}>For <span className="note">(required)</span></span>
        <span className="inline">
          <input
            aria-label="Consecutive intervals before firing"
            type="number"
            min="1"
            required
            value={forIntervals}
            onChange={(e) => setForIntervals(e.target.value)}
            {...field('for')}
          />
          <span className="unit">consecutive intervals, clearing after</span>
          <input
            aria-label="Intervals before clearing"
            type="number"
            min="1"
            required
            value={clearIntervals}
            onChange={(e) => setClearIntervals(e.target.value)}
            {...field('clear')}
          />
        </span>
      </div>
      <label className="field">
        <span>Severity</span>
        <select
          value={severity}
          onChange={(e) => setSeverity(e.target.value as AlertRule['severity'])}
        >
          <option value="critical">critical</option>
          <option value="warning">warning</option>
          <option value="info">info</option>
        </select>
      </label>
      {error && <ErrorText {...errorProps}>{error}</ErrorText>}
      <div className="pill-row">
        <button className="pill accent" type="submit" aria-describedby={describedBy}>
          Create
        </button>
        <button className="pill" type="button" onClick={onCancel}>
          Cancel
        </button>
      </div>
    </form>
  )
}

function TargetOptions({ targets }: { targets: Target[] }) {
  return targets.map((t) => (
    <option key={t.id} value={t.id}>
      {t.path === '/' ? '/ (everything)' : t.path}
    </option>
  ))
}
