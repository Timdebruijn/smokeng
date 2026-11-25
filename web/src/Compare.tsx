import { useEffect, useRef, useState } from 'react'
import { fetchSeries, type Series } from './api'
import { compareSummary, fmtUs } from './format'
import { compareColumns, compareReadout, fmtWhen, gapsBefore, lineStyle, rowAt, stepRow } from './layout'
import { useWidth, downloadCSV } from './shared'
import { compareRows, csvFileName } from './seriesCSV'
import type { Family } from './util'

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

// The line area's horizontal padding, shared by the read and the draw.
const PAD_L = 52
const PAD_R = 8

export interface CompareSeries {
  key: string
  label: string
  targetId: number
  agentId: number
  family?: Family
}

/**
 * One bucket's pooled median, or null where the interval has no samples; gap
 * marks a row that follows a stretch with no rows at all.
 */
type Medians = { ts: Float64Array; med: (number | null)[]; gap: boolean[] }

function medians(s: Series): Medians {
  const med = Array.from(s.ts, (_, i) => {
    const start = s.offsets[i]
    const end = s.offsets[i + 1]
    // Sent and lost, or never attempted. Either way there is no latency to
    // plot, and inventing one would draw a healthy line over an outage.
    if (end <= start) return null
    // Samples arrive sorted, which is what the storage format guarantees.
    return s.values[start + ((end - start) >> 1)]
  })
  return { ts: s.ts, med, gap: gapsBefore(s.ts) }
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
  const [data, setData] = useState<Map<string, Medians>>(new Map())
  const [error, setError] = useState<string | null>(null)
  // The canvas is width:100%, so its box changes without any of the draw
  // effect's data dependencies changing — a sidebar toggle, a window resize,
  // or the "Larger text" a11y zoom. Observe the box and feed it back in, or
  // the backing store keeps its old pixel size and the browser stretches a
  // stale bitmap. Plot.tsx does exactly this; Compare must too.
  const boxW = useWidth(canvasRef)
  // The axis reads --dim/--border via getComputedStyle at draw time, so a
  // change to the accessibility contrast or colour-blind setting — which
  // rewrites those tokens on the root — leaves the gridlines their old colour
  // until the next data refresh. Watch the root's a11y attributes and redraw
  // when they change, so the toggle takes effect at once even while paused.
  const [theme, setTheme] = useState(0)

  const keys = series.map((s) => s.key).join('|')

  // The keyboard cursor: the start of the column under it, or null. Outside the
  // window (it moved since) it reads as no cursor.
  const [cursor, setCursor] = useState<number | null>(null)
  const liveRef = useRef<HTMLDivElement>(null)
  const keyedRef = useRef(false)
  const at = cursor !== null && cursor >= from && cursor <= to ? cursor : null
  const lines = series.map((s) => ({
    label: s.label,
    ts: data.get(s.key)?.ts ?? [],
    med: data.get(s.key)?.med ?? [],
  }))
  const readout = at === null ? null : compareReadout(at, lines)
  // Every series' columns, so one series' gap does not make the cursor skip.
  const columns = compareColumns(lines.map((l) => l.ts))

  const onKey = (e: React.KeyboardEvent) => {
    const live = liveRef.current
    if (e.key === 'Escape') {
      keyedRef.current = false
      setCursor(null)
      if (live) live.textContent = ''
      return
    }
    const next = stepRow(e.key, at === null ? -1 : rowAt(columns, at), columns.length)
    if (next === null) return
    e.preventDefault()
    keyedRef.current = true
    setCursor(columns[next])
    if (live) live.textContent = compareReadout(columns[next], lines)
  }

  const onBlur = () => {
    if (!keyedRef.current) return
    keyedRef.current = false
    setCursor(null)
    if (liveRef.current) liveRef.current.textContent = ''
  }

  useEffect(() => {
    let cancelled = false
    // A line has a point per device pixel at most, so a long window is read
    // pooled into that many columns; each column's median is then the median
    // of every sample in it. Zero means not laid out yet; boxW changes when it is.
    const dpr = window.devicePixelRatio || 1
    const lineW = (canvasRef.current?.clientWidth || boxW) - PAD_L - PAD_R
    const columns = Math.round(lineW * dpr)
    if (columns <= 0) return
    Promise.all(
      series.map((s) =>
        fetchSeries(s.targetId, s.agentId, from, to, columns, s.family)
          .then((r) => [s.key, medians(r)] as const)
          .catch(() => [s.key, null] as const),
      ),
    ).then((pairs) => {
      if (cancelled) return
      const next = new Map<string, Medians>()
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
  }, [keys, from, to, refreshKey, boxW])

  useEffect(() => {
    const mo = new MutationObserver(() => setTheme((t) => t + 1))
    mo.observe(document.documentElement, {
      attributes: true,
      attributeFilter: ['data-contrast', 'data-cb', 'data-theme'],
    })
    return () => mo.disconnect()
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

    const padL = PAD_L
    const padR = PAD_R
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
      ctx.strokeStyle = lineStyle(i).colour
      ctx.setLineDash(lineStyle(i).dash)
      ctx.lineWidth = 1.75
      ctx.lineJoin = 'round'
      ctx.beginPath()
      let drawing = false
      for (let j = 0; j < d.med.length; j++) {
        const v = d.med[j]
        // Break the line rather than bridge it, both at an interval with no
        // samples and across a stretch with no rows at all. A straight segment
        // across a gap asserts a measurement that was never taken, which is the
        // one thing these graphs must not do.
        if (d.gap[j]) drawing = false
        if (v === null) {
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

    if (at !== null) {
      const x = Math.round(xOf(at)) + 0.5
      ctx.setLineDash([])
      ctx.strokeStyle = dim
      ctx.lineWidth = 1
      ctx.beginPath()
      ctx.moveTo(x, padT)
      ctx.lineTo(x, padT + h)
      ctx.stroke()
    }
  }, [data, series, from, to, logScale, boxW, theme, at])

  // Only the series that were read: one that failed has no column, rather than
  // an empty one that would read as "no replies".
  const read = series.filter((s) => data.has(s.key))
  const exportCSV = () =>
    downloadCSV(
      csvFileName(['compare'], from, to),
      compareRows(read.map((s) => ({ label: s.label, ts: data.get(s.key)!.ts, med: data.get(s.key)!.med }))),
    )

  return (
    <div className="card compare">
      <div className="compare-head">
        <h2>Compare — pooled medians</h2>
        {series.map((s, i) => (
          <span key={s.key} className="compare-legend">
            <svg className="compare-swatch" width="24" height="4" aria-hidden="true">
              <line
                x1="0"
                x2="24"
                y1="2"
                y2="2"
                stroke={lineStyle(i).colour}
                strokeWidth="1.75"
                strokeDasharray={lineStyle(i).dash.join(' ') || undefined}
              />
            </svg>
            {s.label}
          </span>
        ))}
        <span className="spacer" />
        <button
          className="pill"
          onClick={exportCSV}
          disabled={!read.some((s) => data.get(s.key)!.ts.length > 0)}
          aria-label="Download comparison as CSV"
        >
          CSV
        </button>
      </div>
      <div
        tabIndex={0}
        role="group"
        aria-label="Compare pooled medians: readout, use the arrow keys"
        onKeyDown={onKey}
        onBlur={onBlur}
      >
        <canvas
          ref={canvasRef}
          className="compare-canvas"
          role="img"
          aria-label={compareSummary(
            `${fmtWhen(from)} – ${fmtWhen(to)}`,
            series.map((s) => ({ label: s.label, medians: data.get(s.key)?.med ?? [] })),
          )}
        />
        <div ref={liveRef} className="visually-hidden" aria-live="polite" />
      </div>
      {readout && <p className="series-readout compare-readout">{readout}</p>}
      {error && <p className="hint small error">{error}</p>}
      <p className="hint small">
        Vantage points are never averaged — each line is its own measurement. Uncheck targets in the
        target list to narrow the comparison. Only the median is drawn here; the distribution is in
        the plots on this page.
      </p>
    </div>
  )
}
