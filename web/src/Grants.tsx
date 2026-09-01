import { Fragment, useCallback, useEffect, useMemo, useState } from 'react'
import {
  createGrant,
  deleteGrant,
  fetchGrants,
  fetchTargets,
  type Grant,
  type Target,
} from './api'

const ROLES: { value: Grant['role']; label: string }[] = [
  { value: 'viewer', label: 'viewer — read' },
  { value: 'editor', label: 'editor — read and write' },
]

export default function Grants({
  readOnly = false,
  defaultRole,
  authEnabled = true,
}: {
  readOnly?: boolean
  defaultRole?: 'viewer' | 'none'
  authEnabled?: boolean
}) {
  const [grants, setGrants] = useState<Grant[]>([])
  const [targets, setTargets] = useState<Target[]>([])
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)
  const [adding, setAdding] = useState(false)

  const reload = useCallback(async () => {
    try {
      const [g, t] = await Promise.all([fetchGrants(), fetchTargets()])
      setGrants(g)
      setTargets(t)
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

  // Grouped by OIDC group name — the list should read as "who can reach
  // what", not as a flat table of unrelated rows.
  const grouped = useMemo(() => {
    const byGroup = new Map<string, Grant[]>()
    for (const g of grants) {
      const list = byGroup.get(g.group) ?? []
      list.push(g)
      byGroup.set(g.group, list)
    }
    for (const list of byGroup.values()) list.sort((a, b) => a.path.localeCompare(b.path))
    return [...byGroup.entries()].sort(([a], [b]) => a.localeCompare(b))
  }, [grants])

  return (
    <>
      {error && <p className="error">{error}</p>}

      <section className="card section-card">
        <div className="section-card-head">
          <span className="section-card-title">Access</span>
        </div>
        <p className="hint">
          A grant gives an OIDC group a role on a target and everything beneath it. A group scoped
          this way sees that subtree as though it were the whole installation — there is no way for
          it to learn that anything else exists. <code>admin</code> is not offered as a role here:
          it is a global role assigned by the identity provider, not something granted per target.
        </p>
        {!authEnabled && (
          <p className="hint warn">
            This instance runs without authentication, so there is nobody to grant anything to:
            every request is treated as an admin. Grants written here are stored and have no
            effect until <code>--oidc-issuer</code> is configured.
          </p>
        )}
        {authEnabled && defaultRole === 'viewer' && (
          <p className="hint warn">
            These grants are not in force yet. This instance runs with{' '}
            <code>--default-role viewer</code>, so every authenticated user can already read
            everything and a grant only ever adds. Write the grants you want, check them, then
            restart with <code>--default-role none</code> — at which point access comes from
            this list alone.
          </p>
        )}

        {grouped.length === 0 ? (
          <p className="hint">No grants.</p>
        ) : (
          <table className="alerts">
            <tbody>
              {grouped.map(([group, list]) => (
                <Fragment key={group}>
                  <tr>
                    <th colSpan={3} className="group-heading">
                      {group}
                    </th>
                  </tr>
                  {list.map((g) => (
                    <GrantRow
                      key={g.id}
                      grant={g}
                      readOnly={readOnly}
                      busy={busy}
                      onRemove={() => run(() => deleteGrant(g.id))}
                      onRerole={(role) =>
                        run(() => createGrant({ group: g.group, target_id: g.target_id, role }))
                      }
                    />
                  ))}
                </Fragment>
              ))}
            </tbody>
          </table>
        )}

        {!readOnly &&
          (adding ? (
            <AddGrantForm
              targets={targets}
              busy={busy}
              onCancel={() => setAdding(false)}
              onSubmit={async (group, targetId, role) => {
                if (await run(() => createGrant({ group, target_id: targetId, role }))) setAdding(false)
              }}
            />
          ) : (
            <div className="pill-row">
              <button className="pill accent" disabled={busy} onClick={() => setAdding(true)}>
                Add a grant…
              </button>
            </div>
          ))}
      </section>
    </>
  )
}

function GrantRow({
  grant,
  readOnly,
  busy,
  onRemove,
  onRerole,
}: {
  grant: Grant
  readOnly: boolean
  busy: boolean
  onRemove: () => Promise<boolean>
  onRerole: (role: Grant['role']) => void
}) {
  const [confirming, setConfirming] = useState(false)
  return (
    <tr>
      <td>
        <code>{grant.path === '' ? '/' : grant.path}</code>{' '}
        <span className="badge">+ subtree</span>
      </td>
      <td>
        {/* The role is the one part of a grant that can change in place: the
            group and the path are what identifies it, so changing either is a
            different grant. Editing here rather than only in the add form
            means narrowing someone's access never goes through a state where
            they have none. */}
        {readOnly ? (
          <span className="badge">{grant.role}</span>
        ) : (
          <select
            value={grant.role}
            disabled={busy}
            aria-label={`role for ${grant.group} on ${grant.path === '' ? '/' : grant.path}`}
            onChange={(e) => onRerole(e.target.value as Grant['role'])}
          >
            {ROLES.map((r) => (
              <option key={r.value} value={r.value}>
                {r.label}
              </option>
            ))}
          </select>
        )}
      </td>
      <td>
        {!readOnly &&
          (confirming ? (
            <>
              <button
                className="pill danger"
                disabled={busy}
                onClick={() => void onRemove().then((ok) => ok && setConfirming(false))}
              >
                Confirm remove
              </button>
              <button className="pill" disabled={busy} onClick={() => setConfirming(false)}>
                Cancel
              </button>
            </>
          ) : (
            <button className="pill danger" disabled={busy} onClick={() => setConfirming(true)}>
              Remove
            </button>
          ))}
      </td>
    </tr>
  )
}

// Depth-first, indented order — same idea as Admin's target tree — so the
// picker reads as a tree even though a <select> cannot draw one directly.
function orderedTargets(targets: Target[]): { target: Target; depth: number }[] {
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
}

function AddGrantForm({
  targets,
  busy,
  onSubmit,
  onCancel,
}: {
  targets: Target[]
  busy: boolean
  onSubmit: (group: string, targetId: number, role: Grant['role']) => void
  onCancel: () => void
}) {
  const ordered = useMemo(() => orderedTargets(targets), [targets])
  const [group, setGroup] = useState('')
  const [targetId, setTargetId] = useState<number | ''>('')
  const [role, setRole] = useState<Grant['role']>('viewer')

  useEffect(() => {
    if (targetId === '' && ordered.length > 0) setTargetId(ordered[0].target.id)
  }, [ordered, targetId])

  return (
    <form
      className="card rule-form"
      onSubmit={(e) => {
        e.preventDefault()
        const trimmed = group.trim()
        if (!trimmed || targetId === '') return
        onSubmit(trimmed, targetId, role)
      }}
    >
      <label className="field">
        <span>Group</span>
        <input
          value={group}
          autoFocus
          placeholder="OIDC group name"
          onChange={(e) => setGroup(e.target.value)}
          required
        />
      </label>
      <label className="field">
        <span>Target</span>
        <select
          value={targetId}
          onChange={(e) => setTargetId(Number(e.target.value))}
        >
          {ordered.map(({ target, depth }) => (
            <option key={target.id} value={target.id}>
              {'  '.repeat(depth)}
              {target.path === '' ? '/' : target.path}
            </option>
          ))}
        </select>
      </label>
      <label className="field">
        <span>Role</span>
        <select value={role} onChange={(e) => setRole(e.target.value as Grant['role'])}>
          {ROLES.map((r) => (
            <option key={r.value} value={r.value}>
              {r.label}
            </option>
          ))}
        </select>
      </label>
      <p className="hint">
        Applies to the target and everything beneath it. Adding a grant for a group and target
        that already has one replaces its role rather than adding a second grant.
      </p>
      <div className="pill-row">
        <button className="pill accent" type="submit" disabled={busy || !group.trim() || targetId === ''}>
          Add grant
        </button>
        <button className="pill" type="button" disabled={busy} onClick={onCancel}>
          Cancel
        </button>
      </div>
    </form>
  )
}
