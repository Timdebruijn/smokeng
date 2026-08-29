/**
 * The shared time cursor (DESIGN.md §8.4). One cursor, every stacked plot —
 * the thing SmokePing structurally cannot do, because each of its graphs is a
 * separate PNG. Deliberately not React state: subscribers draw straight onto
 * their overlay canvas, so a mousemove costs one binary search and one stroke
 * per plot, with no re-render and no density recompute.
 */
type Listener = (t: number | null) => void

const listeners = new Set<Listener>()
let cursor: number | null = null

export function setCursor(t: number | null): void {
  if (t === cursor) return
  cursor = t
  for (const l of listeners) l(cursor)
}


export function subscribeCursor(l: Listener): () => void {
  listeners.add(l)
  l(cursor)
  return () => {
    listeners.delete(l)
  }
}
