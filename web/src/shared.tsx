import { useCallback, useEffect, useRef, useState, type RefObject } from 'react'

/**
 * The reload / run-a-mutation / busy / error shape the admin screens share.
 * `load` fetches and sets state and may throw; `reload` is the same with a
 * failure shown in `error`. `run` clears the error, does the mutation, reloads,
 * and answers whether the mutation worked.
 */
export function useAction(load: () => Promise<void>) {
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)

  const reload = useCallback(async () => {
    try {
      await load()
    } catch (e) {
      setError((e as Error).message)
    }
  }, [load])

  const run = useCallback(
    async (fn: () => Promise<unknown>) => {
      setBusy(true)
      setError(null)
      try {
        await fn()
        await reload()
        return true
      } catch (e) {
        setError((e as Error).message)
        return false
      } finally {
        setBusy(false)
      }
    },
    [reload],
  )

  return { error, busy, reload, run }
}

/** Copy `text`, and say so for two seconds. */
export function useCopy(text: string): [boolean, () => Promise<void>] {
  const [copied, setCopied] = useState(false)
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(text)
      setCopied(true)
      setTimeout(() => setCopied(false), 2000)
    } catch {
      // Clipboard access can be refused; the text is selectable either way.
    }
  }
  return [copied, copy]
}

/**
 * An element's width, observed rather than read once. Reading clientWidth when
 * an effect first runs can catch the element before layout and bake a zero
 * into a render, which draws nothing and never recovers; observing it also
 * makes a resize redraw.
 */
export function useWidth(ref: RefObject<HTMLElement | null>): number {
  const [width, setWidth] = useState(0)
  useEffect(() => {
    const el = ref.current
    if (!el) return
    const ro = new ResizeObserver(([entry]) => setWidth(Math.round(entry.contentRect.width)))
    ro.observe(el)
    setWidth(el.clientWidth)
    return () => ro.disconnect()
  }, [ref])
  return width
}

/**
 * One render worker owning `canvasRef` for the component's life. A canvas can
 * only be transferred once, so the worker is made once and never re-created
 * for a range change. A worker that dies takes the plot with it and says
 * nothing, which is indistinguishable from "no data", so it says so here.
 */
export function useRenderWorker(canvasRef: RefObject<HTMLCanvasElement | null>, label: string) {
  const workerRef = useRef<Worker | null>(null)
  useEffect(() => {
    const canvas = canvasRef.current
    if (!canvas) return
    const worker = new Worker(new URL('./render.worker.ts', import.meta.url), { type: 'module' })
    worker.onerror = (e) => console.error(`${label}: render worker failed:`, e.message || e)
    worker.onmessageerror = () => console.error(`${label}: render worker could not read a message`)
    const off = canvas.transferControlToOffscreen()
    worker.postMessage({ type: 'init', canvas: off }, [off])
    workerRef.current = worker
    return () => {
      worker.terminate()
      workerRef.current = null
    }
  }, [])
  return workerRef
}

/** Remove, then confirm or cancel: a delete is never one click. */
export function ConfirmButton({
  busy,
  onConfirm,
}: {
  busy: boolean
  onConfirm: () => Promise<boolean>
}) {
  const [confirming, setConfirming] = useState(false)
  return confirming ? (
    <>
      <button
        className="pill danger"
        disabled={busy}
        onClick={() => void onConfirm().then((ok) => ok && setConfirming(false))}
      >
        Confirm remove
      </button>
      <button className="pill" disabled={busy} onClick={() => setConfirming(false)}>
        Cancel
      </button>
    </>
  ) : (
    <button className="pill danger" disabled={busy} onClick={() => setConfirming(true)}>
      Remove
    </button>
  )
}

/** The figure tile on the overview and detail screens. */
export function Stat({
  label,
  value,
  sub,
  title,
  tone,
}: {
  label: string
  value: string
  sub?: string
  title?: string
  tone?: 'bad' | 'warn'
}) {
  return (
    <div className="card stat" title={title}>
      <p className="stat-label">{label}</p>
      <p className="stat-value">
        <span className={tone}>{value}</span>
        {sub && <span className="stat-sub">{sub}</span>}
      </p>
    </div>
  )
}
