import { useEffect, useState } from 'react'

interface TargetsResponse {
  targets: { id: number; path: string; host: string | null }[]
}

// Scaffold placeholder. The real UI — density smoke plots, median line, loss
// rail, shared crosshair, brush-zoom — replaces this per DESIGN.md §8.
export default function App() {
  const [targets, setTargets] = useState<TargetsResponse['targets'] | null>(null)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    fetch('/api/v1/targets')
      .then((r) => (r.ok ? r.json() : Promise.reject(new Error(`HTTP ${r.status}`))))
      .then((body: TargetsResponse) => setTargets(body.targets))
      .catch((e: Error) => setError(e.message))
  }, [])

  return (
    <main>
      <h1>smokeng</h1>
      <p>
        Scaffold build — rendering pipeline not implemented yet. The API is
        live; the target tree below comes from <code>/api/v1/targets</code>.
      </p>
      {error && <p className="error">Failed to load targets: {error}</p>}
      {targets && (
        <ul>
          {targets.map((t) => (
            <li key={t.id}>
              <code>{t.path}</code>
              {t.host ? ` → ${t.host}` : ' (group)'}
            </li>
          ))}
        </ul>
      )}
    </main>
  )
}
