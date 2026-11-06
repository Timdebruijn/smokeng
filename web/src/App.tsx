import { Fragment, useEffect, useMemo, useState } from 'react'
import Admin from './Admin'
import Agents from './Agents'
import Alerts from './Alerts'
import Detail from './Detail'
import ErrorBoundary from './ErrorBoundary'
import Overview from './Overview'
import Palette from './Palette'
import A11yMenu, { useA11y, type A11yState } from './A11y'
import Bell from './Bell'
import Compare, { type CompareSeries } from './Compare'
import Grants from './Grants'
import Plot from './Plot'
import { fetchAgents, fetchMe, fetchTargets, logout, type AgentInfo, type Me, type Target } from './api'
import { RANGES, fromLocalInput, refreshMs, resolveSeries, toLocalInput, zoomOut, type PlotSeries } from './util'
import { ErrorText } from './shared'

type View = 'overview' | 'graphs' | 'targets' | 'alerts' | 'agents' | 'access' | 'detail'
type HeaderMenu = 'user' | 'a11y' | 'bell' | null

export default function App() {
  const [view, setView] = useState<View>('overview')
  // One popover at a time. Two open at once overlap in the corner they share,
  // and the one underneath cannot be reached without closing the other.
  const [menu, setMenu] = useState<HeaderMenu>(null)
  const [a11y, toggleA11y] = useA11y()
  // Which target the detail screen is showing. Detail is reached from a plot,
  // never from the tab bar, so it is not one of the tabs.
  const [detailId, setDetailId] = useState<number | null>(null)
  const [paletteOpen, setPaletteOpen] = useState(false)
  const [me, setMe] = useState<Me | null>(null)
  const openDetail = (id: number) => {
    setDetailId(id)
    setView('detail')
  }

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
        menu={menu}
        onMenu={setMenu}
        a11y={a11y}
        onToggleA11y={toggleA11y}
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
          onOpenDetail={openDetail}
        />
      )}
      <main>
      {/* Keyed by what is shown, so after a failure the next screen starts clean. */}
      <ErrorBoundary key={view === 'detail' ? `detail-${detailId}` : view} what={view === 'detail' ? 'this target' : `the ${view} screen`}>
      {view === 'overview' ? (
        <Overview onOpenDetail={openDetail} />
      ) : view === 'detail' && detailId !== null ? (
        <DetailRoute
          targetId={detailId}
          onBack={() => setView('graphs')}
          onEdit={() => setView('targets')}
        />
      ) : view === 'graphs' ? (
        <Graphs onOpenDetail={openDetail} />
      ) : view === 'targets' ? (
        <Admin readOnly={!isAdmin} />
      ) : view === 'alerts' ? (
        <Alerts isAdmin={isAdmin} />
      ) : view === 'agents' ? (
        <Agents readOnly={!isAdmin} externalURL={me?.external_url} />
      ) : (
        <Grants
            readOnly={!isAdmin}
            defaultRole={me?.default_role}
            authEnabled={me?.auth_enabled !== false}
          />
      )}
      </ErrorBoundary>
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
  onOpenPalette,
  a11y,
  onToggleA11y,
  menu,
  onMenu,
}: {
  me: Me | null
  view: View
  views: View[]
  onView: (v: View) => void
  onOpenPalette: () => void
  a11y: A11yState
  onToggleA11y: (k: keyof A11yState) => void
  menu: HeaderMenu
  onMenu: (m: HeaderMenu) => void
}) {
  const who = me?.name ?? me?.email ?? me?.subject ?? ''

  // An open popover closes on Esc (focus back on its trigger) and on a press
  // anywhere outside the header menus.
  useEffect(() => {
    if (!menu) return
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== 'Escape') return
      onMenu(null)
      document.querySelector<HTMLElement>('[aria-controls$="-popover"][aria-expanded="true"]')?.focus()
    }
    const onDown = (e: PointerEvent) => {
      if (!(e.target as Element).closest('.a11y, .bell, .usermenu')) onMenu(null)
    }
    window.addEventListener('keydown', onKey)
    window.addEventListener('pointerdown', onDown)
    return () => {
      window.removeEventListener('keydown', onKey)
      window.removeEventListener('pointerdown', onDown)
    }
  }, [menu, onMenu])
  return (
    <header className="appbar">
      <div className="appbar-inner">
        <button className="brand" onClick={() => onView('overview')}>
          {/* The same ribbon as the tab icon: the smoke and its median. */}
          <img className="brand-mark" src="/favicon.svg" alt="" width={28} height={28} />
          <span className="brand-name">
            smoke<span className="brand-ng">ng</span>
          </span>
          {me?.version && <span className="version-tag">{me.version}</span>}
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
          <span>Search</span>
          <kbd>⌘K</kbd>
        </button>
        <A11yMenu
          state={a11y}
          onToggle={onToggleA11y}
          open={menu === 'a11y'}
          onOpenChange={(o) => onMenu(o ? 'a11y' : null)}
        />
        <Bell
          open={menu === 'bell'}
          onOpenChange={(o) => onMenu(o ? 'bell' : null)}
          onOpenAlerts={() => {
            onMenu(null)
            onView('alerts')
          }}
        />
        {me?.auth_enabled && me.authenticated && (
          <div className="usermenu">
            <button
              className="chip-button"
              aria-expanded={menu === 'user'}
              aria-controls="user-popover"
              onClick={() => onMenu(menu === 'user' ? null : 'user')}
            >
              <span className="avatar">{(who || '?').slice(0, 1).toUpperCase()}</span>
              <span className="who">{who}</span>
              <span className="role-tag">{me.role}</span>
            </button>
            {menu === 'user' && (
              <div id="user-popover" className="popover">
                <p className="popover-title">{me.email ?? me.subject}</p>
                <p className="hint small">Signed in via OIDC · {me.role}</p>
                {me.version && <p className="hint small">smokeng {me.version}</p>}
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
  // Set only when listing agents failed outright, which is a different thing
  // from a list that came back without the name a target asked for.
  const [agentsError, setAgentsError] = useState<string | null>(null)
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
    // must not stop the graphs rendering. It does have to be remembered,
    // though: with no list, nothing resolves, and every target would otherwise
    // be reported as assigned to an agent nobody enrolled — a claim this
    // screen has no evidence for.
    fetchAgents()
      .then((a) => {
        setAgents(a)
        setAgentsError(null)
      })
      .catch((e: Error) => {
        setAgents([])
        setAgentsError(e.message)
      })
  }, [])

  useEffect(() => {
    if (!live || zoom) return
    const id = setInterval(() => setRefreshKey((k) => k + 1), refreshMs(rangeS))
    return () => clearInterval(id)
  }, [live, zoom, rangeS])

  // Freeze the window per refresh tick so every stacked plot shares one time
  // axis — the precondition for the shared crosshair meaning anything.
  const [from, to] = useMemo(() => {
    if (zoom) return [zoom.from, zoom.to]
    const now = Math.floor(Date.now() / 1000)
    return [now - rangeS, now]
  }, [rangeS, refreshKey, zoom])

  const onZoom = (f: number, t: number) => setZoom({ from: f, to: t })

  const pickRange = (seconds: number) => {
    setZoom(null)
    setRangeS(seconds)
  }

  const onZoomOut = () => {
    const next = zoomOut(from, to, Math.floor(Date.now() / 1000))
    if ('rangeS' in next) pickRange(next.rangeS)
    else setZoom(next)
  }

  // A window typed in: the free range the presets are sugar over.
  const [custom, setCustom] = useState<{ from: string; to: string } | null>(null)
  const customFrom = custom ? fromLocalInput(custom.from) : null
  const customTo = custom ? fromLocalInput(custom.to) : null
  const customOk = customFrom !== null && customTo !== null && customFrom < customTo

  const leaves = targets.filter((t) => t.host !== null && t.enabled && !t.hidden)

  // Which targets are on screen. Everything, until someone narrows it: a
  // monitoring page that starts empty is a page that hides a problem.
  const [hidden, setHidden] = useState<Set<number>>(new Set())
  const [graphSearch, setGraphSearch] = useState('')
  // Closed on a phone, where the filter list would otherwise sit above every
  // graph; the toolbar button opens it.
  const [sidebarOpen, setSidebarOpen] = useState(() => !window.matchMedia('(max-width: 640px)').matches)
  const [compareOn, setCompareOn] = useState(false)
  const shown = leaves.filter((t) => !hidden.has(t.id))
  const toggleShown = (id: number) =>
    setHidden((prev) => {
      const next = new Set(prev)
      if (next.has(id)) next.delete(id)
      else next.add(id)
      return next
    })
  // The date belongs in the label once the window is not all of today.
  const when = (t: number) =>
    to - from > 43200 ? new Date(t * 1000).toLocaleString([], { dateStyle: 'short', timeStyle: 'short' }) : new Date(t * 1000).toLocaleTimeString()
  const spanLabel = `${fmtSpan(to - from)} · ${when(from)} → ${when(to)}`

  const byName = new Map(agents.map((a) => [a.name, a.id]))
  const series = shown.flatMap((t) => resolveSeries(t, byName))

  // A series with no enrolled agent has nothing to draw, and a legend entry
  // for an empty line would be a target that looks compared and is not.
  const comparable: CompareSeries[] = series
    .filter((s): s is PlotSeries & { agentId: number } => s.agentId !== null)
    .map((s) => ({
      key: `${s.target.id}-${s.agentId}`,
      label: (s.target.title ?? s.target.name) + (s.agentName ? ` (${s.agentName})` : ''),
      targetId: s.target.id,
      agentId: s.agentId,
    }))

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
          <button
            className={custom ? 'pill active' : 'pill'}
            onClick={() => setCustom(custom ? null : { from: toLocalInput(from), to: toLocalInput(to) })}
            title="Choose the window's start and end"
          >
            custom
          </button>
          <button className="pill" onClick={onZoomOut} title="Show twice as much time around the same centre">
            zoom out
          </button>
          <button
            className={compareOn ? 'pill active' : 'pill'}
            onClick={() => setCompareOn((v) => !v)}
            title="Overlay the pooled medians of the shown targets on one axis"
          >
            compare
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
          {custom && (
            <form
              className="range-custom"
              onSubmit={(e) => {
                e.preventDefault()
                if (!customOk) return
                setZoom({ from: customFrom!, to: customTo! })
                setCustom(null)
              }}
            >
              <label>
                from <input type="datetime-local" value={custom.from} onChange={(e) => setCustom({ ...custom, from: e.target.value })} />
              </label>
              <label>
                to <input type="datetime-local" value={custom.to} onChange={(e) => setCustom({ ...custom, to: e.target.value })} />
              </label>
              <button className="pill accent" type="submit" disabled={!customOk}>
                show
              </button>
              {!customOk && <span className="hint small">the start has to be before the end</span>}
            </form>
          )}
        </div>
        {error && <ErrorText>{error}</ErrorText>}
        {!error && leaves.length === 0 && (
          <p className="hint">
            No targets yet — add them under <strong>Targets</strong>, or import a SmokePing config
            with <code>smokeng config import-smokeping</code>.
          </p>
        )}
        {!error && leaves.length > 0 && series.length === 0 && (
          <div className="card empty">No targets selected — check some in the target list.</div>
        )}
        {compareOn && comparable.length > 0 && (
          <Compare
            series={comparable}
            from={from}
            to={to}
            logScale={logScale}
            refreshKey={refreshKey}
          />
        )}
        {compareOn && comparable.length === 0 && (
          <div className="card empty">
            Nothing to compare — the shown targets have no enrolled agent measuring them.
          </div>
        )}
        {series.map((s) =>
          s.agentId === null ? (
            <Unmeasured
              key={`${s.target.id}-unresolved`}
              target={s.target}
              names={s.agentName ?? ''}
              listFailed={agentsError}
              onOpenDetail={() => onOpenDetail(s.target.id)}
            />
          ) : (
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
          ),
        )}
      </div>
    </section>
  )
}

/**
 * A target's place in the stack when nothing is measuring it. Same card as a
 * plot, deliberately: the operator scanning the column should find the target
 * where they expect it and read why it is blank, rather than notice later that
 * it was never there.
 *
 * A failed agent listing is said as such. Calling the target unenrolled in
 * that case would be a stronger claim than the screen can support — the name
 * may well be fine, and only the check failed.
 */
function Unmeasured({
  target,
  names,
  listFailed,
  onOpenDetail,
}: {
  target: Target
  names: string
  listFailed: string | null
  onOpenDetail: () => void
}) {
  return (
    <section className="card plot">
      <div className="plot-head">
        <span className="dot" style={{ background: 'var(--dim)' }} />
        <button className="plot-title link-title" onClick={onOpenDetail}>
          {target.title ?? target.path}
        </button>
        <span className="host">
          {target.host} · {target.address_family}
        </span>
        <span className="agent">not enrolled</span>
        <span className="spacer" />
        <button className="pill" onClick={onOpenDetail}>
          Detail →
        </button>
      </div>
      {listFailed ? (
        <p className="hint">
          Assigned to <code>{names}</code>, which could not be checked: listing agents failed (
          {listFailed}).
        </p>
      ) : (
        <p className="hint">
          Assigned to <code>{names}</code>, which is not enrolled — so nothing is measuring this
          target.
        </p>
      )}
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
        aria-label="Filter targets"
        placeholder="Filter targets"
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
