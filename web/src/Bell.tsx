import { useEffect, useState } from 'react'
import { fetchFiringAlerts, type FiringAlert } from './api'

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
  const [enabled, setEnabled] = useState(true)

  useEffect(() => {
    let cancelled = false
    const poll = () => {
      fetchFiringAlerts()
        .then((r) => {
          if (cancelled) return
          setAlerts(r.alerts ?? [])
          setEnabled(r.enabled)
        })
        // A failed poll leaves the last known state rather than clearing it.
        // Showing "all quiet" because the request failed would be the one
        // wrong answer this control can give.
        .catch(() => undefined)
    }
    poll()
    const t = setInterval(poll, 30_000)
    return () => {
      cancelled = true
      clearInterval(t)
    }
  }, [])

  return (
    <div className="bell">
      <button
        className="icon-button"
        title={alerts.length ? `${alerts.length} firing` : 'Firing alerts'}
        aria-label={alerts.length ? `${alerts.length} alerts firing` : 'Firing alerts'}
        aria-expanded={open}
        onClick={() => onOpenChange(!open)}
      >
        ▲
        {alerts.length > 0 && <span className="bell-badge">{alerts.length}</span>}
      </button>
      {open && (
        <div className="popover bell-popover">
          <p className="popover-title">Firing now</p>
          {!enabled ? (
            <p className="hint small">
              Rules are stored but not evaluated — no <code>--alert-webhook</code> is set.
            </p>
          ) : alerts.length === 0 ? (
            <p className="hint small">All quiet — nothing is firing.</p>
          ) : (
            alerts.map((a, i) => (
              <button key={`${a.rule}-${a.target}-${i}`} className="bell-row" onClick={onOpenAlerts}>
                <span className="bell-rule">
                  <span className="dot firing" />
                  {a.rule} — {a.describes}
                </span>
                <span className="hint small mono">
                  {a.target}
                  {a.since ? ` · since ${new Date(a.since * 1000).toLocaleTimeString()}` : ''}
                </span>
              </button>
            ))
          )}
        </div>
      )}
    </div>
  )
}
