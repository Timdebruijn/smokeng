import { useCallback, useEffect, useMemo, useState } from 'react'
import Admin from './Admin'
import Agents from './Agents'
import Alerts from './Alerts'
import Grants from './Grants'
import Plot from './Plot'
import { fetchAgents, fetchMe, fetchTargets, logout, type AgentInfo, type Me, type Target } from './api'

const RANGES: { label: string; seconds: number }[] = [
  { label: '15m', seconds: 15 * 60 },
  { label: '1h', seconds: 3600 },
  { label: '6h', seconds: 6 * 3600 },
  { label: '24h', seconds: 24 * 3600 },
]
const REFRESH_MS = 10_000

type View = 'graphs' | 'targets' | 'alerts' | 'agents' | 'access'

export default function App() {
  const [view, setView] = useState<View>('graphs')
  const [userMenuOpen, setUserMenuOpen] = useState(false)
  const [me, setMe] = useState<Me | null>(null)

  useEffect(() => {
    fetchMe()
      .then(setMe)
      .catch(() => setMe(null))
  }, [])

  // Until we know otherwise, assume the least: showing controls that turn out
  // to be refused is worse than showing them a moment late.
  const isAdmin = me?.role === 'admin'
  const needsLogin = me?.auth_enabled === true && !me.authenticated
  // Grant management is global-admin only, both here and on the server — a
  // viewer must not even see the tab exists.
  const views: View[] = isAdmin
    ? ['graphs', 'targets', 'alerts', 'agents', 'access']
    : ['graphs', 'targets', 'alerts', 'agents']

  if (needsLogin) return <SignIn />

  return (
    <>
      <AppHeader
        me={me}
        view={view}
        views={views}
        onView={setView}
        userMenuOpen={userMenuOpen}
        onToggleUser={() => setUserMenuOpen((v) => !v)}
      />
      <main>
      {view === 'graphs' ? (
        <Graphs />
      ) : view === 'targets' ? (
        <Admin readOnly={!isAdmin} />
      ) : view === 'alerts' ? (
        <Alerts isAdmin={isAdmin} />
      ) : view === 'agents' ? (
        <Agents readOnly={!isAdmin} />
      ) : (
        <Grants
            readOnly={!isAdmin}
            defaultRole={me?.default_role}
            authEnabled={me?.auth_enabled !== false}
          />
      )}
      </main>
    </>
  )
}

// The sign-in screen is a screen, not a sentence in the middle of the app: it
// is the whole page for someone who cannot see anything yet.
function SignIn() {
  return (
    <main>
      <section className="signin">
        <div className="card signin-card">
          <span className="brand-mark large">s</span>
          <h1>Sign in to smokeng</h1>
          <p className="hint">
            Latency monitoring with the full RTT distribution, at full resolution, forever. Sign
            in with your organisation account to see measurements.
          </p>
          <a className="button primary block" href="/auth/login">
            Continue with OIDC
          </a>
          <p className="hint small">
            What you may see comes from your identity provider and the grants an admin has
            written.
          </p>
        </div>
      </section>
    </main>
  )
}

function AppHeader({
  me,
  view,
  views,
  onView,
  userMenuOpen,
  onToggleUser,
}: {
  me: Me | null
  view: View
  views: View[]
  onView: (v: View) => void
  userMenuOpen: boolean
  onToggleUser: () => void
}) {
  const who = me?.name ?? me?.email ?? me?.subject ?? ''
  return (
    <header className="appbar">
      <div className="appbar-inner">
        <button className="brand" onClick={() => onView('graphs')}>
          <span className="brand-mark">s</span>
          <span className="brand-name">smokeng</span>
        </button>
        <nav className="tabs">
          {views.map((v) => (
            <button key={v} className={view === v ? 'tab active' : 'tab'} onClick={() => onView(v)}>
              {v[0].toUpperCase() + v.slice(1)}
            </button>
          ))}
        </nav>
        <span className="spacer" />
        {me?.auth_enabled && me.authenticated && (
          <div className="usermenu">
            <button className="chip-button" onClick={onToggleUser}>
              <span className="avatar">{(who || '?').slice(0, 1).toUpperCase()}</span>
              <span className="who">{who}</span>
              <span className="role-tag">{me.role}</span>
            </button>
            {userMenuOpen && (
              <div className="popover">
                <p className="popover-title">{me.email ?? me.subject}</p>
                <p className="hint small">Signed in via OIDC · {me.role}</p>
                <button
                  className="popover-item"
                  onClick={() => void logout().then(() => location.reload())}
                >
                  Sign out
                </button>
              </div>
            )}
          </div>
        )}
      </div>
    </header>
  )
}

function Graphs() {
  const [targets, setTargets] = useState<Target[]>([])
  const [agents, setAgents] = useState<AgentInfo[]>([])
  const [error, setError] = useState<string | null>(null)
  const [rangeS, setRangeS] = useState(3600)
  const [refreshKey, setRefreshKey] = useState(0)
  const [live, setLive] = useState(true)
  const [logScale, setLogScale] = useState(true)
  // An explicit window from brush-zoom; null means a trailing live window.
  // Free ranges are the mechanism, the presets are only sugar over it.
  const [zoom, setZoom] = useState<{ from: number; to: number } | null>(null)

  useEffect(() => {
    fetchTargets()
      .then(setTargets)
      .catch((e: Error) => setError(e.message))
    // An unenrolled deployment has only the local agent; failing to list them
    // must not stop the graphs rendering.
    fetchAgents()
      .then(setAgents)
      .catch(() => setAgents([]))
  }, [])

  useEffect(() => {
    if (!live || zoom) return
    const id = setInterval(() => setRefreshKey((k) => k + 1), REFRESH_MS)
    return () => clearInterval(id)
  }, [live, zoom])

  // Freeze the window per refresh tick so every stacked plot shares one time
  // axis — the precondition for the shared crosshair meaning anything.
  const [from, to] = useMemo(() => {
    if (zoom) return [zoom.from, zoom.to]
    const now = Math.floor(Date.now() / 1000)
    return [now - rangeS, now]
  }, [rangeS, refreshKey, zoom])

  const onZoom = useCallback((f: number, t: number) => setZoom({ from: f, to: t }), [])

  const pickRange = (seconds: number) => {
    setZoom(null)
    setRangeS(seconds)
  }

  const leaves = targets.filter((t) => t.host !== null && t.enabled && !t.hidden)
  const spanLabel = `${fmtSpan(to - from)} · ${new Date(from * 1000).toLocaleTimeString()} → ${new Date(to * 1000).toLocaleTimeString()}`

  // One plot per (target, agent) pair: the same target seen from two vantage
  // points is two different measurements, and averaging them would destroy
  // the very thing that makes a second vantage point worth having.
  const byID = new Map(agents.map((a) => [a.name, a.id]))
  const series = leaves.flatMap((t) => {
    const names = (t.settings.agents.effective || 'local').split(/\s+/).filter(Boolean)
    const resolved = names.map((n) => ({ name: n, id: byID.get(n) })).filter((a) => a.id !== undefined)
    // Fall back to the local agent when the names cannot be resolved yet.
    if (resolved.length === 0) return [{ target: t, agentId: 0, agentName: undefined }]
    return resolved.map((a) => ({
      target: t,
      agentId: a.id as number,
      agentName: resolved.length > 1 ? a.name : undefined,
    }))
  })

  return (
    <>
      <div className="subbar">
        <p className="range">
          {spanLabel}
          {zoom && (
            <button className="link" onClick={() => setZoom(null)}>
              reset zoom
            </button>
          )}
          {!zoom && <span className="hint">drag on a plot to zoom</span>}
        </p>
        <div className="controls">
          {RANGES.map((r) => (
            <button
              key={r.label}
              className={!zoom && r.seconds === rangeS ? 'active' : ''}
              onClick={() => pickRange(r.seconds)}
            >
              {r.label}
            </button>
          ))}
          <button className={logScale ? 'active' : ''} onClick={() => setLogScale(!logScale)}>
            log
          </button>
          <button
            className={live && !zoom ? 'active' : ''}
            onClick={() => {
              setZoom(null)
              setLive(!live)
            }}
          >
            {live && !zoom ? '● live' : '○ paused'}
          </button>
        </div>
      </div>
      {error && <p className="error">{error}</p>}
      {!error && leaves.length === 0 && (
        <p>
          No targets yet — add them under <strong>Targets</strong>, or import a SmokePing config with{' '}
          <code>smokeng config import-smokeping</code>.
        </p>
      )}
      {series.map((s) => (
        <Plot
          key={`${s.target.id}-${s.agentId}`}
          target={s.target}
          agentId={s.agentId}
          agentName={s.agentName}
          from={from}
          to={to}
          refreshKey={refreshKey}
          logScale={logScale}
          onZoom={onZoom}
        />
      ))}
    </>
  )
}

function fmtSpan(seconds: number): string {
  if (seconds < 90) return `${Math.round(seconds)}s`
  if (seconds < 5400) return `${Math.round(seconds / 60)}m`
  if (seconds < 172800) return `${(seconds / 3600).toFixed(1)}h`
  return `${(seconds / 86400).toFixed(1)}d`
}
