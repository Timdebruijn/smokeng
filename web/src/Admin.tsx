import { useCallback, useEffect, useMemo, useState } from 'react'
import {
  createTarget,
  deleteTarget,
  fetchTargets,
  updateTarget,
  type SettingKey,
  type AgentInfo,
  fetchAgents,
  type SettingValue,
  type Target,
} from './api'

interface SettingDef {
  key: SettingKey
  label: string
  unit?: string
  kind: 'number' | 'text' | 'choice' | 'agents'
  min?: number
  max?: number
  choices?: { value: string; label: string }[]
}

// The bounds and choices here are the ones the server enforces. Stating them
// twice is deliberate: a value the server will refuse should be refused while
// the operator is still looking at the field, not after a round trip.
const SETTINGS: SettingDef[] = [
  { key: 'interval_s', label: 'Interval', unit: 's', kind: 'number', min: 1 },
  { key: 'pings_per_interval', label: 'Pings per interval', kind: 'number', min: 1 },
  {
    key: 'probe_mode',
    label: 'Probe mode',
    kind: 'choice',
    choices: [
      { value: 'burst', label: 'burst — back to back, one moment in time' },
      { value: 'spread', label: 'spread — evenly across the interval' },
    ],
  },
  { key: 'burst_gap_ms', label: 'Burst gap', unit: 'ms', kind: 'number', min: 0 },
  { key: 'timeout_ms', label: 'Timeout', unit: 'ms', kind: 'number', min: 1 },
  { key: 'packet_size', label: 'Packet size', unit: 'bytes', kind: 'number', min: 12, max: 65000 },
  { key: 'dscp', label: 'DSCP', kind: 'number', min: 0, max: 63 },
  { key: 'agents', label: 'Agents', kind: 'agents' },
  { key: 'trace_interval_s', label: 'Path discovery', unit: 's (0 = off)', kind: 'number', min: 0 },
]

export default function Admin({ readOnly = false }: { readOnly?: boolean }) {
  const [targets, setTargets] = useState<Target[]>([])
  const [agents, setAgents] = useState<AgentInfo[]>([])
  const [selectedId, setSelectedId] = useState<number | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [saving, setSaving] = useState(false)
  // A viewer may look at the tree but not change it. The server enforces this
  // regardless; disabling the controls only saves them a refused request.
  const busy = saving || readOnly

  const reload = useCallback(async () => {
    try {
      const [list, enrolled] = await Promise.all([fetchTargets(), fetchAgents()])
      setTargets(list)
      setAgents(enrolled)
      setSelectedId((cur) => (cur !== null && list.some((t) => t.id === cur) ? cur : (list[0]?.id ?? null)))
    } catch (e) {
      setError((e as Error).message)
    }
  }, [])

  useEffect(() => {
    void reload()
  }, [reload])

  // Runs a mutation, surfaces its validation message, and refreshes — the
  // server is the authority on what the tree now looks like.
  const run = useCallback(
    async (fn: () => Promise<unknown>) => {
      setSaving(true)
      setError(null)
      try {
        await fn()
        await reload()
        return true
      } catch (e) {
        setError((e as Error).message)
        return false
      } finally {
        setSaving(false)
      }
    },
    [reload],
  )

  // Depth-first order with indentation, so the tree reads as a tree.
  const ordered = useMemo(() => {
    const children = new Map<number | null, Target[]>()
    for (const t of targets) {
      const list = children.get(t.parent_id) ?? []
      list.push(t)
      children.set(t.parent_id, list)
    }
    for (const list of children.values()) {
      list.sort((a, b) => a.sort_order - b.sort_order || a.name.localeCompare(b.name))
    }
    const out: { target: Target; depth: number }[] = []
    const walk = (parent: number | null, depth: number) => {
      for (const t of children.get(parent) ?? []) {
        out.push({ target: t, depth })
        walk(t.id, depth + 1)
      }
    }
    walk(null, 0)
    return out
  }, [targets])

  const selected = targets.find((t) => t.id === selectedId) ?? null

  return (
    <div className="admin">
      <aside className="tree">
        {ordered.map(({ target, depth }) => (
          <button
            key={target.id}
            className={`tree-row${target.id === selectedId ? ' selected' : ''}`}
            style={{ paddingLeft: 8 + depth * 14 }}
            onClick={() => setSelectedId(target.id)}
          >
            <span className={target.is_group ? 'group' : 'leaf'}>
              {target.parent_id === null ? '/' : target.name}
            </span>
            {!target.enabled && <span className="badge">disabled</span>}
            {target.hidden && <span className="badge">hidden</span>}
            {target.host && <span className="dim">{target.host}</span>}
          </button>
        ))}
      </aside>
      <section className="detail">
        {error && <p className="error">{error}</p>}
        {selected ? (
          <TargetDetail
            key={selected.id}
            target={selected}
            agents={agents}
            busy={busy}
            onPatch={(body) => run(() => updateTarget(selected.id, body))}
            onDelete={(recursive) =>
              run(() => deleteTarget(selected.id, recursive)).then((ok) => {
                if (ok) setSelectedId(null)
                return ok
              })
            }
            onAddChild={(body) => run(() => createTarget({ ...body, parent_id: selected.id }))}
          />
        ) : (
          <p>Select a target.</p>
        )}
      </section>
    </div>
  )
}

// validateSetting catches what a single field cannot see on its own. The burst
// rule is a relationship between three settings, and the server rejects the
// combination rather than any one of them — so without this the operator gets
// a refusal on a field that is not the one they would need to change.
function validateSetting(target: Target, def: SettingDef, next: number | string | null): string | null {
  if (next === null) return null
  if (def.kind === 'number') {
    const v = Number(next)
    if (!Number.isFinite(v)) return 'must be a number'
    if (def.min !== undefined && v < def.min) return `must be at least ${def.min}`
    if (def.max !== undefined && v > def.max) return `must be at most ${def.max}`
  }
  if (def.key === 'agents' && String(next).trim() === '') {
    return 'a target measured by nobody is not a configuration; disable it instead'
  }

  const eff = (k: SettingKey) => Number(target.settings[k].effective)
  const proposed = (k: SettingKey) => (k === def.key ? Number(next) : eff(k))
  const mode = def.key === 'probe_mode' ? String(next) : String(target.settings.probe_mode.effective)
  if (mode === 'burst' && ['interval_s', 'pings_per_interval', 'burst_gap_ms', 'probe_mode'].includes(def.key)) {
    const span = proposed('pings_per_interval') * proposed('burst_gap_ms')
    const interval = proposed('interval_s') * 1000
    if (span >= interval) {
      return `${proposed('pings_per_interval')} pings ${proposed('burst_gap_ms')}ms apart take ${span / 1000}s, which does not fit in a ${proposed('interval_s')}s interval`
    }
  }
  return null
}

interface DetailProps {
  target: Target
  agents: AgentInfo[]
  busy: boolean
  onPatch: (body: Parameters<typeof updateTarget>[1]) => Promise<boolean>
  onDelete: (recursive: boolean) => Promise<boolean>
  onAddChild: (body: { name: string; host?: string; address_family?: string }) => Promise<boolean>
}

function TargetDetail({ target, agents, busy, onPatch, onDelete, onAddChild }: DetailProps) {
  const isRoot = target.parent_id === null
  const [adding, setAdding] = useState(false)

  return (
    <>
      <h2>
        <code>{target.path}</code>
        <span className="dim">{target.is_group ? 'group' : `${target.host} · ${target.address_family}`}</span>
      </h2>

      <div className="fields">
        {!isRoot && (
          <TextField label="Name" value={target.name} busy={busy} onSave={(v) => onPatch({ name: v })} />
        )}
        <TextField
          label="Title"
          value={target.title ?? ''}
          busy={busy}
          placeholder="(display name)"
          onSave={(v) => onPatch({ title: v === '' ? null : v })}
        />
        <TextField
          label="Notes"
          value={target.notes ?? ''}
          busy={busy}
          onSave={(v) => onPatch({ notes: v === '' ? null : v })}
        />
        {!isRoot && (
          <>
            <TextField
              label="Host"
              value={target.host ?? ''}
              busy={busy}
              placeholder="(empty = group node)"
              onSave={(v) =>
                // Host and address family are set together or not at all.
                onPatch(v === '' ? { host: null, address_family: null } : { host: v })
              }
            />
            <label className="field">
              <span>Address family</span>
              <select
                value={target.address_family ?? ''}
                disabled={busy || target.host === null}
                onChange={(e) => void onPatch({ address_family: e.target.value })}
              >
                <option value="v4">v4</option>
                <option value="v6">v6</option>
              </select>
            </label>
          </>
        )}
        <label className="field checkbox">
          <input
            type="checkbox"
            checked={target.enabled}
            disabled={busy}
            onChange={(e) => void onPatch({ enabled: e.target.checked })}
          />
          <span>Enabled — measured by the prober</span>
        </label>
        <label className="field checkbox">
          <input
            type="checkbox"
            checked={target.hidden}
            disabled={busy}
            onChange={(e) => void onPatch({ hidden: e.target.checked })}
          />
          <span>Hidden — measured, but not shown on the graphs page</span>
        </label>
      </div>

      <h3>Settings</h3>
      <p className="hint">
        Unset values are inherited from an ancestor. Overriding writes the value on this node;
        reverting clears it so it follows its parent again.
      </p>
      <table className="settings">
        <tbody>
          {SETTINGS.map((s) => (
            <SettingRow
              key={s.key}
              def={s}
              value={target.settings[s.key] as SettingValue<number | string>}
              agents={agents}
              busy={busy}
              validate={(v) => validateSetting(target, s, v)}
              onSet={(v) => onPatch({ settings: { [s.key]: v } })}
            />
          ))}
        </tbody>
      </table>

      <h3>Structure</h3>
      <div className="actions">
        {adding ? (
          <AddChildForm
            busy={busy}
            onCancel={() => setAdding(false)}
            onSubmit={async (body) => {
              if (await onAddChild(body)) setAdding(false)
            }}
          />
        ) : (
          <button onClick={() => setAdding(true)} disabled={busy}>
            Add child…
          </button>
        )}
        {!isRoot && (
          <>
            <button className="danger" disabled={busy} onClick={() => void onDelete(false)}>
              Delete
            </button>
            <button className="danger" disabled={busy} onClick={() => void onDelete(true)}>
              Delete with descendants
            </button>
          </>
        )}
      </div>
      <p className="hint">Deleting a target never deletes its measurements.</p>
    </>
  )
}

function SettingRow({
  def,
  value,
  agents,
  busy,
  validate,
  onSet,
}: {
  def: SettingDef
  value: SettingValue<number | string>
  agents: AgentInfo[]
  busy: boolean
  validate: (v: number | string | null) => string | null
  onSet: (v: number | string | null) => Promise<boolean>
}) {
  const isLocal = value.local !== null
  const [draft, setDraft] = useState(String(value.effective))
  const [problem, setProblem] = useState<string | null>(null)
  useEffect(() => {
    setDraft(String(value.effective))
    setProblem(null)
  }, [value.effective])

  const set = (next: number | string | null) => {
    const bad = validate(next)
    setProblem(bad)
    if (bad !== null) return
    void onSet(next)
  }

  const commit = () => {
    const next = def.kind === 'number' ? Number(draft) : draft
    if (next === value.local) return
    set(next)
  }

  return (
    <tr>
      <th>{def.label}</th>
      <td>
        {def.kind === 'agents' ? (
          <AgentPicker
            selected={String(value.effective)}
            agents={agents}
            disabled={busy || !isLocal}
            onChange={set}
          />
        ) : def.kind === 'choice' ? (
          <select
            value={String(value.effective)}
            disabled={busy || !isLocal}
            onChange={(e) => set(e.target.value)}
          >
            {def.choices?.map((c) => (
              <option key={c.value} value={c.value}>
                {c.label}
              </option>
            ))}
          </select>
        ) : (
          <>
            <input
              value={draft}
              disabled={busy || !isLocal}
              size={10}
              type={def.kind === 'number' ? 'number' : 'text'}
              min={def.min}
              max={def.max}
              onChange={(e) => setDraft(e.target.value)}
              onBlur={commit}
              onKeyDown={(e) => e.key === 'Enter' && e.currentTarget.blur()}
            />
            {def.unit && <span className="unit">{def.unit}</span>}
          </>
        )}
        {problem && <p className="field-error">{problem}</p>}
      </td>
      <td className="provenance">
        {value.source === 'local' ? (
          <span className="chip local">set here</span>
        ) : value.source === 'outside' ? (
          // The ancestor that set this is above the caller's scope, so it has
          // no name they may be told. The value still applies, and saying so
          // is better than showing a number with no account of itself.
          <span className="chip">inherited from outside your scope</span>
        ) : (
          <span className="chip">
            inherited from <code>{value.source.path === '/' ? '/' : value.source.path}</code>
          </span>
        )}
      </td>
      <td>
        {isLocal ? (
          <button disabled={busy} onClick={() => void onSet(null)}>
            Revert
          </button>
        ) : (
          <button disabled={busy} onClick={() => void onSet(value.effective)}>
            Override
          </button>
        )}
      </td>
    </tr>
  )
}

// AgentPicker replaces what used to be a free-text field. A name typed here
// that matched no enrolled agent meant the target was measured by nobody, with
// no error anywhere — an empty graph indistinguishable from one that is
// measured and never answers (DESIGN.md §4.4). Offering the enrolled names
// makes that unspellable.
//
// It is also why inheritance stays replace-not-accumulate: overriding
// pre-fills with the inherited set, so "everything from local, and this
// subtree also from ams-01" is one checkbox rather than a second inheritance
// mechanism to reason about.
function AgentPicker({
  selected,
  agents,
  disabled,
  onChange,
}: {
  selected: string
  agents: AgentInfo[]
  disabled: boolean
  onChange: (v: string) => void
}) {
  const chosen = selected.trim().split(/\s+/).filter(Boolean)
  const known = ['local', ...agents.filter((a) => !a.is_local).map((a) => a.name)]
  // A name assigned before the agent was removed would otherwise vanish from
  // the list silently, and unchecking is not what happened to it.
  const orphaned = chosen.filter((n) => !known.includes(n))

  const toggle = (name: string, on: boolean) => {
    const next = on ? [...chosen, name] : chosen.filter((n) => n !== name)
    onChange(next.join(' '))
  }

  return (
    <div className="agent-picker">
      {[...known, ...orphaned].map((name) => (
        <label key={name} className="agent-option">
          <input
            type="checkbox"
            checked={chosen.includes(name)}
            disabled={disabled}
            onChange={(e) => toggle(name, e.target.checked)}
          />
          <span>
            {name}
            {name === 'local' && <span className="unit">the master itself</span>}
            {orphaned.includes(name) && <span className="unit">not enrolled</span>}
          </span>
        </label>
      ))}
    </div>
  )
}

function TextField({
  label,
  value,
  busy,
  placeholder,
  onSave,
}: {
  label: string
  value: string
  busy: boolean
  placeholder?: string
  onSave: (v: string) => Promise<boolean>
}) {
  const [draft, setDraft] = useState(value)
  useEffect(() => setDraft(value), [value])
  return (
    <label className="field">
      <span>{label}</span>
      <input
        value={draft}
        disabled={busy}
        placeholder={placeholder}
        onChange={(e) => setDraft(e.target.value)}
        onBlur={() => draft !== value && void onSave(draft)}
        onKeyDown={(e) => e.key === 'Enter' && e.currentTarget.blur()}
      />
    </label>
  )
}

function AddChildForm({
  busy,
  onSubmit,
  onCancel,
}: {
  busy: boolean
  onSubmit: (body: { name: string; host?: string; address_family?: string }) => void
  onCancel: () => void
}) {
  const [name, setName] = useState('')
  const [host, setHost] = useState('')
  const [family, setFamily] = useState('v4')
  return (
    <form
      className="add-child"
      onSubmit={(e) => {
        e.preventDefault()
        onSubmit(host ? { name, host, address_family: family } : { name })
      }}
    >
      <input placeholder="name" value={name} onChange={(e) => setName(e.target.value)} required />
      <input placeholder="host (empty = group)" value={host} onChange={(e) => setHost(e.target.value)} />
      <select value={family} onChange={(e) => setFamily(e.target.value)} disabled={!host}>
        <option value="v4">v4</option>
        <option value="v6">v6</option>
      </select>
      <button type="submit" disabled={busy || !name}>
        Create
      </button>
      <button type="button" onClick={onCancel} disabled={busy}>
        Cancel
      </button>
    </form>
  )
}
