import { useCallback, useEffect, useMemo, useState } from 'react'
import {
  createTarget,
  deleteTarget,
  fetchTargets,
  updateTarget,
  type SettingKey,
  type SettingValue,
  type Target,
} from './api'

const SETTINGS: { key: SettingKey; label: string; unit?: string; kind: 'number' | 'text' }[] = [
  { key: 'interval_s', label: 'Interval', unit: 's', kind: 'number' },
  { key: 'pings_per_interval', label: 'Pings per interval', kind: 'number' },
  { key: 'probe_mode', label: 'Probe mode', kind: 'text' },
  { key: 'burst_gap_ms', label: 'Burst gap', unit: 'ms', kind: 'number' },
  { key: 'timeout_ms', label: 'Timeout', unit: 'ms', kind: 'number' },
  { key: 'packet_size', label: 'Packet size', unit: 'bytes', kind: 'number' },
  { key: 'dscp', label: 'DSCP', kind: 'number' },
  { key: 'agents', label: 'Agents', kind: 'text' },
]

export default function Admin() {
  const [targets, setTargets] = useState<Target[]>([])
  const [selectedId, setSelectedId] = useState<number | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)

  const reload = useCallback(async () => {
    try {
      const list = await fetchTargets()
      setTargets(list)
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

interface DetailProps {
  target: Target
  busy: boolean
  onPatch: (body: Parameters<typeof updateTarget>[1]) => Promise<boolean>
  onDelete: (recursive: boolean) => Promise<boolean>
  onAddChild: (body: { name: string; host?: string; address_family?: string }) => Promise<boolean>
}

function TargetDetail({ target, busy, onPatch, onDelete, onAddChild }: DetailProps) {
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
              busy={busy}
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
  busy,
  onSet,
}: {
  def: (typeof SETTINGS)[number]
  value: SettingValue<number | string>
  busy: boolean
  onSet: (v: number | string | null) => Promise<boolean>
}) {
  const isLocal = value.local !== null
  const [draft, setDraft] = useState(String(value.effective))
  useEffect(() => setDraft(String(value.effective)), [value.effective])

  const commit = () => {
    const next = def.kind === 'number' ? Number(draft) : draft
    if (def.kind === 'number' && Number.isNaN(next)) return
    if (next === value.local) return
    void onSet(next)
  }

  return (
    <tr>
      <th>{def.label}</th>
      <td>
        <input
          value={draft}
          disabled={busy || !isLocal}
          size={10}
          onChange={(e) => setDraft(e.target.value)}
          onBlur={commit}
          onKeyDown={(e) => e.key === 'Enter' && e.currentTarget.blur()}
        />
        {def.unit && <span className="unit">{def.unit}</span>}
      </td>
      <td className="provenance">
        {value.source === 'local' ? (
          <span className="chip local">set here</span>
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
