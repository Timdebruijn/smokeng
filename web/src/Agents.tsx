import { useCallback, useEffect, useState } from 'react'
import {
  createAgentToken,
  deleteAgent,
  deleteAgentToken,
  fetchAgents,
  fetchAgentTokens,
  updateAgent,
  type AgentInfo,
  type AgentToken,
  type NewAgentToken,
} from './api'

const TTL_OPTIONS: { seconds: number; label: string }[] = [
  { seconds: 900, label: '15 minutes' },
  { seconds: 3600, label: '1 hour' },
  { seconds: 14400, label: '4 hours' },
  { seconds: 86400, label: '24 hours' },
]

export default function Agents({ readOnly = false }: { readOnly?: boolean }) {
  const [agents, setAgents] = useState<AgentInfo[]>([])
  const [tokens, setTokens] = useState<AgentToken[]>([])
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)
  const [adding, setAdding] = useState(false)
  // Holds the plaintext token right after minting. Nothing else — not even a
  // reload — can bring it back once it is dismissed.
  const [minted, setMinted] = useState<NewAgentToken | null>(null)

  const reload = useCallback(async () => {
    try {
      const [a, t] = await Promise.all([fetchAgents(), fetchAgentTokens()])
      setAgents(a)
      setTokens(t)
    } catch (e) {
      setError((e as Error).message)
    }
  }, [])

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

  const agentName = (id: number | undefined) => {
    if (id === undefined) return undefined
    return agents.find((a) => a.id === id)?.name
  }

  return (
    <>
      {error && <p className="error">{error}</p>}

      <h2 className="section">Agents</h2>
      <p className="hint">
        Each agent measures from its own vantage point, and each is its own series — two
        vantage points that disagree is the finding, not something to average away. The local
        prober is this master itself, and cannot be renamed, disabled or removed.
      </p>
      {agents.length === 0 ? (
        <p className="hint">No agents.</p>
      ) : (
        <table className="alerts">
          <tbody>
            {agents.map((a) => (
              <AgentRow
                key={a.id}
                agent={a}
                readOnly={readOnly}
                busy={busy}
                onRename={(name) => run(() => updateAgent(a.id, { name }))}
                onToggle={() => run(() => updateAgent(a.id, { enabled: !a.enabled }))}
                onRemove={() => run(() => deleteAgent(a.id))}
              />
            ))}
          </tbody>
        </table>
      )}

      {!readOnly &&
        (adding ? (
          <AddAgentForm
            busy={busy}
            onCancel={() => setAdding(false)}
            onSubmit={async (name, ttlS) => {
              setBusy(true)
              setError(null)
              try {
                const t = await createAgentToken(name, ttlS)
                setMinted(t)
                setAdding(false)
                await reload()
              } catch (e) {
                setError((e as Error).message)
              } finally {
                setBusy(false)
              }
            }}
          />
        ) : (
          <div className="actions">
            <button disabled={busy} onClick={() => setAdding(true)}>
              Add an agent…
            </button>
          </div>
        ))}

      {minted && <TokenReveal token={minted} onDismiss={() => setMinted(null)} />}

      <h2 className="section">Enrolment tokens</h2>
      <p className="hint">
        A token is single-use: an agent spends it on its first connection. Unspent tokens expire
        on their own; spent and expired ones are kept here as a record.
      </p>
      {tokens.length === 0 ? (
        <p className="hint">No tokens.</p>
      ) : (
        <table className="alerts">
          <tbody>
            {tokens.map((t) => (
              <TokenRow
                key={t.id}
                token={t}
                agentName={agentName(t.agent_id)}
                readOnly={readOnly}
                busy={busy}
                onRevoke={() => run(() => deleteAgentToken(t.id))}
              />
            ))}
          </tbody>
        </table>
      )}
    </>
  )
}

function AgentRow({
  agent,
  readOnly,
  busy,
  onRename,
  onToggle,
  onRemove,
}: {
  agent: AgentInfo
  readOnly: boolean
  busy: boolean
  onRename: (name: string) => Promise<boolean>
  onToggle: () => Promise<boolean>
  onRemove: () => Promise<boolean>
}) {
  const [draft, setDraft] = useState(agent.name)
  useEffect(() => setDraft(agent.name), [agent.name])
  const [confirming, setConfirming] = useState(false)

  return (
    <tr className={agent.enabled ? '' : 'disabled-row'}>
      <td>
        {agent.is_local ? (
          <strong>{agent.name}</strong>
        ) : readOnly ? (
          agent.name
        ) : (
          <input
            value={draft}
            disabled={busy}
            onChange={(e) => setDraft(e.target.value)}
            onBlur={() => draft.trim() && draft !== agent.name && void onRename(draft.trim())}
            onKeyDown={(e) => e.key === 'Enter' && e.currentTarget.blur()}
          />
        )}
        {agent.is_local && <span className="badge">local prober</span>}
      </td>
      {/* The local prober does not report to anything — it is the master. A
          last-seen of "never" there reads as a fault rather than as a
          category that does not apply. */}
      <td className="dim">{agent.is_local ? '—' : relativeTime(agent.last_seen)}</td>
      <td>{agent.enabled ? '' : <span className="badge">disabled</span>}</td>
      <td>
        {!agent.is_local && !readOnly && (
          <>
            <button disabled={busy} onClick={() => void onToggle()}>
              {agent.enabled ? 'Disable' : 'Enable'}
            </button>
            {confirming ? (
              <>
                <button
                  className="danger"
                  disabled={busy}
                  onClick={() => void onRemove().then((ok) => ok && setConfirming(false))}
                >
                  Confirm remove
                </button>
                <button disabled={busy} onClick={() => setConfirming(false)}>
                  Cancel
                </button>
              </>
            ) : (
              <button className="danger" disabled={busy} onClick={() => setConfirming(true)}>
                Remove
              </button>
            )}
          </>
        )}
      </td>
    </tr>
  )
}

function TokenRow({
  token,
  agentName,
  readOnly,
  busy,
  onRevoke,
}: {
  token: AgentToken
  agentName: string | undefined
  readOnly: boolean
  busy: boolean
  onRevoke: () => Promise<boolean>
}) {
  const state = token.used ? 'spent' : token.expired ? 'expired' : 'unspent'
  return (
    <tr className={state === 'unspent' ? '' : 'disabled-row'}>
      <td>{token.name}</td>
      <td className="dim">
        {state === 'unspent' && `expires ${new Date(token.expires_at * 1000).toLocaleString()}`}
        {state === 'spent' &&
          `used by ${agentName ?? (token.agent_id !== undefined ? `agent ${token.agent_id}` : 'an agent')}${
            token.used_at ? ` · ${new Date(token.used_at * 1000).toLocaleString()}` : ''
          }`}
        {state === 'expired' && `expired ${new Date(token.expires_at * 1000).toLocaleString()}`}
      </td>
      <td>{state !== 'unspent' && <span className="badge">{state}</span>}</td>
      <td>
        {state === 'unspent' && !readOnly && (
          <button className="danger" disabled={busy} onClick={() => void onRevoke()}>
            Revoke
          </button>
        )}
      </td>
    </tr>
  )
}

function AddAgentForm({
  busy,
  onSubmit,
  onCancel,
}: {
  busy: boolean
  onSubmit: (name: string, ttlS: number) => void
  onCancel: () => void
}) {
  const [name, setName] = useState('')
  const [ttlS, setTtlS] = useState(3600)
  return (
    <form
      className="rule-form"
      onSubmit={(e) => {
        e.preventDefault()
        onSubmit(name.trim(), ttlS)
      }}
    >
      <label className="field">
        <span>Name</span>
        <input value={name} autoFocus onChange={(e) => setName(e.target.value)} required />
      </label>
      <label className="field">
        <span>Token expires in</span>
        <select value={ttlS} onChange={(e) => setTtlS(Number(e.target.value))}>
          {TTL_OPTIONS.map((o) => (
            <option key={o.seconds} value={o.seconds}>
              {o.label}
            </option>
          ))}
        </select>
      </label>
      <div className="actions">
        <button type="submit" disabled={busy || !name.trim()}>
          Create token
        </button>
        <button type="button" disabled={busy} onClick={onCancel}>
          Cancel
        </button>
      </div>
    </form>
  )
}

function TokenReveal({ token, onDismiss }: { token: NewAgentToken; onDismiss: () => void }) {
  // The agent refuses a plain-HTTP master unless told otherwise, because this
  // request is the one carrying a usable credential. Show the command that
  // actually works here, and say why it needs the flag, rather than handing
  // over one that will be rejected.
  const origin = window.location.origin
  const overTLS = origin.startsWith('https://')
  const command =
    `smokeng agent run --master ${origin} --token ${token.token}` +
    (overTLS ? '' : ' --insecure-allow-http')
  const [copied, setCopied] = useState(false)

  const copy = async () => {
    try {
      await navigator.clipboard.writeText(command)
      setCopied(true)
      setTimeout(() => setCopied(false), 2000)
    } catch {
      // Clipboard access can be denied; the command below is still selectable.
    }
  }

  return (
    <div className="token-reveal">
      <h3>Token for “{token.name}”</h3>
      <p className="hint">
        This token is shown once, here, and cannot be retrieved again — if it is lost, revoke it
        and issue a new one. Run this on the agent host to enrol it with this master:
      </p>
      <div className="command">
        <code>{command}</code>
      </div>
      {!overTLS && (
        <p className="hint">
          This master is not on TLS, so the command carries{' '}
          <code>--insecure-allow-http</code>. The token is a credential and this sends it in
          the clear; put a TLS-terminating proxy in front before doing this over a network
          you do not trust.
        </p>
      )}
      <div className="actions">
        <button onClick={() => void copy()}>{copied ? 'Copied' : 'Copy command'}</button>
        <button onClick={onDismiss}>I have saved it — dismiss</button>
      </div>
    </div>
  )
}

/** Coarse relative time — this is a "still alive" indicator, not a clock. */
function relativeTime(unixSeconds: number | undefined): string {
  if (unixSeconds === undefined) return 'never'
  const deltaS = Math.floor(Date.now() / 1000) - unixSeconds
  if (deltaS < 5) return 'just now'
  if (deltaS < 60) return `${deltaS}s ago`
  const m = Math.round(deltaS / 60)
  if (m < 60) return `${m} minute${m === 1 ? '' : 's'} ago`
  const h = Math.round(m / 60)
  if (h < 24) return `${h} hour${h === 1 ? '' : 's'} ago`
  const d = Math.round(h / 24)
  return `${d} day${d === 1 ? '' : 's'} ago`
}
