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
  const rowsRef = useRef<{
    ts: Float64Array
    median: Float64Array
    /** 1 where the interval measured this series at all; see ExtraSeries. */
    measured: Uint8Array
    span: number
  } | null>(null)
  const cursorRef = useRef<number | null>(null)
  const [state, setState] = useState<'loading' | 'ok' | 'unmeasured' | 'empty' | 'error'>('loading')
  const [readout, setReadout] = useState<string | null>(null)
  // Re-fetch and re-render on a real width change. Without this the density
  // canvas kept the bitmap it was given — time axis baked in — while CSS
  // stretched it to the new width, so an excursion sat visually at one time
  // while the crosshair, which measures the live element, reported another.
  const [width, setWidth] = useState(0)
  const label = SERIES_LABELS[name]

  useEffect(() => {
    const el = wrapRef.current
    if (!el) return
    const ro = new ResizeObserver(([entry]) => setWidth(Math.round(entry.contentRect.width)))
    ro.observe(el)
    setWidth(el.clientWidth)
    return () => ro.disconnect()
  }, [])

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
          // empty plot, which would read as "measured, and flat" — but say
          // only what is known. An empty window and a peer that stamps nothing
          // both land here, and they are not the same fact.
          rowsRef.current = null
          // And drop the readout with the rows it came from. It is only
          // recomputed when the cursor next moves, so leaving it would show a
          // median beside the words "not measured" until the reader happened
          // to twitch the mouse — a stale number presented as current, which
          // is the one thing this project must not do.
          setReadout(null)
          setState(s.ts.length === 0 ? 'empty' : 'unmeasured')
          workerRef.current.postMessage({ type: 'clear' })
          return
        }
        const n = s.ts.length
        const median = new Float64Array(n)
        for (let i = 0; i < n; i++) {
          const a = ex.offsets[i]
          const b = ex.offsets[i + 1]
          // Values are stored sorted ascending, so the median is a lookup.
          // An interval with no values is a gap, not a zero.
          median[i] = ex.measured[i] === 1 && b > a ? ex.values[a + ((b - a) >> 1)] : NaN
        }
        rowsRef.current = {
          ts: s.ts.slice(),
          median,
          measured: ex.measured.slice(),
          span: inferSpan(s.ts, n),
        }
        // Measured now rather than when the effect started: at effect time the
        // element may not have been laid out, and a zero width would render an
        // empty plot that never recovers.
        const cssW = wrapRef.current?.clientWidth || width
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
        setReadout(null)
        setState('error')
        workerRef.current?.postMessage({ type: 'clear' })
      })
    return () => {
      cancelled = true
    }
  }, [targetId, agentId, name, from, to, refreshKey, targetPath, width])

  // The shared cursor: one crosshair across every stacked plot.
  useEffect(() => {
    const draw = () => {
      const cv = overlayRef.current
      const wrap = wrapRef.current
      if (!cv || !wrap) return
      const dpr = window.devicePixelRatio || 1
      const w = Math.round(wrap.clientWidth * dpr)
      const h = Math.round(HEIGHT * dpr)
      // Only on a real size change. Assigning width or height clears the
      // canvas and reallocates its buffer, and this runs on every mousemove
      // over any plot on the page — Plot.tsx guards it for the same reason.
      if (cv.width !== w || cv.height !== h) {
        cv.width = w
        cv.height = h
      }
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
      // The last row starting at or before the cursor — a row's ts is the
      // start of the interval it covers, [ts, ts+span). Picking the *nearest*
      // row instead reported the following interval's median for everything
      // past the midpoint of each bucket, which is the number an operator
      // reads off the graph. Plot.tsx has always done it this way.
      let lo = 0
      let hi = rows.ts.length - 1
      let idx = -1
      while (lo <= hi) {
        const mid = (lo + hi) >> 1
        if (rows.ts[mid] <= t) {
          idx = mid
          lo = mid + 1
        } else {
          hi = mid - 1
        }
      }
      if (idx < 0 || t > rows.ts[idx] + rows.span) {
        setReadout(null)
        return
      }
      lo = idx
      const v = rows.median[lo]
      // Three different things, and the graph used to call all of them "not
      // measured": the peer could not report it, it could but this interval
      // had nothing to difference (one reply, so no consecutive pair), and a
      // real value.
      let reading: string
      if (!Number.isNaN(v)) {
        reading = `median ${name === 'server_processing' ? fmtUs(v) : fmtSigned(v)}`
      } else if (rows.measured[lo] === 1) {
        reading = 'nothing to compare'
      } else {
        reading = 'not measured'
      }
      setReadout(`${fmtClock(rows.ts[lo])} · ${reading}`)
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
        {state === 'empty' && <p className="series-note">No measurements in this window.</p>}
        {state === 'unmeasured' && (
          <p className="series-note">
            Not measured. This session's timestamps do not support it — the far end stamps at the
            midpoint or on one side only, or negotiated the monotonic clock away, and the figure
            would mean something other than its heading. Check what the <code>irtt server</code>{' '}
            was started with.
          </p>
        )}
        {state === 'error' && <p className="series-note">Could not load this series.</p>}
      </div>
    </figure>
  )
}
