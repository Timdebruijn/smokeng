import { useEffect, useId, useRef, useState } from 'react'
import { deleteIrttKey, fetchIrttKey, putIrttKey, type IrttKey } from './api'
import { announce, ConfirmButton, ErrorText } from './shared'

// The HMAC key an irtt server is started with (`irtt server --hmac=<key>`).
// It belongs to the server, so it is keyed on host:port: two targets measuring
// the same server share one key. Only the master's own probes use a key set
// here; an agent reads its own keyfile.

/** The line to start the server with, quoted for a shell when the key needs it. */
function serverCommand(key: string): string {
  const arg = /^[A-Za-z0-9._~+/=-]+$/.test(key) ? key : `'${key.replace(/'/g, `'\\''`)}'`
  return `irtt server --hmac=${arg}`
}

export default function IrttKeyPanel({ endpoint, agents }: { endpoint: string; agents: string }) {
  // Keys set here reach the master's own probes only; an agent reads its keyfile.
  const remote = agents.split(/\s+/).filter((n) => n && n !== 'local')
  const [state, setState] = useState<IrttKey | null>(null)
  const [shown, setShown] = useState(false)
  const [entering, setEntering] = useState(false)
  const [draft, setDraft] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const id = useId()
  // Saving, removing or cancelling takes away the control that had focus;
  // put it on the first button of the block once it is back.
  const section = useRef<HTMLElement>(null)
  const refocus = useRef(false)
  useEffect(() => {
    if (!refocus.current || busy) return
    refocus.current = false
    if (!section.current?.contains(document.activeElement)) {
      section.current?.querySelector<HTMLElement>('.actions button')?.focus()
    }
  }, [state, entering, busy])

  useEffect(() => {
    let live = true
    setState(null)
    setShown(false)
    fetchIrttKey(endpoint).then(
      (k) => live && setState(k),
      (e: Error) => live && setError(e.message),
    )
    return () => {
      live = false
    }
  }, [endpoint])

  const act = async (fn: () => Promise<IrttKey | null>, done: string) => {
    setBusy(true)
    setError(null)
    refocus.current = true
    try {
      const next = await fn()
      setState(next ?? { endpoint, source: 'none' })
      announce(done)
      return true
    } catch (e) {
      setError((e as Error).message)
      return false
    } finally {
      setBusy(false)
    }
  }

  const generate = () =>
    act(async () => {
      const k = await putIrttKey(endpoint, { generate: true })
      setShown(true)
      return k
    }, `New irtt key generated for ${endpoint}`)

  const save = () =>
    act(async () => {
      const k = await putIrttKey(endpoint, { key: draft })
      setEntering(false)
      setDraft('')
      setShown(true)
      return k
    }, `irtt key saved for ${endpoint}`)

  return (
    <section ref={section} className="irtt-key" aria-labelledby={`${id}-h`}>
      <h3 id={`${id}-h`}>irtt key</h3>
      <p className="hint">
        For an irtt server started with <code>--hmac</code>. Endpoint <code>{endpoint}</code>; every
        target measuring it uses this key. Only probes on this master use it.
      </p>
      {remote.length > 0 && (
        <p className="hint warn-line">
          Measured by {remote.join(', ')} as well: an agent does not get a key set here. Put it in
          the keyfile on that host (<code>--irtt-hmac-keys</code>).
        </p>
      )}
      {error && <ErrorText>{error}</ErrorText>}
      {state === null ? (
        !error && <p className="hint">Loading…</p>
      ) : state.source === 'keyfile' ? (
        <p>
          Set in the keyfile on the host (<code>--irtt-hmac-keys</code>), which wins over one set here.
          Change it there.
        </p>
      ) : (
        <>
          <p>
            {state.source === 'db'
              ? 'A key is set.'
              : 'No key. If the server runs with --hmac, it refuses every session from this target.'}
          </p>
          {state.source === 'db' && shown && state.key && (
            <div className="irtt-key-shown">
              <code className="irtt-key-value">{state.key}</code>
              <p className="hint">Start the server with:</p>
              <code className="irtt-key-value">{serverCommand(state.key)}</code>
            </div>
          )}
          {entering ? (
            <form
              className="add-child"
              onSubmit={(e) => {
                e.preventDefault()
                void save()
              }}
            >
              <label>
                <span>
                  Key <span className="note">(as given to --hmac; 0x… is hex)</span>
                </span>
                <input
                  autoFocus
                  autoComplete="off"
                  spellCheck={false}
                  value={draft}
                  onChange={(e) => setDraft(e.target.value)}
                  required
                  maxLength={1024}
                />
              </label>
              <div className="add-child-buttons">
                <button type="submit" disabled={busy}>
                  Save key
                </button>
                <button
                  type="button"
                  disabled={busy}
                  onClick={() => {
                    refocus.current = true
                    setEntering(false)
                  }}
                >
                  Cancel
                </button>
              </div>
            </form>
          ) : (
            <div className="actions">
              {state.source === 'db' && (
                <button disabled={busy} aria-expanded={shown} onClick={() => setShown(!shown)}>
                  {shown ? 'Hide key' : 'Show key'}
                </button>
              )}
              {state.source === 'db' ? (
                // Replacing a key breaks the server until it is restarted with
                // the new one, so it asks first.
                <ConfirmButton
                  className=""
                  busy={busy}
                  label="Generate new key"
                  confirmLabel="Replace the key"
                  subject={endpoint}
                  onConfirm={generate}
                />
              ) : (
                <button disabled={busy} onClick={() => void generate()}>
                  Generate key
                </button>
              )}
              <button disabled={busy} onClick={() => setEntering(true)}>
                Enter key…
              </button>
              {state.source === 'db' && (
                <ConfirmButton
                  className=""
                  busy={busy}
                  label="Remove key"
                  subject={endpoint}
                  onConfirm={() => act(async () => (await deleteIrttKey(endpoint), null), `irtt key removed for ${endpoint}`)}
                />
              )}
            </div>
          )}
        </>
      )}
    </section>
  )
}
