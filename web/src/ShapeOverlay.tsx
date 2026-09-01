import { useEffect, useMemo, useState } from 'react'
import { fetchShapeReference, type ShapeReference } from './api'

/**
 * The evidence behind a fired shape alert: the reference distribution it is
 * compared against, and the current one, drawn together.
 *
 * A z-score or a distance in milliseconds is a claim. This is the thing itself —
 * and seeing the two curves is how a person decides whether a shape alert found
 * a real change or a curiosity. It is the same commitment the graphs make: show
 * the distribution, never a summary standing in for it.
 */
export default function ShapeOverlay({
  ruleId,
  targetId,
  agentId,
}: {
  ruleId: number
  targetId: number
  agentId: number
}) {
  const [data, setData] = useState<ShapeReference | null>(null)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    let cancelled = false
    fetchShapeReference(ruleId, targetId, agentId)
      .then((d) => !cancelled && setData(d))
      .catch((e: Error) => !cancelled && setError(e.message))
    return () => {
      cancelled = true
    }
  }, [ruleId, targetId, agentId])

  const bins = useMemo(() => {
    if (!data) return null
    return histograms(data.reference ?? [], data.current ?? [])
  }, [data])

  if (error) return <p className="error">{error}</p>
  if (!data) return <p className="hint small">Loading the distributions…</p>
  if (!data.available || !bins) {
    return (
      <p className="hint small">
        {data.kind === 'golden'
          ? 'No reference captured for this rule yet, so there is nothing to compare against.'
          : 'Still building a baseline for this series — a rolling comparison needs some history first.'}
      </p>
    )
  }

  const W = 520
  const H = 120
  const max = Math.max(...bins.ref, ...bins.cur, 1)
  const bw = W / bins.ref.length

  return (
    <div className="shape-overlay">
      <svg viewBox={`0 0 ${W} ${H + 18}`} width="100%" height={H + 18} role="img"
        aria-label="The reference distribution and the current one, overlaid">
        {bins.ref.map((v, i) => (
          <rect
            key={`r${i}`}
            x={i * bw}
            y={H - (v / max) * H}
            width={Math.max(bw - 1, 1)}
            height={(v / max) * H}
            fill="var(--dim)"
            opacity={0.55}
          />
        ))}
        {bins.cur.map((v, i) => (
          <rect
            key={`c${i}`}
            x={i * bw}
            y={H - (v / max) * H}
            width={Math.max(bw - 1, 1)}
            height={(v / max) * H}
            fill="var(--accent)"
            opacity={0.55}
          />
        ))}
        <text x={0} y={H + 14} className="axis-label" fontSize="10" fill="currentColor">
          {fmtMs(bins.lo)}
        </text>
        <text x={W} y={H + 14} textAnchor="end" className="axis-label" fontSize="10" fill="currentColor">
          {fmtMs(bins.hi)}
        </text>
      </svg>
      <p className="hint small">
        <span className="swatch reference" /> {data.kind === 'golden' ? 'captured reference' : 'recent baseline'}
        {'  '}
        <span className="swatch current" /> current interval
      </p>
    </div>
  )
}

// Two histograms over a shared log-spaced range, so the pair is comparable and
// a change at 1 ms is as visible as one at 100 ms — the same reason the plots
// default to a logarithmic axis.
function histograms(ref: number[], cur: number[]) {
  const all = [...ref, ...cur].filter((v) => v > 0)
  if (all.length === 0) return null
  const lo = Math.min(...all)
  const hi = Math.max(...all)
  if (!(hi > lo)) return null
  const n = 40
  const l0 = Math.log(lo)
  const l1 = Math.log(hi)
  const fill = (xs: number[]) => {
    const out = new Array<number>(n).fill(0)
    for (const v of xs) {
      if (v <= 0) continue
      let i = Math.floor(((Math.log(v) - l0) / (l1 - l0)) * n)
      if (i < 0) i = 0
      if (i >= n) i = n - 1
      out[i]++
    }
    // Normalise to a fraction, so distributions of different sample counts are
    // compared on shape rather than on how many probes happened to go out.
    const total = xs.length || 1
    return out.map((v) => v / total)
  }
  return { ref: fill(ref), cur: fill(cur), lo, hi }
}

function fmtMs(us: number): string {
  const ms = us / 1000
  return ms >= 10 ? `${ms.toFixed(0)}ms` : ms >= 1 ? `${ms.toFixed(1)}ms` : `${us.toFixed(0)}µs`
}
