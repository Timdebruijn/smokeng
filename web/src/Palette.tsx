import { useEffect, useMemo, useRef, useState } from 'react'
import { fetchTargets, type Target } from './api'

export interface PaletteAction {
  id: string
  label: string
  hint: string
  run: () => void
}

/**
 * Go straight to a target, or to a screen, without navigating there.
 *
 * A monitoring tool is read under time pressure, and a tree of any size makes
 * clicking to a target slow exactly when it matters. Everything here is
 * something the caller could already reach: the palette is a shortcut, never a
 * way past a permission.
 */
export default function Palette({
  actions,
  onClose,
  onOpenDetail,
}: {
  actions: PaletteAction[]
  onClose: () => void
  onOpenDetail: (id: number) => void
}) {
  const [query, setQuery] = useState('')
  const [cursor, setCursor] = useState(0)
  const [targets, setTargets] = useState<Target[]>([])
  const input = useRef<HTMLInputElement>(null)

  useEffect(() => {
    input.current?.focus()
    void fetchTargets()
      .then(setTargets)
      .catch(() => setTargets([]))
  }, [])

  const items = useMemo(() => {
    const q = query.trim().toLowerCase()
    const targetItems: PaletteAction[] = targets
      .filter((t) => t.host !== null)
      .map((t) => ({
        id: `target-${t.id}`,
        label: t.title ?? t.path,
        hint: t.host ?? '',
        run: () => onOpenDetail(t.id),
      }))
    const all = [...actions, ...targetItems]
    if (q === '') return all.slice(0, 12)
    return all
      .filter((a) => a.label.toLowerCase().includes(q) || a.hint.toLowerCase().includes(q))
      .slice(0, 12)
  }, [query, targets, actions, onOpenDetail])

  useEffect(() => setCursor(0), [query])

  const choose = (a: PaletteAction | undefined) => {
    if (!a) return
    a.run()
    onClose()
  }

  return (
    <div className="modal-backdrop" onClick={onClose}>
      <div className="palette" onClick={(e) => e.stopPropagation()}>
        <input
          ref={input}
          value={query}
          placeholder="Jump to a target or a screen…"
          onChange={(e) => setQuery(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === 'ArrowDown') {
              e.preventDefault()
              setCursor((c) => Math.min(c + 1, items.length - 1))
            } else if (e.key === 'ArrowUp') {
              e.preventDefault()
              setCursor((c) => Math.max(c - 1, 0))
            } else if (e.key === 'Enter') {
              e.preventDefault()
              choose(items[cursor])
            } else if (e.key === 'Escape') {
              onClose()
            }
          }}
        />
        {items.length === 0 ? (
          <p className="hint small palette-empty">Nothing matches.</p>
        ) : (
          items.map((a, i) => (
            <button
              key={a.id}
              className={i === cursor ? 'palette-item active' : 'palette-item'}
              onMouseEnter={() => setCursor(i)}
              onClick={() => choose(a)}
            >
              <span>{a.label}</span>
              <span className="hint small mono">{a.hint}</span>
            </button>
          ))
        )}
      </div>
    </div>
  )
}
