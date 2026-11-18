import { useState } from 'react'
import { parseFamilyChoice, type FamilyChoice } from './util'

const STORAGE_KEY = 'smokeng.family'

function load(): FamilyChoice {
  try {
    return parseFamilyChoice(localStorage.getItem(STORAGE_KEY))
  } catch {
    // A private window or blocked site data: show everything, as a first visit does.
    return 'both'
  }
}

/**
 * Which address families the graphs and rows show. Remembered in this browser
 * only, like the accessibility options: it is a way of reading, not a setting
 * of what is measured.
 */
export function useFamilyChoice(): [FamilyChoice, (c: FamilyChoice) => void] {
  const [choice, setChoice] = useState<FamilyChoice>(load)
  const set = (c: FamilyChoice) => {
    setChoice(c)
    try {
      localStorage.setItem(STORAGE_KEY, c)
    } catch {
      // Not remembering the choice is survivable; not making it would not be.
    }
  }
  return [choice, set]
}

const CHOICES: { value: FamilyChoice; label: string }[] = [
  { value: 'v4', label: 'v4' },
  { value: 'v6', label: 'v6' },
  { value: 'both', label: 'v4 + v6' },
]

export default function FamilyFilter({
  choice,
  onChange,
}: {
  choice: FamilyChoice
  onChange: (c: FamilyChoice) => void
}) {
  // Two forms of one control, and the stylesheet shows one of them: three
  // buttons where the header has room, a select where it has not. On a phone
  // the buttons pushed the header onto a third row. The one that is not shown
  // is display: none, so it is not in the tab order or the accessibility tree.
  return (
    <>
      <div className="family-filter" role="group" aria-label="Address family shown">
        {CHOICES.map((c) => (
          <button
            key={c.value}
            type="button"
            className={choice === c.value ? 'pill active' : 'pill'}
            aria-pressed={choice === c.value}
            onClick={() => onChange(c.value)}
          >
            {c.label}
          </button>
        ))}
      </div>
      <select
        className="family-select"
        aria-label="Address family shown"
        value={choice}
        onChange={(e) => onChange(parseFamilyChoice(e.target.value))}
      >
        {CHOICES.map((c) => (
          <option key={c.value} value={c.value}>
            {c.label}
          </option>
        ))}
      </select>
    </>
  )
}
