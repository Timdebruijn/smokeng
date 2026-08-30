import { useCallback, useEffect, useState } from 'react'

/**
 * Presentation settings that belong to the person reading the graphs rather
 * than to the installation: contrast, text size, and a colour-blind-safe
 * palette.
 *
 * They live on this device and are deliberately not a server-side preference.
 * Someone reading smokeng on a projector in a meeting room, or on a laptop in
 * daylight, needs a different rendering from the same account's usual one, and
 * a setting that followed the account would fight them.
 *
 * The colour-blind option matters more here than in most applications. This UI
 * spends red and green on "lossy" and "healthy" — the single most common
 * confusion — so without an alternative palette the loss rail says nothing to
 * roughly one man in twelve.
 */
export interface A11yState {
  contrast: boolean
  largeText: boolean
  colourSafe: boolean
}

const STORAGE_KEY = 'smokeng.a11y'
const DEFAULTS: A11yState = { contrast: false, largeText: false, colourSafe: false }

function load(): A11yState {
  try {
    const raw = localStorage.getItem(STORAGE_KEY)
    if (!raw) return DEFAULTS
    const parsed = JSON.parse(raw) as Partial<A11yState>
    return {
      contrast: parsed.contrast === true,
      largeText: parsed.largeText === true,
      colourSafe: parsed.colourSafe === true,
    }
  } catch {
    // A private window, cleared site data, or storage the browser refuses.
    // None of those is a reason to fail to render.
    return DEFAULTS
  }
}

/**
 * Applies the settings as attributes on <html>, which is where the stylesheet
 * looks for them, and persists them.
 *
 * The attributes go on the document element rather than on a wrapper so the
 * text-size zoom takes the whole page with it — including anything rendered
 * into a portal or fixed to the viewport, which a wrapper would leave behind
 * at the original size.
 */
export function useA11y(): [A11yState, (k: keyof A11yState) => void] {
  const [state, setState] = useState<A11yState>(load)

  useEffect(() => {
    const el = document.documentElement
    const set = (attr: string, on: boolean, value: string) => {
      if (on) el.setAttribute(attr, value)
      else el.removeAttribute(attr)
    }
    set('data-contrast', state.contrast, 'high')
    set('data-textsize', state.largeText, 'large')
    set('data-cb', state.colourSafe, 'safe')
    try {
      localStorage.setItem(STORAGE_KEY, JSON.stringify(state))
    } catch {
      // Not being able to remember the choice is survivable; not being able to
      // make it would not be, so the attributes are set either way.
    }
  }, [state])

  const toggle = useCallback((k: keyof A11yState) => {
    setState((s) => ({ ...s, [k]: !s[k] }))
  }, [])

  return [state, toggle]
}

const ROWS: { key: keyof A11yState; label: string; hint: string }[] = [
  {
    key: 'contrast',
    label: 'Higher contrast',
    hint: 'Darker text and stronger borders, and no shadows to soften them.',
  },
  {
    key: 'largeText',
    label: 'Larger text',
    hint: 'Scales the whole interface, so the graphs grow with it.',
  },
  {
    key: 'colourSafe',
    label: 'Colour-blind safe palette',
    hint: 'Replaces the red and green of loss and health with blue and orange.',
  },
]

export default function A11yMenu({
  state,
  onToggle,
  open,
  onOpenChange,
}: {
  state: A11yState
  onToggle: (k: keyof A11yState) => void
  open: boolean
  onOpenChange: (open: boolean) => void
}) {
  return (
    <div className="a11y">
      <button
        className="icon-button"
        title="Accessibility options"
        aria-label="Accessibility options"
        aria-expanded={open}
        onClick={() => onOpenChange(!open)}
      >
        Aa
      </button>
      {open && (
        <div className="popover a11y-popover">
          <p className="popover-title">Accessibility</p>
          {ROWS.map((r) => (
            <label key={r.key} className="a11y-row">
              <input type="checkbox" checked={state[r.key]} onChange={() => onToggle(r.key)} />
              <span>
                <span className="a11y-label">{r.label}</span>
                <span className="hint small">{r.hint}</span>
              </span>
            </label>
          ))}
          <p className="hint small a11y-note">Saved on this device.</p>
        </div>
      )}
    </div>
  )
}
