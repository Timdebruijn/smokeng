import { useEffect, useState } from 'react'
import { exportConfig, importConfig } from './api'

/**
 * The target tree as text, in both directions.
 *
 * Import is declarative and never prunes: anything absent from the file is
 * disabled, which importing the file again undoes. Deleting is what --prune
 * does, and that stays on the command line where the operator can read a
 * summary before running it for real.
 */
export default function TomlDialog({
  mode,
  onClose,
  onImported,
}: {
  mode: 'export' | 'import'
  onClose: () => void
  onImported: () => void
}) {
  const [text, setText] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [result, setResult] = useState<string | null>(null)
  const [copied, setCopied] = useState(false)

  useEffect(() => {
    if (mode !== 'export') return
    void exportConfig()
      .then(setText)
      .catch((e) => setError((e as Error).message))
  }, [mode])

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => e.key === 'Escape' && onClose()
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [onClose])

  const doImport = async () => {
    setBusy(true)
    setError(null)
    setResult(null)
    try {
      const r = await importConfig(text)
      setResult([r.summary, ...(r.warnings ?? []).map((w) => `warning: ${w}`)].join('\n'))
      onImported()
    } catch (e) {
      setError((e as Error).message)
    } finally {
      setBusy(false)
    }
  }

  const copy = async () => {
    try {
      await navigator.clipboard.writeText(text)
      setCopied(true)
      setTimeout(() => setCopied(false), 2000)
    } catch {
      // Clipboard access can be refused; the text is selectable either way.
    }
  }

  return (
    <div className="modal-backdrop" onClick={onClose}>
      <div className="modal" onClick={(e) => e.stopPropagation()}>
        <h2>{mode === 'export' ? 'Export as TOML' : 'Import TOML'}</h2>
        <p className="hint small">
          {mode === 'export'
            ? 'The whole tree, including alert rules and grants. This is what config export writes, so it round-trips.'
            : 'Applied declaratively to the whole tree: anything absent is disabled, never deleted. Use config import --prune on the command line if you mean to delete.'}
        </p>
        <textarea
          value={text}
          readOnly={mode === 'export'}
          spellCheck={false}
          onChange={(e) => setText(e.target.value)}
          placeholder={mode === 'import' ? 'Paste a targets.toml here…' : ''}
        />
        {error && <p className="error">{error}</p>}
        {result && <pre className="import-result">{result}</pre>}
        <div className="pill-row">
          {mode === 'import' ? (
            <button className="pill accent" disabled={busy || text.trim() === ''} onClick={() => void doImport()}>
              {busy ? 'Applying…' : 'Import'}
            </button>
          ) : (
            <button className="pill accent" onClick={() => void copy()}>
              {copied ? 'Copied' : 'Copy'}
            </button>
          )}
          <button className="pill" onClick={onClose}>
            Close
          </button>
        </div>
      </div>
    </div>
  )
}
