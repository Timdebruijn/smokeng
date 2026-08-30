import { useEffect, useMemo, useRef, useState } from 'react'
import { fetchSeries, type Series } from './api'
import { fmtUs } from './format'

/**
 * Several series' pooled medians on one axis.
 *
 * The stacked plots answer "what is this target doing"; this answers "which of
 * these is worse", which a column of separate y-axes cannot. It is deliberately
 * only the median: overlaying full distributions would produce a wash of
 * overlapping density in which no individual target is readable, and the
 * per-target plots below already carry the distribution that matters.
 *
 * Vantage points are never merged. A target measured from three agents is
 * three lines here, exactly as it is three plots below — averaging them would
 * destroy the one thing a second vantage point is for.
 */

/**
 * Okabe-Ito, chosen as identities rather than as meanings.
 *
 * These are fixed and do not follow the colour-blind-safe setting, because
 * they are already safe under it: this palette is distinguishable under every
 * common form of colour vision deficiency. A line's colour here says "which
 * target", never "how healthy", so it carries none of the red/green semantics
 * that setting exists to replace.
 */
const LINE_COLOURS = [
  '#0072b2',
  '#e69f00',
  '#009e73',
  '#cc79a7',
  '#56b4e9',
  '#d55e00',
  '#8c62aa',
  '#117733',
]

export interface CompareSeries {
  key: string
  label: string
  targetId: number
  agentId: number
}

/** One bucket's pooled median, or null where the interval has no samples. */
function medians(s: Series): { ts: Float64Array; med: (number | null)[] } {
  const med: (number | null)[] = new Array(s.ts.length)
  for (let i = 0; i < s.ts.length; i++) {
    const start = s.offsets[i]
    const end = s.offsets[i + 1]
    if (end <= start) {
      // Sent and lost, or never attempted. Either way there is no latency to
      // plot, and inventing one would draw a healthy line over an outage.
      med[i] = null
      continue
    }
    // Samples arrive sorted, which is what the storage format guarantees.
    med[i] = s.values[start + ((end - start) >> 1)]
  }
  return { ts: s.ts, med }
}

export default function Compare({
  series,
  from,
  to,
  logScale,
  refreshKey,
}: {
  series: CompareSeries[]
  from: number
  to: number
  logScale: boolean
  refreshKey: number
}) {
  const canvasRef = useRef<HTMLCanvasElement>(null)
  const [data, setData] = useState<Map<string, { ts: Float64Array; med: (number | null)[] }>>(
    new Map(),
  )
  const [error, setError] = useState<string | null>(null)
  // The canvas is width:100%, so its box changes without any of the draw
  // effect's data dependencies changing — a sidebar toggle, a window resize,
  // or the "Larger text" a11y zoom. Observe the box and feed it back in, or
  // the backing store keeps its old pixel size and the browser stretches a
  // stale bitmap. Plot.tsx does exactly this; Compare must too.
  const [boxW, setBoxW] = useState(0)
  // The axis reads --dim/--border via getComputedStyle at draw time, so a
  // change to the accessibility contrast or colour-blind setting — which
  // rewrites those tokens on the root — leaves the gridlines their old colour
  // until the next data refresh. Watch the root's a11y attributes and redraw
  // when they change, so the toggle takes effect at once even while paused.
  const [theme, setTheme] = useState(0)

  const keys = useMemo(() => series.map((s) => s.key).join('|'), [series])

  useEffect(() => {
    let cancelled = false
    Promise.all(
      series.map((s) =>
        fetchSeries(s.targetId, s.agentId, from, to)
          .then((r) => [s.key, medians(r)] as const)
          .catch(() => [s.key, null] as const),
      ),
    ).then((pairs) => {
      if (cancelled) return
      const next = new Map<string, { ts: Float64Array; med: (number | null)[] }>()
      let failed = 0
      for (const [k, v] of pairs) {
        if (v) next.set(k, v)
        else failed++
      }
      setData(next)
      setError(failed > 0 ? `${failed} of ${pairs.length} series could not be read` : null)
    })
    return () => {
      cancelled = true
    }
    // keys rather than series: the array identity changes on every render.
  }, [keys, from, to, refreshKey])

  useEffect(() => {
    const canvas = canvasRef.current
    if (!canvas) return
    const ro = new ResizeObserver(([entry]) => {
      setBoxW(Math.round(entry.contentRect.width))
    })
    ro.observe(canvas)
    setBoxW(canvas.clientWidth)

    const mo = new MutationObserver(() => setTheme((t) => t + 1))
    mo.observe(document.documentElement, {
      attributes: true,
      attributeFilter: ['data-contrast', 'data-cb', 'data-theme'],
    })
    return () => {
      ro.disconnect()
      mo.disconnect()
    }
  }, [])

  useEffect(() => {
    const canvas = canvasRef.current
    if (!canvas) return
    const ctx = canvas.getContext('2d')
    if (!ctx) return

    const dpr = window.devicePixelRatio || 1
    const cssW = canvas.clientWidth
    const cssH = canvas.clientHeight
    canvas.width = Math.round(cssW * dpr)
    canvas.height = Math.round(cssH * dpr)
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0)
    ctx.clearRect(0, 0, cssW, cssH)

    const css = getComputedStyle(document.documentElement)
    const dim = css.getPropertyValue('--dim').trim() || '#888'
    const border = css.getPropertyValue('--border').trim() || '#ccc'

    const padL = 52
    const padR = 8
    const padT = 8
    const padB = 22
    const w = cssW - padL - padR
    const h = cssH - padT - padB
    if (w <= 0 || h <= 0) return

    // One scale across every line, or the comparison is between pictures
    // rather than between numbers.
    let lo = Infinity
    let hi = -Infinity
    for (const d of data.values()) {
      for (const v of d.med) {
        if (v === null) continue
        if (v < lo) lo = v
        if (v > hi) hi = v
      }
    }
    if (!isFinite(lo) || !isFinite(hi)) {
      ctx.fillStyle = dim
      ctx.font = '13px system-ui, sans-serif'
      ctx.textAlign = 'center'
      ctx.fillText('No replies in this window.', cssW / 2, cssH / 2)
      return
    }
    if (hi === lo) hi = lo + 1

    const useLog = logScale && lo > 0
    const yOf = (v: number) => {
      const t = useLog
        ? (Math.log(Math.max(v, 1)) - Math.log(lo)) / (Math.log(hi) - Math.log(lo) || 1)
        : (v - lo) / (hi - lo)
      return padT + h - t * h
    }
    const xOf = (t: number) => padL + ((t - from) / Math.max(1, to - from)) * w

    // Axis: four labelled gridlines is enough to read a magnitude from
    // without turning the plot into graph paper.
    ctx.strokeStyle = border
    ctx.fillStyle = dim
    ctx.lineWidth = 1
    ctx.font = '11px system-ui, sans-serif'
    ctx.textAlign = 'right'
    ctx.textBaseline = 'middle'
    for (let i = 0; i <= 3; i++) {
      const frac = i / 3
      const v = useLog
        ? Math.exp(Math.log(lo) + frac * (Math.log(hi) - Math.log(lo)))
        : lo + frac * (hi - lo)
      const y = Math.round(yOf(v)) + 0.5
      ctx.beginPath()
      ctx.moveTo(padL, y)
      ctx.lineTo(padL + w, y)
      ctx.stroke()
      ctx.fillText(fmtUs(v), padL - 6, y)
    }

    series.forEach((s, i) => {
      const d = data.get(s.key)
      if (!d) return
      ctx.strokeStyle = LINE_COLOURS[i % LINE_COLOURS.length]
      ctx.lineWidth = 1.75
      ctx.lineJoin = 'round'
      ctx.beginPath()
      let drawing = false
      for (let j = 0; j < d.med.length; j++) {
        const v = d.med[j]
        if (v === null) {
          // Break the line rather than bridge it. A straight segment across a
          // gap asserts a measurement that was never taken, which is the one
          // thing these graphs must not do.
          drawing = false
          continue
        }
        const x = xOf(d.ts[j])
        const y = yOf(v)
        if (drawing) ctx.lineTo(x, y)
        else ctx.moveTo(x, y)
        drawing = true
      }
      ctx.stroke()
    })
  }, [data, series, from, to, logScale, boxW, theme])

  return (
    <div className="card compare">
      <div className="compare-head">
        <h2>Compare — pooled medians</h2>
        {series.map((s, i) => (
          <span key={s.key} className="compare-legend">
            <span
              className="compare-swatch"
              style={{ background: LINE_COLOURS[i % LINE_COLOURS.length] }}
            />
            {s.label}
          </span>
        ))}
      </div>
      <canvas ref={canvasRef} className="compare-canvas" />
      {error && <p className="hint small error">{error}</p>}
      <p className="hint small">
        Vantage points are never averaged — each line is its own measurement. Uncheck targets on
        the left to narrow the comparison. Only the median is drawn here; the distribution is in
        the plots below.
      </p>
    </div>
  )
}
