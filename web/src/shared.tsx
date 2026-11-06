import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode, type RefObject } from 'react'
import { refreshMs, zoomOut } from './util'

/**
 * The reload / run-a-mutation / busy / error shape the admin screens share.
 * `load` fetches and sets state and may throw; `reload` is the same with a
 * failure shown in `error` and a success clearing it; it runs once on mount.
 * `run` clears the error, does the mutation, reloads, and answers whether the
 * mutation worked.
 */
export function useAction(load: () => Promise<void>) {
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)

  const reload = useCallback(async () => {
    try {
      await load()
      setError(null)
    } catch (e) {
      setError((e as Error).message)
    }
  }, [load])

  useEffect(() => {
    void reload()
  }, [reload])

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

/** An error line that screen readers announce when it appears. */
export function ErrorText({ children, id }: { children: ReactNode; id?: string }) {
  return (
    <p className="error" role="alert" id={id}>
      {children}
    </p>
  )
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
  const confirmRef = useRef<HTMLButtonElement>(null)
  const removeRef = useRef<HTMLButtonElement>(null)
  const moved = useRef(false)
  // The button that had focus is gone after each swap; put focus on its
  // replacement, but not on first render.
  useEffect(() => {
    if (moved.current) (confirming ? confirmRef : removeRef).current?.focus()
    moved.current = true
  }, [confirming])
  return confirming ? (
    <>
      <button
        ref={confirmRef}
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
    <button ref={removeRef} className="pill danger" disabled={busy} onClick={() => setConfirming(true)}>
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

/**
 * A native modal <dialog>: the browser traps focus, makes the page inert and
 * handles Esc. The dialog itself is the full-viewport transparent host, so a
 * click that lands on it rather than on the panel inside is a backdrop click.
 * Focus goes back to whatever had it before the dialog opened.
 */
export function Modal({
  label,
  labelledBy,
  onClose,
  children,
}: {
  label?: string
  labelledBy?: string
  onClose: () => void
  children: ReactNode
}) {
  const ref = useRef<HTMLDialogElement>(null)
  useEffect(() => {
    const d = ref.current!
    const prev = document.activeElement as HTMLElement | null
    d.showModal()
    return () => {
      d.close()
      prev?.focus()
    }
  }, [])
  return (
    <dialog
      ref={ref}
      className="modal-host"
      aria-label={label}
      aria-labelledby={labelledBy}
      onCancel={(e) => {
        e.preventDefault()
        onClose()
      }}
      onClick={(e) => e.target === e.currentTarget && onClose()}
    >
      {children}
    </dialog>
  )
}

/**
 * The time window a graphs screen shows: a trailing window of `rangeS` that
 * moves forward while live, or an explicit window from brush-zoom or a typed
 * range, which pauses it. Picking a range, or zooming out back into the
 * present, drops the explicit window, so it moves again unless live was
 * switched off by hand.
 *
 * The window is frozen per refresh tick, so every plot on the screen shares one
 * time axis — the precondition for the shared crosshair meaning anything.
 */
export function useTimeWindow(initialRangeS = 3600) {
  const [rangeS, setRangeS] = useState(initialRangeS)
  const [refreshKey, setRefreshKey] = useState(0)
  const [live, setLive] = useState(true)
  // Null means a trailing live window. Free ranges are the mechanism, the
  // presets are only sugar over it.
  const [zoom, setZoom] = useState<{ from: number; to: number } | null>(null)

  useEffect(() => {
    if (!live || zoom) return
    const id = setInterval(() => setRefreshKey((k) => k + 1), refreshMs(rangeS))
    return () => clearInterval(id)
  }, [live, zoom, rangeS])

  const [from, to] = useMemo(() => {
    if (zoom) return [zoom.from, zoom.to]
    const now = Math.floor(Date.now() / 1000)
    return [now - rangeS, now]
  }, [rangeS, refreshKey, zoom])

  const pickRange = (seconds: number) => {
    setZoom(null)
    setRangeS(seconds)
  }

  const onZoomOut = () => {
    const next = zoomOut(from, to, Math.floor(Date.now() / 1000))
    if ('rangeS' in next) pickRange(next.rangeS)
    else setZoom(next)
  }

  const toggleLive = () => {
    setZoom(null)
    setLive(!live)
  }

  return { rangeS, from, to, refreshKey, live: live && !zoom, zoom, setZoom, pickRange, onZoomOut, toggleLive }
}
