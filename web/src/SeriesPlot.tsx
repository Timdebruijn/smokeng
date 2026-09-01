import { useEffect, useRef, useState } from 'react'
import { SERIES_LABELS, fetchSeries, type SeriesName } from './api'
import { setCursor, subscribeCursor } from './crosshair'
import { AXIS_W, fmtClock, fmtSigned, fmtUs, inferSpan } from './layout'

interface Props {
  targetId: number
  targetPath: string
  agentId: number
  name: SeriesName
  from: number
  to: number
  refreshKey: number
}

/** Shorter than the round trip's: these are secondary graphs, read alongside it. */
const HEIGHT = 150

/**
 * One extra per-packet distribution, drawn with the same density renderer the
 * round trip uses and sharing its time cursor, so the three line up under one
 * crosshair.
 *
 * Deliberately not a mode of Plot. Plot carries the loss rail, the quality
 * flags, the path-change markers and the zoom — all of which describe the
 * round-trip measurement and none of which this series measured. Bolting a
 * second meaning onto it would have put a loss figure under a graph that never
 * counted a loss.
 */
export default function SeriesPlot({
  targetId,
  targetPath,
  agentId,
  name,
  from,
  to,
  refreshKey,
}: Props) {
  const wrapRef = useRef<HTMLDivElement>(null)
  const canvasRef = useRef<HTMLCanvasElement>(null)
  const overlayRef = useRef<HTMLCanvasElement>(null)
  const workerRef = useRef<Worker | null>(null)
  const rowsRef = useRef<{ ts: Float64Array; median: Float64Array; span: number } | null>(null)
  const cursorRef = useRef<number | null>(null)
  const [state, setState] = useState<'loading' | 'ok' | 'unmeasured' | 'error'>('loading')
  const [readout, setReadout] = useState<string | null>(null)
  const label = SERIES_LABELS[name]

  useEffect(() => {
    const cv = canvasRef.current
    if (!cv) return
    const off = cv.transferControlToOffscreen()
    const worker = new Worker(new URL('./render.worker.ts', import.meta.url), { type: 'module' })
    worker.onerror = (e) => console.error(`series ${targetPath}/${name}:`, e.message || e)
    worker.postMessage({ type: 'init', canvas: off }, [off])
    workerRef.current = worker
    return () => {
      worker.terminate()
      workerRef.current = null
    }
    // The canvas can only be transferred once, so this worker is created once
    // per mounted plot and never re-created for a range change.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  useEffect(() => {
    let cancelled = false
    const dpr = window.devicePixelRatio || 1
    const dark = matchMedia('(prefers-color-scheme: dark)').matches
    fetchSeries(targetId, agentId, from, to)
      .then((s) => {
        if (cancelled || !workerRef.current) return
        const ex = s.extra[name]
        if (!ex) {
          // Nothing in this window measured it. Say so rather than drawing an
          // empty plot, which would read as "measured, and flat".
          rowsRef.current = null
          setState('unmeasured')
          workerRef.current.postMessage({ type: 'clear' })
          return
        }
        const n = s.ts.length
        const median = new Float64Array(n)
        for (let i = 0; i < n; i++) {
          const a = ex.offsets[i]
          const b = ex.offsets[i + 1]
          // Values are stored sorted ascending, so the median is a lookup.
          // An interval with no values is a gap, not a zero: with only one
          // reply there is no consecutive pair to compare.
          median[i] = ex.measured[i] === 1 && b > a ? ex.values[a + ((b - a) >> 1)] : NaN
        }
        rowsRef.current = { ts: s.ts.slice(), median, span: inferSpan(s.ts, n) }
        const cssW = wrapRef.current?.clientWidth ?? 0
        if (cssW === 0) return
        setState('ok')
        workerRef.current.postMessage(
          {
            type: 'render',
            series: {
              ts: s.ts,
              sent: s.sent,
              received: s.received,
              offsets: ex.offsets,
              values: ex.values,
            },
            view: {
              cssW,
              cssH: HEIGHT,
              dpr,
              t0: from,
              t1: to,
              dark,
              log: false,
              signed: name !== 'server_processing',
              rail: false,
            },
          },
          [s.ts.buffer, s.sent.buffer, s.received.buffer, ex.offsets.buffer, ex.values.buffer],
        )
      })
      .catch((e: Error) => {
        if (cancelled) return
        console.error(`series ${targetPath}/${name}:`, e)
        rowsRef.current = null
        setState('error')
        workerRef.current?.postMessage({ type: 'clear' })
      })
    return () => {
      cancelled = true
    }
  }, [targetId, agentId, name, from, to, refreshKey, targetPath])

  // The shared cursor: one crosshair across every stacked plot.
  useEffect(() => {
    const draw = () => {
      const cv = overlayRef.current
      const wrap = wrapRef.current
      if (!cv || !wrap) return
      const dpr = window.devicePixelRatio || 1
      const cssW = wrap.clientWidth
      cv.width = Math.round(cssW * dpr)
      cv.height = Math.round(HEIGHT * dpr)
      const ctx = cv.getContext('2d')!
      ctx.clearRect(0, 0, cv.width, cv.height)
      const t = cursorRef.current
      if (t === null || t < from || t > to) {
        setReadout(null)
        return
      }
      const plotW = cv.width - Math.round(AXIS_W * dpr)
      const x = Math.round(AXIS_W * dpr) + ((t - from) / (to - from)) * plotW
      ctx.strokeStyle = matchMedia('(prefers-color-scheme: dark)').matches
        ? 'rgba(255,255,255,0.35)'
        : 'rgba(0,0,0,0.3)'
      ctx.lineWidth = Math.max(1, Math.round(dpr / 2))
      ctx.beginPath()
      ctx.moveTo(x + 0.5, 0)
      ctx.lineTo(x + 0.5, cv.height)
      ctx.stroke()

      const rows = rowsRef.current
      if (!rows) {
        setReadout(null)
        return
      }
      let lo = 0
      let hi = rows.ts.length - 1
      while (lo < hi) {
        const mid = (lo + hi) >> 1
        if (rows.ts[mid] < t) lo = mid + 1
        else hi = mid
      }
      // Outside the interval the cursor sits in, there is nothing to report.
      if (lo > 0 && Math.abs(rows.ts[lo - 1] - t) < Math.abs(rows.ts[lo] - t)) lo--
      if (Math.abs(rows.ts[lo] - t) > rows.span) {
        setReadout(null)
        return
      }
      const v = rows.median[lo]
      setReadout(
        `${fmtClock(rows.ts[lo])} · median ${
          Number.isNaN(v) ? 'not measured' : name === 'server_processing' ? fmtUs(v) : fmtSigned(v)
        }`,
      )
    }
    return subscribeCursor((t) => {
      cursorRef.current = t
      draw()
    })
  }, [from, to, name])

  const onMove = (e: React.MouseEvent) => {
    const wrap = wrapRef.current
    if (!wrap) return
    const r = wrap.getBoundingClientRect()
    const plotW = r.width - AXIS_W
    const x = e.clientX - r.left - AXIS_W
    if (x < 0 || plotW <= 0) return setCursor(null)
    setCursor(from + (x / plotW) * (to - from))
  }

  return (
    <figure className="series-plot">
      <figcaption>
        <strong>{label?.title ?? name}</strong>
        <span className="series-help">{label?.help}</span>
        {readout && <span className="series-readout">{readout}</span>}
      </figcaption>
      <div
        ref={wrapRef}
        className="series-canvas"
        style={{ height: HEIGHT }}
        onMouseMove={onMove}
        onMouseLeave={() => setCursor(null)}
      >
        <canvas ref={canvasRef} />
        <canvas ref={overlayRef} className="series-overlay" />
        {state === 'unmeasured' && (
          <p className="series-note">
            Not measured in this window. The far end returned no timestamps to compute it from —
            an <code>irtt server</code> started without them, or a probe type that has no such
            measure.
          </p>
        )}
        {state === 'error' && <p className="series-note">Could not load this series.</p>}
      </div>
    </figure>
  )
}
