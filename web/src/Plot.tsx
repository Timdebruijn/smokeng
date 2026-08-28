import { useEffect, useRef } from 'react'
import { fetchSeries, type Target } from './api'

const PLOT_HEIGHT = 240

interface Props {
  target: Target
  from: number
  to: number
  refreshKey: number
  logScale: boolean
}

/**
 * One smoke plot. The canvas is transferred to a dedicated worker once; every
 * (range, refresh) change fetches the series and posts it with transferable
 * buffers, so the main thread neither parses nor draws.
 */
export default function Plot({ target, from, to, refreshKey, logScale }: Props) {
  const canvasRef = useRef<HTMLCanvasElement>(null)
  const workerRef = useRef<Worker | null>(null)

  useEffect(() => {
    const canvas = canvasRef.current!
    const worker = new Worker(new URL('./render.worker.ts', import.meta.url), {
      type: 'module',
    })
    const off = canvas.transferControlToOffscreen()
    worker.postMessage({ type: 'init', canvas: off }, [off])
    workerRef.current = worker
    return () => {
      worker.terminate()
      workerRef.current = null
    }
  }, [])

  useEffect(() => {
    let cancelled = false
    const canvas = canvasRef.current!
    const dpr = window.devicePixelRatio || 1
    const w = Math.round(canvas.clientWidth * dpr)
    const h = Math.round(PLOT_HEIGHT * dpr)
    const dark = matchMedia('(prefers-color-scheme: dark)').matches
    fetchSeries(target.id, from, to)
      .then((series) => {
        if (cancelled || !workerRef.current) return
        workerRef.current.postMessage(
          { type: 'render', series, view: { w, h, t0: from, t1: to, dark, log: logScale } },
          [
            series.ts.buffer,
            series.sent.buffer,
            series.received.buffer,
            series.flags.buffer,
            series.offsets.buffer,
            series.values.buffer,
          ],
        )
      })
      .catch((e: Error) => console.error(`plot ${target.path}:`, e))
    return () => {
      cancelled = true
    }
  }, [target.id, target.path, from, to, refreshKey, logScale])

  return (
    <section className="plot">
      <h2>
        {target.title ?? target.path}
        <span className="host">
          {target.host} · {target.address_family}
        </span>
      </h2>
      <canvas ref={canvasRef} style={{ width: '100%', height: PLOT_HEIGHT }} />
    </section>
  )
}
