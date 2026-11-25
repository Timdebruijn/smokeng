import { useEffect, useId, useRef, useState } from 'react'
import { SERIES_LABELS, fetchSeries, type SeriesName } from './api'
import { setCursor, subscribeCursor } from './crosshair'
import { chartSummary, fmtUs } from './format'
import { AXIS_W, fmtClock, fmtSigned, fmtWhen, inferSpan, rowAt, stepRow } from './layout'
import { useRenderWorker, useWidth, downloadCSV } from './shared'
import { csvFileName, extraSeriesRows } from './seriesCSV'
import type { Family } from './util'

interface Props {
  targetId: number
  /** Only for the plot's text alternative. */
  targetName: string
  agentId: number
  /** Which of a both target's two series; unset for any other. */
  family?: Family
  name: SeriesName
  from: number
  to: number
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
  targetName,
  agentId,
  family,
  name,
  from,
  to,
}: Props) {
  const wrapRef = useRef<HTMLDivElement>(null)
  const canvasRef = useRef<HTMLCanvasElement>(null)
  const overlayRef = useRef<HTMLCanvasElement>(null)
  const rowsRef = useRef<{
    ts: Float64Array
    median: Float64Array
    /** 1 where the interval measured this series at all; see ExtraSeries. */
    measured: Uint8Array
    span: number
    /** The window these rows were fetched for, which the props may have left since. */
    range: { from: number; to: number }
    /** A pooled read: where each column ends. */
    ends?: Float64Array
  } | null>(null)
  const cursorRef = useRef<number | null>(null)
  // The readout the last draw produced, for the keyboard's live region (the
  // mouse never writes that region, or every mousemove would be announced).
  const readoutRef = useRef<string | null>(null)
  const liveRef = useRef<HTMLDivElement>(null)
  const keyedRef = useRef(false)
  const [state, setState] = useState<'loading' | 'ok' | 'unmeasured' | 'empty' | 'error'>('loading')
  const [readout, setReadoutState] = useState<string | null>(null)
  const [summary, setSummary] = useState('')
  const setReadout = (r: string | null) => {
    readoutRef.current = r
    setReadoutState(r)
  }
  // Re-fetch and re-render on a real width change. Without this the density
  // canvas kept the bitmap it was given — time axis baked in — while CSS
  // stretched it to the new width, so an excursion sat visually at one time
  // while the crosshair, which measures the live element, reported another.
  const width = useWidth(wrapRef)
  const workerRef = useRenderWorker(canvasRef, `series ${targetId}${family ? `/${family}` : ''}/${name}`)
  const label = SERIES_LABELS[name]
  const hintId = useId()
  const plotName = [targetName, label?.title ?? name, family].filter(Boolean).join(' ')
  const when = `${fmtWhen(from)} – ${fmtWhen(to)}`

  useEffect(() => {
    let cancelled = false
    const dpr = window.devicePixelRatio || 1
    const dark = matchMedia('(prefers-color-scheme: dark)').matches
    // Sized before the fetch, as Plot is: a long window comes back pooled into
    // exactly the columns the worker draws. Zero means not laid out yet, and
    // `width` changes when it is.
    const cssW = wrapRef.current?.clientWidth || width
    const columns = Math.round(cssW * dpr) - Math.round(AXIS_W * dpr)
    if (cssW === 0 || columns <= 0) return
    fetchSeries(targetId, agentId, from, to, columns, family)
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
          setSummary(
            `${plotName}, ${when}: ${s.ts.length === 0 ? 'no data in this window' : 'not measured in this session'}.`,
          )
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
          range: { from, to },
          ends: s.pooled?.ends.slice(),
        }
        setState('ok')
        setSummary(
          chartSummary({
            name: plotName,
            when,
            medians: median,
            intervals: s.pooled ? s.pooled.rounds.reduce((a, b) => a + b, 0) : n,
            fmt: name === 'server_processing' ? fmtUs : fmtSigned,
          }),
        )
        workerRef.current.postMessage(
          {
            type: 'render',
            series: {
              ts: s.ts,
              sent: s.sent,
              received: s.received,
              offsets: ex.offsets,
              values: ex.values,
              pooled: s.pooled !== undefined,
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
        console.error(`series ${targetId}${family ? `/${family}` : ''}/${name}:`, e)
        rowsRef.current = null
        setReadout(null)
        setState('error')
        setSummary(`${plotName}: could not be loaded.`)
        workerRef.current?.postMessage({ type: 'clear' })
      })
    return () => {
      cancelled = true
    }
  }, [targetId, agentId, family, name, from, to, width])

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
      const idx = rowAt(rows.ts, t)
      if (idx < 0 || t >= (rows.ends ? rows.ends[idx] : rows.ts[idx] + rows.span)) {
        setReadout(null)
        return
      }
      const v = rows.median[idx]
      // Three different things, and the graph used to call all of them "not
      // measured": the peer could not report it, it could but this interval
      // had nothing to difference (one reply, so no consecutive pair), and a
      // real value.
      let reading: string
      if (!Number.isNaN(v)) {
        reading = `median ${name === 'server_processing' ? fmtUs(v) : fmtSigned(v)}`
      } else if (rows.measured[idx] === 1) {
        reading = 'nothing to compare'
      } else {
        reading = 'not measured'
      }
      const when = rows.ends ? `${fmtWhen(rows.ts[idx])} – ${fmtWhen(rows.ends[idx])}` : fmtClock(rows.ts[idx])
      setReadout(`${when} · ${reading}`)
    }
    return subscribeCursor((t) => {
      cursorRef.current = t
      draw()
    })
  }, [from, to, name])

  // Same keys as Plot: the shared cursor, one row at a time; the draw leaves the
  // readout in readoutRef, which goes into the live region.
  const onKey = (e: React.KeyboardEvent) => {
    const live = liveRef.current
    if (e.key === 'Escape') {
      keyedRef.current = false
      setCursor(null)
      if (live) live.textContent = ''
      return
    }
    const rows = rowsRef.current
    if (!rows) return
    const cur = cursorRef.current
    const next = stepRow(e.key, cur === null ? -1 : rowAt(rows.ts, cur), rows.ts.length)
    if (next === null) return
    e.preventDefault()
    keyedRef.current = true
    setCursor(rows.ts[next])
    if (live) live.textContent = readoutRef.current ?? ''
  }

  const onBlur = () => {
    if (!keyedRef.current) return
    keyedRef.current = false
    setCursor(null)
    if (liveRef.current) liveRef.current.textContent = ''
  }

  const onMove = (e: React.MouseEvent) => {
    const wrap = wrapRef.current
    if (!wrap) return
    const r = wrap.getBoundingClientRect()
    const plotW = r.width - AXIS_W
    const x = e.clientX - r.left - AXIS_W
    if (x < 0 || plotW <= 0) return setCursor(null)
    setCursor(from + (x / plotW) * (to - from))
  }

  const exportCSV = () => {
    const rows = rowsRef.current
    if (rows) downloadCSV(csvFileName([targetName, family, name], rows.range.from, rows.range.to), extraSeriesRows(rows))
  }

  return (
    <figure className="series-plot">
      <figcaption>
        <strong>{label?.title ?? name}</strong>
        {family && <span className="agent">{family}</span>}
        <span className="series-help">{label?.help}</span>
        <button className="pill" onClick={exportCSV} disabled={state !== 'ok'} aria-label={`Download ${plotName} as CSV`}>
          CSV
        </button>
        {readout && <span className="series-readout">{readout}</span>}
      </figcaption>
      <div
        ref={wrapRef}
        className="series-canvas"
        style={{ height: HEIGHT }}
        onMouseMove={onMove}
        onMouseLeave={() => setCursor(null)}
        tabIndex={0}
        role="group"
        aria-label={`${plotName} values`}
        aria-describedby={hintId}
        onKeyDown={onKey}
        onBlur={onBlur}
      >
        <canvas ref={canvasRef} role="img" aria-label={summary || `${plotName}: loading.`} />
        <canvas ref={overlayRef} className="series-overlay" aria-hidden="true" />
        <div ref={liveRef} className="visually-hidden" aria-live="polite" />
        <span id={hintId} hidden>
          Use the arrow keys to move the cursor through the values; Escape clears it.
        </span>
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
