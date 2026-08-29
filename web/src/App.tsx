import { useCallback, useEffect, useMemo, useState } from 'react'
import Admin from './Admin'
import Plot from './Plot'
import { fetchTargets, type Target } from './api'

const RANGES: { label: string; seconds: number }[] = [
  { label: '15m', seconds: 15 * 60 },
  { label: '1h', seconds: 3600 },
  { label: '6h', seconds: 6 * 3600 },
  { label: '24h', seconds: 24 * 3600 },
]
const REFRESH_MS = 10_000

type View = 'graphs' | 'targets'

export default function App() {
  const [view, setView] = useState<View>('graphs')
  return (
    <main>
      <header>
        <h1>smokeng</h1>
        <nav className="controls">
          <button className={view === 'graphs' ? 'active' : ''} onClick={() => setView('graphs')}>
            Graphs
          </button>
          <button className={view === 'targets' ? 'active' : ''} onClick={() => setView('targets')}>
            Targets
          </button>
        </nav>
      </header>
      {view === 'graphs' ? <Graphs /> : <Admin />}
    </main>
  )
}

function Graphs() {
  const [targets, setTargets] = useState<Target[]>([])
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
      {leaves.map((t) => (
        <Plot
          key={t.id}
          target={t}
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
