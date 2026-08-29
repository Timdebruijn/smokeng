import { Fragment, useCallback, useEffect, useMemo, useState } from 'react'
import Admin from './Admin'
import Agents from './Agents'
import Alerts from './Alerts'
import Detail from './Detail'
import Overview from './Overview'
import Palette from './Palette'
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

type View = 'overview' | 'graphs' | 'targets' | 'alerts' | 'agents' | 'access' | 'detail'

export default function App() {
  const [view, setView] = useState<View>('overview')
  const [userMenuOpen, setUserMenuOpen] = useState(false)
  // Which target the detail screen is showing. Detail is reached from a plot,
  // never from the tab bar, so it is not one of the tabs.
  const [detailId, setDetailId] = useState<number | null>(null)
  const [paletteOpen, setPaletteOpen] = useState(false)
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
    ? ['overview', 'graphs', 'targets', 'alerts', 'agents', 'access']
    : ['overview', 'graphs', 'targets', 'alerts', 'agents']

  // ⌘K / Ctrl-K, the shortcut every tool of this shape has.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'k') {
        e.preventDefault()
        setPaletteOpen((v) => !v)
      }
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [])

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
        onOpenPalette={() => setPaletteOpen(true)}
      />
      {paletteOpen && (
        <Palette
          actions={views.map((v) => ({
            id: `view-${v}`,
            label: v[0].toUpperCase() + v.slice(1),
            hint: 'screen',
            run: () => setView(v),
          }))}
          onClose={() => setPaletteOpen(false)}
          onOpenDetail={(id) => {
            setDetailId(id)
            setView('detail')
          }}
        />
      )}
      <main>
      {view === 'overview' ? (
        <Overview
          onOpenDetail={(id) => {
            setDetailId(id)
            setView('detail')
          }}
        />
      ) : view === 'detail' && detailId !== null ? (
        <DetailRoute
          targetId={detailId}
          onBack={() => setView('graphs')}
          onEdit={() => setView('targets')}
        />
      ) : view === 'graphs' ? (
        <Graphs
          onOpenDetail={(id) => {
            setDetailId(id)
            setView('detail')
          }}
        />
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
  onOpenPalette,
}: {
  me: Me | null
  view: View
  views: View[]
  onView: (v: View) => void
  userMenuOpen: boolean
  onToggleUser: () => void
  onOpenPalette: () => void
}) {
  const who = me?.name ?? me?.email ?? me?.subject ?? ''
  return (
    <header className="appbar">
      <div className="appbar-inner">
        <button className="brand" onClick={() => onView('overview')}>
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
        <button className="search-button" onClick={onOpenPalette}>
          <span>Search…</span>
          <kbd>⌘K</kbd>
        </button>
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

function Graphs({ onOpenDetail }: { onOpenDetail: (id: number) => void }) {
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

  // Which targets are on screen. Everything, until someone narrows it: a
  // monitoring page that starts empty is a page that hides a problem.
  const [hidden, setHidden] = useState<Set<number>>(new Set())
  const [graphSearch, setGraphSearch] = useState('')
  const [sidebarOpen, setSidebarOpen] = useState(true)
  const shown = leaves.filter((t) => !hidden.has(t.id))
  const toggleShown = (id: number) =>
    setHidden((prev) => {
      const next = new Set(prev)
      if (next.has(id)) next.delete(id)
      else next.add(id)
      return next
    })
  const spanLabel = `${fmtSpan(to - from)} · ${new Date(from * 1000).toLocaleTimeString()} → ${new Date(to * 1000).toLocaleTimeString()}`

  // One plot per (target, agent) pair: the same target seen from two vantage
  // points is two different measurements, and averaging them would destroy
  // the very thing that makes a second vantage point worth having.
  const byID = new Map(agents.map((a) => [a.name, a.id]))
  const series = shown.flatMap((t) => {
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
    <section className="graphs">
      {sidebarOpen && (
        <TargetSidebar
          leaves={leaves}
          hidden={hidden}
          search={graphSearch}
          onSearch={setGraphSearch}
          onToggle={toggleShown}
          onAll={(on) => setHidden(on ? new Set() : new Set(leaves.map((t) => t.id)))}
        />
      )}
      <div className="graphs-main">
        <div className="card toolbar">
          <button
            className="icon-button"
            title="Toggle target list"
            onClick={() => setSidebarOpen((v) => !v)}
          >
            ☰
          </button>
          {RANGES.map((r) => (
            <button
              key={r.label}
              className={!zoom && r.seconds === rangeS ? 'pill active' : 'pill'}
              onClick={() => pickRange(r.seconds)}
            >
              {r.label}
            </button>
          ))}
          <button
            className={logScale ? 'pill active' : 'pill'}
            onClick={() => setLogScale(!logScale)}
          >
            log
          </button>
          <button
            className={live && !zoom ? 'pill active' : 'pill'}
            onClick={() => {
              setZoom(null)
              setLive(!live)
            }}
          >
            {live && !zoom ? '● live' : '○ paused'}
          </button>
          <span className="spacer" />
          <span className="span-label">{spanLabel}</span>
          {zoom ? (
            <button className="pill accent" onClick={() => setZoom(null)}>
              reset zoom
            </button>
          ) : (
            <span className="hint small">drag on a plot to zoom</span>
          )}
        </div>
        {error && <p className="error">{error}</p>}
        {!error && leaves.length === 0 && (
          <p className="hint">
            No targets yet — add them under <strong>Targets</strong>, or import a SmokePing config
            with <code>smokeng config import-smokeping</code>.
          </p>
        )}
        {!error && leaves.length > 0 && series.length === 0 && (
          <div className="card empty">No targets selected — check some in the target list.</div>
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
            onOpenDetail={() => onOpenDetail(s.target.id)}
          />
        ))}
      </div>
    </section>
  )
}

// The target list. Group headings come from the path, so the tree the operator
// built is the tree they filter with.
function TargetSidebar({
  leaves,
  hidden,
  search,
  onSearch,
  onToggle,
  onAll,
}: {
  leaves: Target[]
  hidden: Set<number>
  search: string
  onSearch: (v: string) => void
  onToggle: (id: number) => void
  onAll: (on: boolean) => void
}) {
  const q = search.trim().toLowerCase()
  const matching = leaves.filter(
    (t) => q === '' || t.path.toLowerCase().includes(q) || (t.host ?? '').includes(q),
  )
  const rows: { group: string; items: Target[] }[] = []
  for (const t of matching) {
    const group = t.path.slice(0, t.path.lastIndexOf('/')) || '/'
    const last = rows[rows.length - 1]
    if (last && last.group === group) last.items.push(t)
    else rows.push({ group, items: [t] })
  }
  const allShown = leaves.every((t) => !hidden.has(t.id))

  return (
    <aside className="card sidebar">
      <input
        className="filter"
        value={search}
        onChange={(e) => onSearch(e.target.value)}
        placeholder="Filter targets…"
      />
      {rows.map((r) => (
        <Fragment key={r.group}>
          <p className="side-group">{r.group}</p>
          {r.items.map((t) => (
            <label key={t.id} className="side-leaf">
              <input
                type="checkbox"
                checked={!hidden.has(t.id)}
                onChange={() => onToggle(t.id)}
              />
              <span className="side-name">{t.title ?? t.name}</span>
            </label>
          ))}
        </Fragment>
      ))}
      {matching.length === 0 && <p className="hint small">Nothing matches that filter.</p>}
      <button className="pill wide" onClick={() => onAll(!allShown)}>
        {allShown ? 'Hide all' : 'Show all'}
      </button>
    </aside>
  )
}

// The detail screen needs the tree and the agent list the same way Graphs
// does; loading them here keeps it reachable by id alone.
function DetailRoute({
  targetId,
  onBack,
  onEdit,
}: {
  targetId: number
  onBack: () => void
  onEdit: () => void
}) {
  const [targets, setTargets] = useState<Target[]>([])
  const [agents, setAgents] = useState<AgentInfo[]>([])
  useEffect(() => {
    void fetchTargets().then(setTargets).catch(() => setTargets([]))
    void fetchAgents().then(setAgents).catch(() => setAgents([]))
  }, [])
  const target = targets.find((t) => t.id === targetId)
  if (!target) return <p className="hint">Loading…</p>
  return <Detail target={target} agents={agents} onBack={onBack} onEdit={onEdit} />
}

function fmtSpan(seconds: number): string {
  if (seconds < 90) return `${Math.round(seconds)}s`
  if (seconds < 5400) return `${Math.round(seconds / 60)}m`
  if (seconds < 172800) return `${(seconds / 3600).toFixed(1)}h`
  return `${(seconds / 86400).toFixed(1)}d`
}
