import { useCallback, useEffect, useRef, useState } from 'react'
import { fetchPathChanges, fetchSeries, icmpErrorName, type PathChange, type Target } from './api'
import { setCursor, subscribeCursor } from './crosshair'
import { AXIS_W, PLOT_HEIGHT, densityHeight, fmtClock, fmtUs, inferSpan } from './layout'

interface Props {
  target: Target
  /** Which agent measured this series; 0 is the master's own prober. */
  agentId: number
  /** Shown only when more than one agent measures the target. */
  agentName?: string
  from: number
  to: number
  refreshKey: number
  logScale: boolean
  onZoom: (from: number, to: number) => void
}

/**
 * A per-row summary kept on the main thread for crosshair readouts. The bulk
 * buffers are transferred to the worker (and neutered here), so this small
 * index — built before the transfer — is what the cursor reads: O(log n) per
 * mousemove, no density recompute.
 */
interface RowIndex {
  ts: Float64Array
  median: Float64Array
  loss: Float64Array
  sent: Float64Array
  received: Float64Array
  icmpErrors: (number | null)[]
  span: number
}

/**
 * Measurement-quality flags (store.Flag* in Go). A measurement is only worth
 * as much as the conditions it was taken under, so anything that weakens it
 * is surfaced next to the plot instead of being left to be inferred.
 */
const FLAGS: { bit: number; label: string; title: string }[] = [
  { bit: 1 << 0, label: 'userspace TX', title: 'Send timestamps taken in userspace; scheduler jitter widens the smoke.' },
  { bit: 1 << 1, label: 'userspace RX', title: 'Receive timestamps taken in userspace; scheduler jitter widens the smoke.' },
  { bit: 1 << 2, label: 'raw socket', title: 'Unprivileged datagram ICMP was unavailable; running on raw sockets.' },
  {
    bit: 1 << 3,
    label: 'dropped replies',
    title: 'The receive queue overflowed: some loss shown here is ours, not the network’s.',
  },
  {
    bit: 1 << 5,
    label: 'clock step',
    title: 'The wall clock jumped during these intervals; affected RTTs are unreliable.',
  },
  {
    bit: 1 << 6,
    label: 'send refused',
    title: 'The local stack would not transmit these probes — no route, or a local firewall rule.',
  },
]
const FLAG_ICMP_ERROR = 1 << 4

function flagCounts(
  flags: Uint8Array,
  icmpErrors: (number | null)[],
): { label: string; title: string; count: number }[] {
  const out = FLAGS.map((f) => {
    let count = 0
    for (let i = 0; i < flags.length; i++) if (flags[i] & f.bit) count++
    return { label: f.label, title: f.title, count }
  }).filter((f) => f.count > 0)

  // ICMP errors get named rather than lumped together: "host prohibited" and
  // "TTL exceeded" call for different responses, and both differ from
  // silence.
  const byError = new Map<number, number>()
  for (let i = 0; i < flags.length; i++) {
    if (!(flags[i] & FLAG_ICMP_ERROR)) continue
    const e = icmpErrors[i]
    if (e !== null) byError.set(e, (byError.get(e) ?? 0) + 1)
  }
  for (const [packed, count] of [...byError].sort((a, b) => b[1] - a[1])) {
    out.push({
      label: icmpErrorName(packed),
      title: 'Probes were refused with this ICMP error rather than going unanswered.',
      count,
    })
  }
  return out
}

/**
 * One smoke plot: a worker-owned canvas for the density, plus a main-thread
 * overlay canvas for the shared crosshair and the brush selection, so
 * interaction never waits on a render (DESIGN.md §8.2, §8.4).
 */
export default function Plot({
  target,
  agentId,
  agentName,
  from,
  to,
  refreshKey,
  logScale,
  onZoom,
}: Props) {
  const canvasRef = useRef<HTMLCanvasElement>(null)
  const overlayRef = useRef<HTMLCanvasElement>(null)
  const stackRef = useRef<HTMLDivElement>(null)
  const workerRef = useRef<Worker | null>(null)
  const rowsRef = useRef<RowIndex | null>(null)
  const cursorRef = useRef<number | null>(null)
  const brushRef = useRef<{ t0: number; t1: number } | null>(null)
  const pathsRef = useRef<PathChange[]>([])
  const draggingRef = useRef(false)
  const rangeRef = useRef({ from, to })
  rangeRef.current = { from, to }
  const [quality, setQuality] = useState<{ label: string; title: string; count: number }[]>([])
  const [rowCount, setRowCount] = useState(0)
  // The plot's own width, observed rather than read once. Reading clientWidth
  // when the effect first runs can catch the element before layout and bake a
  // zero into the render, which draws nothing at all and never recovers.
  // Observing it also makes the density redraw when the window is resized,
  // which it previously did not.
  const [width, setWidth] = useState(0)

  // Pixel ↔ time mapping, in CSS pixels over the density area only.
  const xToTime = useCallback((xCss: number, widthCss: number): number | null => {
    const plotW = widthCss - AXIS_W
    if (plotW <= 0 || xCss < AXIS_W) return null
    const { from: f, to: t } = rangeRef.current
    return f + ((xCss - AXIS_W) / plotW) * (t - f)
  }, [])

  const timeToX = useCallback((t: number, widthCss: number): number => {
    const plotW = widthCss - AXIS_W
    const { from: f, to: tt } = rangeRef.current
    return AXIS_W + ((t - f) / (tt - f)) * plotW
  }, [])

  const drawOverlay = useCallback(() => {
    const cv = overlayRef.current
    if (!cv) return
    const dpr = window.devicePixelRatio || 1
    const cssW = cv.clientWidth
    const cssH = PLOT_HEIGHT
    if (cv.width !== Math.round(cssW * dpr) || cv.height !== Math.round(cssH * dpr)) {
      cv.width = Math.round(cssW * dpr)
      cv.height = Math.round(cssH * dpr)
    }
    const ctx = cv.getContext('2d')!
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0)
    ctx.clearRect(0, 0, cssW, cssH)
    const plotH = densityHeight(cssH)
    const dark = matchMedia('(prefers-color-scheme: dark)').matches

    // Brush selection
    const brush = brushRef.current
    if (brush) {
      const x0 = timeToX(Math.min(brush.t0, brush.t1), cssW)
      const x1 = timeToX(Math.max(brush.t0, brush.t1), cssW)
      ctx.fillStyle = dark ? 'rgba(125,211,252,0.16)' : 'rgba(3,105,161,0.14)'
      ctx.fillRect(x0, 0, x1 - x0, plotH)
      ctx.strokeStyle = dark ? 'rgba(125,211,252,0.7)' : 'rgba(3,105,161,0.7)'
      ctx.lineWidth = 1
      ctx.beginPath()
      ctx.moveTo(x0 + 0.5, 0)
      ctx.lineTo(x0 + 0.5, plotH)
      ctx.moveTo(x1 + 0.5, 0)
      ctx.lineTo(x1 + 0.5, plotH)
      ctx.stroke()
    }

    // Path changes: a mark on the time axis, so "the path changed at 14:02"
    // sits beside "the smoke widened at 14:03" without a second view.
    for (const change of pathsRef.current) {
      const x = timeToX(change.ts, cssW)
      if (x < AXIS_W || x > cssW) continue
      ctx.strokeStyle = dark ? 'rgba(217,119,6,0.85)' : 'rgba(180,83,9,0.8)'
      ctx.lineWidth = 1
      ctx.setLineDash([3, 3])
      ctx.beginPath()
      ctx.moveTo(Math.round(x) + 0.5, 0)
      ctx.lineTo(Math.round(x) + 0.5, plotH)
      ctx.stroke()
      ctx.setLineDash([])
    }

    // Shared crosshair
    const t = cursorRef.current
    if (t === null) return
    const x = timeToX(t, cssW)
    if (x < AXIS_W || x > cssW) return
    ctx.strokeStyle = dark ? 'rgba(255,255,255,0.45)' : 'rgba(0,0,0,0.4)'
    ctx.lineWidth = 1
    ctx.beginPath()
    ctx.moveTo(Math.round(x) + 0.5, 0)
    ctx.lineTo(Math.round(x) + 0.5, plotH)
    ctx.stroke()

    // Readout for the interval under the cursor
    const rows = rowsRef.current
    if (!rows || rows.ts.length === 0) return
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
    if (idx < 0 || t > rows.ts[idx] + rows.span) return

    const lines = [
      fmtClock(rows.ts[idx]),
      Number.isNaN(rows.median[idx]) ? 'no reply' : `median ${fmtUs(rows.median[idx])}`,
      `${rows.received[idx]}/${rows.sent[idx]} replies${rows.loss[idx] > 0 ? ` · ${Math.round(rows.loss[idx] * 100)}% loss` : ''}`,
    ]
    const err = rows.icmpErrors[idx]
    if (err !== null && err !== undefined) lines.push(icmpErrorName(err))
    // The route in force at this instant, so a step in the smoke can be read
    // against the path that carried it.
    const inForce = pathsRef.current.filter((c) => c.ts <= t).at(-1)
    if (inForce) {
      const named = inForce.hops.filter((h) => h !== '*')
      lines.push(`path: ${named.length ? named.join(' → ') : 'no hops answered'}`)
    }
    ctx.font = '11px system-ui, sans-serif'
    const boxW = Math.max(...lines.map((l) => ctx.measureText(l).width)) + 14
    const boxH = lines.length * 14 + 10
    const bx = x + 8 + boxW > cssW ? x - 8 - boxW : x + 8
    ctx.fillStyle = dark ? 'rgba(20,20,22,0.92)' : 'rgba(255,255,255,0.94)'
    ctx.strokeStyle = dark ? 'rgba(255,255,255,0.16)' : 'rgba(0,0,0,0.14)'
    ctx.beginPath()
    ctx.roundRect(bx, 6, boxW, boxH, 5)
    ctx.fill()
    ctx.stroke()
    ctx.fillStyle = dark ? '#e6e6e6' : '#1a1a1a'
    ctx.textAlign = 'left'
    ctx.textBaseline = 'top'
    lines.forEach((l, i) => ctx.fillText(l, bx + 7, 12 + i * 14))
  }, [timeToX])

  useEffect(() => {
    const el = stackRef.current
    if (!el) return
    const ro = new ResizeObserver(([entry]) => {
      setWidth(Math.round(entry.contentRect.width))
    })
    ro.observe(el)
    setWidth(el.clientWidth)
    return () => ro.disconnect()
  }, [])

  // One worker per plot, owning its density canvas for the component's life.
  useEffect(() => {
    const canvas = canvasRef.current!
    const worker = new Worker(new URL('./render.worker.ts', import.meta.url), {
      type: 'module',
    })
    // A worker that dies takes the plot with it and says nothing, which is
    // indistinguishable from "no data". Say it out loud instead.
    worker.onerror = (e) => {
      console.error(`plot ${target.path}: render worker failed:`, e.message || e)
    }
    worker.onmessageerror = () => {
      console.error(`plot ${target.path}: render worker could not read a message`)
    }
    const off = canvas.transferControlToOffscreen()
    worker.postMessage({ type: 'init', canvas: off }, [off])
    workerRef.current = worker
    return () => {
      worker.terminate()
      workerRef.current = null
    }
  }, [])

  // Redraw the overlay whenever the shared cursor moves — no React re-render.
  useEffect(() => {
    return subscribeCursor((t) => {
      cursorRef.current = t
      drawOverlay()
    })
  }, [drawOverlay])

  useEffect(() => {
    let cancelled = false
    const dpr = window.devicePixelRatio || 1
    const dark = matchMedia('(prefers-color-scheme: dark)').matches
    fetchSeries(target.id, agentId, from, to)
      .then((series) => {
        if (cancelled || !workerRef.current) return
        const n = series.ts.length
        const rows: RowIndex = {
          ts: series.ts.slice(),
          median: new Float64Array(n),
          loss: new Float64Array(n),
          sent: series.sent.slice(),
          received: series.received.slice(),
          icmpErrors: series.icmpErrors,
          span: inferSpan(series.ts, n),
        }
        for (let i = 0; i < n; i++) {
          const a = series.offsets[i]
          const b = series.offsets[i + 1]
          // Samples are stored sorted ascending, so the median is a lookup.
          rows.median[i] = b > a ? series.values[a + ((b - a) >> 1)] : NaN
          rows.loss[i] = series.sent[i] > 0 ? 1 - series.received[i] / series.sent[i] : NaN
        }
        // Measure here rather than when the effect started: at effect time
        // the element may not have been laid out, and a zero width renders an
        // empty plot that never recovers. `width` only drives re-renders on
        // resize; the live measurement is what gets drawn.
        const cssW = stackRef.current?.clientWidth || width
        if (cssW === 0) return
        rowsRef.current = rows
        setQuality(flagCounts(series.flags, series.icmpErrors))
        setRowCount(n)
        workerRef.current.postMessage(
          {
            type: 'render',
            series,
            view: { cssW, cssH: PLOT_HEIGHT, dpr, t0: from, t1: to, dark, log: logScale },
          },
          [
            series.ts.buffer,
            series.sent.buffer,
            series.received.buffer,
            series.flags.buffer,
            series.offsets.buffer,
            series.values.buffer,
          ],
        )
        drawOverlay()
      })
      .catch((e: Error) => console.error(`plot ${target.path}:`, e))

    // Route changes are a separate, much smaller fetch; a failure here must
    // not stop the smoke rendering.
    fetchPathChanges(target.id, agentId, from, to)
      .then((changes) => {
        if (cancelled) return
        pathsRef.current = changes
        drawOverlay()
      })
      .catch(() => {
        pathsRef.current = []
      })
    return () => {
      cancelled = true
    }
  }, [target.id, target.path, agentId, from, to, refreshKey, logScale, width, drawOverlay])

  const onMove = (e: React.MouseEvent<HTMLDivElement>) => {
    const rect = e.currentTarget.getBoundingClientRect()
    const t = xToTime(e.clientX - rect.left, rect.width)
    if (draggingRef.current && brushRef.current && t !== null) {
      brushRef.current.t1 = t
      drawOverlay()
    }
    setCursor(t)
  }

  const onDown = (e: React.MouseEvent<HTMLDivElement>) => {
    const rect = e.currentTarget.getBoundingClientRect()
    const t = xToTime(e.clientX - rect.left, rect.width)
    if (t === null) return
    draggingRef.current = true
    brushRef.current = { t0: t, t1: t }
  }

  const onUp = () => {
    const brush = brushRef.current
    draggingRef.current = false
    brushRef.current = null
    drawOverlay()
    if (!brush) return
    const lo = Math.min(brush.t0, brush.t1)
    const hi = Math.max(brush.t0, brush.t1)
    // Ignore an accidental click; require a real drag (≥ 5 seconds).
    if (hi - lo >= 5) onZoom(Math.floor(lo), Math.ceil(hi))
  }

  const onLeave = () => {
    draggingRef.current = false
    brushRef.current = null
    setCursor(null)
  }

  return (
    <section className="card plot">
      <div className="plot-head">
        <span className="dot" style={{ background: dotColour(quality.length) }} />
        <span className="plot-title">{target.title ?? target.path}</span>
        <span className="host">
          {target.host} · {target.address_family}
        </span>
        {agentName && <span className="agent">from {agentName}</span>}
        {quality.map((q) => (
          <span key={q.label} className="quality" title={q.title}>
            {q.label}
            {rowCount > 0 && q.count < rowCount && <> ({q.count}/{rowCount})</>}
          </span>
        ))}
      </div>
      <div
        ref={stackRef}
        className="plot-stack"
        style={{ height: PLOT_HEIGHT }}
        onMouseMove={onMove}
        onMouseDown={onDown}
        onMouseUp={onUp}
        onMouseLeave={onLeave}
      >
        <canvas ref={canvasRef} style={{ width: '100%', height: PLOT_HEIGHT }} />
        <canvas ref={overlayRef} className="overlay" style={{ width: '100%', height: PLOT_HEIGHT }} />
      </div>
    </section>
  )
}

// The status dot beside a plot's name. It reports whether smokeng could measure
// cleanly, not whether the network is fast: a widened band with a flag on it
// has an attributable cause, and that is what the colour is for.
function dotColour(flagged: number): string {
  return flagged === 0 ? 'var(--good)' : 'var(--warn)'
}
