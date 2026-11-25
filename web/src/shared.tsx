import { useCallback, useEffect, useId, useMemo, useRef, useState, useSyncExternalStore, type ReactNode, type RefObject } from 'react'
import { csvCell, refreshMs, zoomOut } from './util'

/**
 * The reload / run-a-mutation / busy / error shape the admin screens share.
 * `load` fetches and sets state and may throw; `reload` is the same with a
 * failure shown in `error` and a success clearing it; it runs once on mount.
 * `run` clears the error, does the mutation, reloads, and answers whether the
 * mutation worked.
 */
/**
 * A ref for a header popover, which hangs from the right edge of its button.
 * That is right while the button sits at the right of the bar. Once the bar
 * wraps, the button can start a second row at the left, and the popover would
 * open past the left edge of the window, where nothing can scroll to it. This
 * moves it back in by exactly what stuck out.
 */
export function keepInView(el: HTMLElement | null) {
  if (!el) return
  const over = 8 - el.getBoundingClientRect().left
  if (over > 0) el.style.right = `${-over}px`
}

/**
 * Text that explains a label or badge, reachable by keyboard and touch where a
 * title attribute is not. The children become a button that opens a native
 * popover; the browser centres it in the window, so it is never cut off, and
 * handles Esc and a press outside. `className` styles the button.
 */
export function Hint({ text, className, children }: { text: string; className?: string; children: ReactNode }) {
  const id = useId()
  return (
    <>
      <button type="button" className={className ? `hint-button ${className}` : 'hint-button'} popoverTarget={id}>
        {children}
      </button>
      <div id={id} popover="auto" className="hint-pop">
        {text}
      </div>
    </>
  )
}

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

  // Runs a change and reloads; hands the failure to the caller, so a form can
  // show it next to its own button.
  const attempt = useCallback(
    async (fn: () => Promise<unknown>) => {
      setBusy(true)
      try {
        await fn()
        await reload()
        return null
      } catch (e) {
        return (e as Error).message
      } finally {
        setBusy(false)
      }
    },
    [reload],
  )

  // The same, for everything that is not a form: the failure goes to the
  // screen-level error.
  const run = useCallback(
    async (fn: () => Promise<unknown>) => {
      setError(null)
      const failure = await attempt(fn)
      setError(failure)
      return failure === null
    },
    [attempt],
  )

  return { error, busy, reload, run, attempt }
}

let announcement = ''
const announcers = new Set<() => void>()
const setAnnouncement = (text: string) => {
  announcement = text
  announcers.forEach((f) => f())
}

/**
 * Say something to screen readers without moving focus. The text is cleared
 * and set a tick later, so the same message twice is read twice.
 */
export function announce(text: string) {
  setAnnouncement('')
  setTimeout(() => setAnnouncement(text), 50)
}

/**
 * The live region `announce` writes to. The page renders one, always, and so
 * does every modal: an open modal makes the rest of the page inert, and a
 * status region in inert content is not read out.
 */
export function Announcer() {
  const text = useSyncExternalStore(
    (f) => {
      announcers.add(f)
      return () => announcers.delete(f)
    },
    () => announcement,
  )
  return (
    <div role="status" className="visually-hidden">
      {text}
    </div>
  )
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
      announce('Copied to clipboard')
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
  label = 'Remove',
  confirmLabel = `Confirm ${label.toLowerCase()}`,
  className = 'pill',
}: {
  busy: boolean
  onConfirm: () => Promise<boolean>
  label?: string
  confirmLabel?: string
  /** Base class of both buttons; `danger` is added to the destructive one. */
  className?: string
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
        className={`${className} danger`.trim()}
        disabled={busy}
        onClick={() => void onConfirm().then((ok) => ok && setConfirming(false))}
      >
        {confirmLabel}
      </button>
      <button className={className || undefined} disabled={busy} onClick={() => setConfirming(false)}>
        Cancel
      </button>
    </>
  ) : (
    <button ref={removeRef} className={`${className} danger`.trim()} disabled={busy} onClick={() => setConfirming(true)}>
      {label}
    </button>
  )
}

/** The figure tile on the overview and detail screens. */
export function Stat({
  label,
  value,
  sub,
  hint,
  tone,
}: {
  label: string
  value: string
  sub?: string
  hint?: string
  tone?: 'bad' | 'warn'
}) {
  return (
    <div className="card stat">
      <div className="stat-label">{hint ? <Hint text={hint}>{label}</Hint> : label}</div>
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
      <Announcer />
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
    announce(live ? 'Paused' : 'Live')
    setLive(!live)
  }

  return { rangeS, from, to, refreshKey, live: live && !zoom, zoom, setZoom, pickRange, onZoomOut, toggleLive }
}

/** The live/paused toggle every self-refreshing screen shows (WCAG 2.2.2). */
export function LivePill({ live, onToggle }: { live: boolean; onToggle: () => void }) {
  return (
    <button className={live ? 'pill active' : 'pill'} aria-pressed={live} onClick={onToggle}>
      {live ? '● live' : '○ paused'}
    </button>
  )
}

/**
 * Calls `fn` every `ms` while live; resuming calls it once at once. `fn` must
 * be stable (useCallback), or the timer restarts on every render. Not called
 * on mount: the screens load themselves.
 */
export function useLiveRefresh(fn: () => void, ms: number) {
  const [live, setLive] = useState(true)
  useEffect(() => {
    if (!live) return
    const id = setInterval(fn, ms)
    return () => clearInterval(id)
  }, [live, fn, ms])
  const toggleLive = () => {
    announce(live ? 'Paused' : 'Live')
    setLive(!live)
    if (!live) fn()
  }
  return { live, toggleLive }
}

/**
 * A status dot whose colour carries a state: the same span as before, plus the
 * state in words for assistive technology. A dot beside text that already says
 * it stays a bare span with aria-hidden.
 */
export function StatusDot({ className = 'dot', background, label }: { className?: string; background?: string; label: string }) {
  return (
    <>
      <span className={className} style={background ? { background } : undefined} />
      <span className="visually-hidden">{label}</span>
    </>
  )
}

/** Offers rows as a CSV file download. */
export function downloadCSV(filename: string, rows: (string | number)[][]) {
  const text = rows.map((r) => r.map(csvCell).join(',')).join('\n')
  const url = URL.createObjectURL(new Blob([text], { type: 'text/csv' }))
  const a = document.createElement('a')
  a.href = url
  a.download = filename
  a.click()
  URL.revokeObjectURL(url)
}
