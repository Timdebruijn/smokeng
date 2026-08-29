# Access control

smokeng has three roles and one way of scoping them.

| Role | Where it comes from | What it permits |
| --- | --- | --- |
| `admin` | The identity provider, via `--oidc-admin-value` | Everything, everywhere |
| `editor` | A grant | Read and write inside one subtree — except agents, enrolment tokens, grants, `/metrics` and config import/export, which stay global admin regardless of any grant (see below) |
| `viewer` | A grant, or `--default-role` | Read inside one subtree, or everywhere |

`admin` is global and is not grantable. Everything else is a **grant**, and a grant's
`editor` role only ever reaches the target tree, alert rules and their own subtree — it
never reaches the global-admin-only routes listed under "What a grant never confers"
below.

## Grants

A grant gives an OIDC group a role on a target node and everything beneath it —
the same inheritance every other setting on the tree uses.

```toml
[[grants]]
group = "gemeente-x-beheer"
path  = "Klanten/GemeenteX"
role  = "viewer"

[[grants]]
group = "noc"
path  = "Klanten"
role  = "editor"
```

Or in the UI under **Access**, as an admin.

Grants are keyed on **groups**, never on individuals. One person is a group of
one in your provider. The identity provider supplies group membership — read
from the same claim as `--oidc-admin-claim` — and nothing else. Authorisation
lives here, next to the tree it applies to, rather than in a provider-specific
claim you would have to go and read during an incident.

## Isolation is total

A scoped user's subtree is presented as though it were the whole installation.
They do not see what is above it or beside it — not the nodes, not the names,
not the fact that there are any. A municipality does not learn that other
municipalities are customers.

Concretely:

- Paths are rendered relative to their grant. Someone granted
  `Klanten/GemeenteX` sees `/GemeenteX` and `/GemeenteX/gw`.
- A parent they cannot see is not named in the response.
- Every list is filtered: targets, alert rules, firing alerts, agents.
- A target outside their scope is answered as **absent**, not forbidden.
  "You may not read that" would confirm there is something to read.

### Settings inherited from outside

A node inherits from ancestors a scoped user may not know exist. Such a value is
reported with its source as `outside`: the effective value, honestly labelled,
with no path. Naming the ancestor would disclose a node they should not know
about; withholding the value would show them a number they cannot account for.

So a scoped user sees "interval 60s, inherited from outside your scope" and can
override it on their own node, which is exactly what they need to know.

## What a grant never confers

However wide the subtree, a grant does not reach:

- **Agents and enrolment tokens.** Agents are global infrastructure. A scoped
  user sees the *names* and liveness of the agents that measure targets they can
  see — otherwise "from ams-01" on their own graph is unreadable — and no public
  keys, no tokens, and no evidence that other agents exist.
- **The root defaults.**
- **`/metrics`**, which counts and names things across the whole installation.
- **`config import` and `config export`**, which are declarative over the entire
  tree and cannot express a partial apply.

Those are all global admin.

## Turning it on

Adding a grant changes nothing by itself. What an authenticated user holding no
grant may do is a separate, explicit setting:

```bash
smokeng serve --default-role viewer   # the default: everyone authenticated reads everything
smokeng serve --default-role none     # only grants confer access
```

It is deliberately a setting rather than a consequence. A security change that
happens as a side effect of unrelated configuration is a change nobody
reviewed — so you write your grants first, check them, and then flip
`--default-role` to `none` when you mean to.

Restrict *who can authenticate at all* at your identity provider. smokeng
decides what a signed-in user may reach; it does not decide who may sign in.

## Grants in TOML

Grants round-trip through `config export` and `config import` like the rest of
the tree, so they can live in Git.

They are more declarative than targets are, in one specific way: **a grant
absent from the file is removed**, with no `--prune` to ask for it. A target is
disabled rather than deleted because its measurements are the product and
destroying them would be the damaging act. A grant holds no data, and for
authorisation the damaging direction is the other one — leaving access in place
that the file no longer says should exist.

The exception: a file with **no `grants` key at all** leaves grants untouched.
Otherwise importing a targets-only file would silently revoke everyone.

## Auditing

`GET /api/v1/grants` lists every grant with the path it applies to. There is no
separate audit log; the grant table is the record of who may see what, and it is
exportable, diffable and reviewable as text.
