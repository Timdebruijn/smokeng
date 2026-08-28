import { useEffect, useMemo, useState } from 'react'
import { fetchTargets, type Target } from './api'
import Plot from './Plot'

const RANGES: { label: string; seconds: number }[] = [
  { label: '15m', seconds: 15 * 60 },
  { label: '1h', seconds: 3600 },
  { label: '6h', seconds: 6 * 3600 },
  { label: '24h', seconds: 24 * 3600 },
]
const REFRESH_MS = 10_000

export default function App() {
  const [targets, setTargets] = useState<Target[]>([])
  const [error, setError] = useState<string | null>(null)
  const [rangeS, setRangeS] = useState(3600)
  const [refreshKey, setRefreshKey] = useState(0)
  const [live, setLive] = useState(true)
  const [logScale, setLogScale] = useState(true)

  useEffect(() => {
    fetchTargets()
      .then(setTargets)
      .catch((e: Error) => setError(e.message))
  }, [])

  useEffect(() => {
    if (!live) return
    const id = setInterval(() => setRefreshKey((k) => k + 1), REFRESH_MS)
    return () => clearInterval(id)
  }, [live])

  // Freeze the window per refresh tick so all stacked plots share one time
  // axis — the precondition for the shared crosshair later.
  const [from, to] = useMemo(() => {
    const now = Math.floor(Date.now() / 1000)
    return [now - rangeS, now]
  }, [rangeS, refreshKey])

  const leaves = targets.filter((t) => t.host !== null && t.enabled && !t.hidden)

  return (
    <main>
      <header>
        <h1>smokeng</h1>
        <div className="controls">
          {RANGES.map((r) => (
            <button
              key={r.label}
              className={r.seconds === rangeS ? 'active' : ''}
              onClick={() => setRangeS(r.seconds)}
            >
              {r.label}
            </button>
          ))}
          <button className={logScale ? 'active' : ''} onClick={() => setLogScale(!logScale)}>
            log
          </button>
          <button className={live ? 'active' : ''} onClick={() => setLive(!live)}>
            {live ? '● live' : '○ paused'}
          </button>
        </div>
      </header>
      {error && <p className="error">{error}</p>}
      {!error && leaves.length === 0 && (
        <p>
          No targets yet — import some with <code>smokeng config import targets.toml</code>.
        </p>
      )}
      {leaves.map((t) => (
        <Plot key={t.id} target={t} from={from} to={to} refreshKey={refreshKey} logScale={logScale} />
      ))}
    </main>
  )
}
