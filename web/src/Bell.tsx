import { useEffect, useState } from 'react'
import { acknowledgeAlert, fetchFiringAlerts, type FiringAlert } from './api'

/**
 * What is firing, reachable from every page.
 *
 * The Alerts page already lists these, but an operator who is looking at a
 * graph should not have to go and ask whether anything is wrong — an alert
 * nobody navigated to is an alert nobody saw. The count sits on the icon so
 * the answer is visible without opening anything.
 *
 * The badge is absent rather than "0" when nothing is firing: a zero is a
 * number to read, and the useful state is the one that draws no attention at
 * all.
 */
export default function Bell({
  open,
  onOpenChange,
  onOpenAlerts,
}: {
  open: boolean
  onOpenChange: (open: boolean) => void
  onOpenAlerts: () => void
}) {
  const [alerts, setAlerts] = useState<FiringAlert[]>([])
  // Whether transitions are posted anywhere, not whether they are evaluated:
  // rules are always evaluated, and the firing list below is proof of it. The
  // only thing a missing webhook changes is that nothing is posted onward.
  const [delivering, setDelivering] = useState(true)

  const poll = () =>
    fetchFiringAlerts()
      .then((r) => {
        setAlerts(r.alerts ?? [])
        setDelivering(r.delivering)
      })
      // A failed poll leaves the last known state rather than clearing it.
      // Showing "all quiet" because the request failed would be the one
      // wrong answer this control can give.
      .catch(() => undefined)

  useEffect(() => {
    let cancelled = false
    const tick = () => {
      if (!cancelled) void poll()
    }
    tick()
    const t = setInterval(tick, 30_000)
    return () => {
      cancelled = true
      clearInterval(t)
    }
    // poll closes over only setState setters, which are stable.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  const ack = async (a: FiringAlert) => {
    await acknowledgeAlert(a, !a.acked)
    await poll() // reflect the new state at once rather than waiting 30s
  }

  // The badge counts what still needs attention: alerts that are neither
  // acknowledged nor silenced. Both are still firing and still listed, just not
  // shouting.
  const unacked = alerts.filter((a) => !a.acked && !a.silenced).length

  return (
    <div className="bell">
      <button
        className="icon-button"
        title={unacked ? `${unacked} firing` : 'Firing alerts'}
        aria-label={unacked ? `${unacked} alerts firing` : 'Firing alerts'}
        aria-expanded={open}
        onClick={() => onOpenChange(!open)}
      >
        ▲
        {unacked > 0 && <span className="bell-badge">{unacked}</span>}
      </button>
      {open && (
        <div className="popover bell-popover">
          <p className="popover-title">Firing now</p>
          {!delivering && (
            <p className="hint small">
              Evaluated and recorded, but not posted anywhere: no <code>--alert-webhook</code>.
            </p>
          )}
          {alerts.length === 0 ? (
            <p className="hint small">All quiet — nothing is firing.</p>
          ) : (
            alerts.map((a, i) => {
              const muted = a.acked || a.silenced
              return (
                <div key={`${a.rule}-${a.target}-${i}`} className={muted ? 'bell-item acked' : 'bell-item'}>
                  <button className="bell-row" onClick={onOpenAlerts}>
                    <span className="bell-rule">
                      <span className={muted ? 'dot' : 'dot firing'} />
                      {a.rule} — {a.describes}
                    </span>
                    <span className="hint small mono">
                      {a.target}
                      {a.since ? ` · since ${new Date(a.since * 1000).toLocaleTimeString()}` : ''}
                      {a.silenced ? ' · silenced' : a.acked && a.acked_by ? ` · ack ${a.acked_by}` : ''}
                    </span>
                  </button>
                  {/* A silence is managed on the Alerts page, so the bell only
                      offers the per-episode acknowledge. */}
                  {!a.silenced && (
                    <button
                      className="pill small"
                      title={a.acked ? 'Un-acknowledge' : 'Acknowledge — still fires, just stops nagging'}
                      onClick={() => void ack(a)}
                    >
                      {a.acked ? 'unack' : 'ack'}
                    </button>
                  )}
                </div>
              )
            })
          )}
        </div>
      )}
    </div>
  )
}
