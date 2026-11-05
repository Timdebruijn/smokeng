# Configuration

The database is the source of truth. TOML is how you get configuration into it and back
out of it again, so a target tree can live in Git and be applied like any other
infrastructure change.

```bash
smokeng config import --db smokeng.db targets.toml   # apply a file
smokeng config export --db smokeng.db > targets.toml # write the tree back out
```

`import` is declarative and idempotent. Anything present in the file is created or updated;
anything missing from it is **disabled**, so a mistake in the file never silently destroys
history. `--prune` deletes instead. See [Config as code](#config-as-code) for the CLI against
the UI, and for what a round trip loses.

`export` writes only *local* values — settings a node sets itself, not the ones it
inherits — so an export can be re-imported without flattening the tree.

## Config as code

Keep the TOML in Git and apply it. Two ways in, one code path:

| | `smokeng config import` | UI "Import TOML" / `PUT /api/v1/config` |
| --- | --- | --- |
| Where it runs | On the master host, on the database file (`--db`, default `smokeng.db`) | Any browser or client with a global-admin session |
| `--prune` | Yes: deletes what the file omits | Never; absence always disables |
| Unknown agent names | `--allow-unknown-agents` | `?allow_unknown_agents=1` |

Both call the same function, so validation is identical. Everything is validated before the
first write, and a file with an error changes nothing.

**An import is not transactional.** After validation it writes node by node. A failure or a
crash halfway leaves a partly applied tree, and an edit made in the UI at the same time can be
overwritten or interleaved. Run imports when nobody else is editing, and re-run after a
failure; the import is idempotent.

**Drift detection.** `config export` writes the live tree. Commit an export as the baseline,
then compare it later:

```bash
smokeng config export --db /var/lib/smokeng/smokeng.db > live.toml
diff -u targets.toml live.toml
```

Export writes only local values, so a hand-written file differs in ordering and comments even
when it means the same thing. Diff an export against an earlier export, not against a file
written by hand.

**What does not round-trip.** The TOML carries the target tree, alert rules and grants. It does
not carry:

- silences, which live only in the database;
- enrolled agents and their keys; enrol them with `smokeng agent add` ([Remote agents](agents.md));
- the reference of a `golden` baseline: the rule says `baseline = "golden"`, but the captured
  distribution is measured data and is taken from the UI or the API;
- measurements and path history.

An export that has no grants omits the key, and an import without it leaves grants alone. An
explicit empty `grants` list removes them all.

## The target tree

Targets form a tree. Paths are slash-separated, with no leading or trailing slash:

```toml
[targets."Datacenter"]
title = "Datacenter Rotterdam"

[targets."Datacenter/Core"]
title = "Core switches"

[targets."Datacenter/Core/sw-core-01"]
host = "10.0.0.1"
address_family = "v4"
```

A node with **both** `host` and `address_family` is a *target* and gets probed. A node
with neither is a *group*: it exists to hold settings and to organise the UI. Setting only
one of the two is an error. Intermediate groups are created automatically if you skip
them, so you can declare a deep path directly.

IPv4 and IPv6 are separate targets, on purpose. A host that answers on both is two
measurements — they take different paths and fail independently, and averaging them
together would hide exactly the problem you are looking for.

## Inheritance

Every probe setting is inherited down the tree. A setting left unset inherits from the
nearest ancestor that sets it; the root sets all of them, so resolution always terminates.

```toml
[defaults]
interval_s = 60          # everything is measured every minute…

[targets."Datacenter"]
interval_s = 30          # …except the datacenter, every 30 seconds…

[targets."Datacenter/Core/sw-core-01"]
pings_per_interval = 60  # …and this switch, which also gets more pings
```

`sw-core-01` resolves to `interval_s = 30` (from `Datacenter`) and
`pings_per_interval = 60` (its own). The UI shows, for every setting, whether the value is
local or inherited and which node it came from.

`[defaults]` is the root node's own settings. There is no `[targets.""]` entry.

## Limits

A write from the UI or the API, and `config import`, is held to the same limits, so a typo
or a compromised editor cannot take the prober, or the thing it probes, down with it.

| What | Limit |
| --- | --- |
| Tree | 16 levels deep, 10 000 nodes |
| `name` / `title` / `notes` | 128 / 500 / 16 384 characters; a name has no surrounding whitespace |
| `host`, DNS query | 253 characters; HTTP path 2048; agent list 1024 |
| `interval_s` | at most 86 400 |
| `pings_per_interval` | at most 1000; `packet_size` 12 – 9000; `burst_gap_ms` at most 600 000 |
| One target's load | 100 pings/s and 500 000 bytes/s on average |
| All targets together | 5000 pings/s |
| `timeout_ms` | no longer than the interval |
| `trace_interval_s` | 0 (off) or 30 – 604 800 (a week) |
| `retention_s` | 0 (keep everything) or 3600 – 315 360 000 (ten years) |

The limits are applied to what a write **changes**, and a load limit only to a change that
makes the load worse. A value stored before a limit existed keeps working and can be left
alone; the write that touches it is the one that is refused. Reads are bounded in the same
spirit: a range query is refused with 400 when it would read more than 20 million samples
(2 million availability rows, 100 000 path changes), counted from what is stored, not from
how wide the window is.

## Probe settings reference

All of these are inheritable, and all are valid in `[defaults]` and in any `[targets."…"]`
table. "Types" says which probe types read the setting; a setting that does not apply to a
type is stored and ignored. What each type measures: [Probes](probes.md).

| Key | Types | Type | Root default | Constraint | Meaning |
| --- | --- | --- | --- | --- | --- |
| `probe_type` | all | string | `"icmp"` | `icmp`, `dns`, `tcp`, `http`, `https`, `irtt` | What the probes of an interval are |
| `interval_s` | all | int | `60` | 1 – 86 400 | Seconds between measurement intervals |
| `pings_per_interval` | all | int | `20` | 1 – 1000 | Probes per interval: the width of your distribution |
| `probe_mode` | all | string | `"burst"` | `burst` or `spread` | `burst` sends the probes back to back; `spread` distributes them across the interval. See [Probes](probes.md#politeness-how-much-load-a-target-gets) |
| `burst_gap_ms` | all | int | `10` | 0 – 600 000 | Milliseconds between probes in burst mode. `irtt` needs more than 0. `pings_per_interval × burst_gap_ms` must be less than `interval_s × 1000` |
| `timeout_ms` | all | int | `1000` | 1 – 600 000, and no longer than the interval | How long a reply may take before it counts as lost |
| `packet_size` | `icmp`, `irtt` | int | `56` | 12 – 9000 | Payload bytes (`icmp`) or packet length (`irtt`) |
| `dscp` | `icmp`, `dns`, `tcp`, `http`, `https`, `irtt` | int | `0` | 0 – 63 | DSCP marking. Admin-only to set; see [Probes](probes.md#which-settings-apply) |
| `probe_port` | `dns`, `tcp`, `http`, `https`, `irtt` | int | unset | 1 – 65535 | Port; unset means the type's default (`tcp` has none and needs it) |
| `dns_query` | `dns` | string | unset | a domain name | What to ask. Unset asks `.` NS |
| `dns_rr_type` | `dns` | string | unset | `A`, `AAAA`, `CNAME`, `MX`, `NS`, `PTR`, `SOA`, `SRV`, `TXT` | Which record to ask for |
| `http_path` | `http`, `https` | string | unset | a path | What to request; unset means `/` |
| `tls_skip_verify` | `https` | bool | `false` | | Turn off certificate verification for this target. See [Probes](probes.md#internal-certificates) |
| `agents` | all | array | `["local"]` | every name must be enrolled | Which vantage points measure this target |
| `trace_interval_s` | path discovery | int | `300` | 0, or 30 – 604 800 | Seconds between traceroutes; `0` disables path discovery |
| `retention_s` | all | int | `0` | 0, or 3600 – 315 360 000 | Seconds to keep raw measurements; `0` keeps them forever. Older intervals are deleted whole, never downsampled; see [Operations](operations.md). Admin-only to set |
| `graph_series` | `irtt` | string | `all` | `all`, `""`, or names | Which extra per-packet distributions are drawn. Display only |

`agents` is checked against the agents you have enrolled. A name that matches none is refused,
with the offending name and the list of what exists, because the alternative is a target
measured by nobody, which draws like one that never answers. Pass `--allow-unknown-agents` when
the tree is deliberately applied before the agents that will serve it. A space-separated string
(`agents = "local ams-01"`) is still accepted; an export always writes the array form.

### `graph_series`

An `irtt` session measures more per packet than the round trip. smokeng stores all of it; this
setting chooses what is drawn.

| Series | What it is |
| --- | --- |
| `ipdv_send` | How much later or earlier each packet reached the far end than the one before it |
| `ipdv_receive` | The same on the way back |
| `server_processing` | How long the far end held each packet before replying |

```toml
graph_series = "all"                      # every series that has data (the default)
graph_series = "ipdv_send ipdv_receive"   # just the two directions
graph_series = ""                         # none; only the round trip
```

`all` picks up a series added in a later release; an explicit list does not.

This changes what is **shown**, never what is **kept**, so turning a graph on later shows the
history it already has. Where the far end does not return timestamps, the graph says the series
was not measured instead of drawing a flat line at zero. Absolute one-way delay is not graphed;
see [Probes](probes.md#irtt).

### Probe types

`probe_type` is inherited like everything else, so a whole subtree can be measured over HTTPS by
setting it once on the group.

| Type | Measures | Default port | Extra settings |
| --- | --- | --- | --- |
| `icmp` | Echo request to echo reply | none | `packet_size` |
| `dns` | A query against this host as a resolver | 53 | `dns_query`, `dns_rr_type`, `probe_port` |
| `tcp` | The TCP handshake | none; **required** | `probe_port` |
| `http` | Request to response headers | 80 | `http_path`, `probe_port` |
| `https` | As `http`, including the TLS handshake | 443 | `http_path`, `probe_port`, `tls_skip_verify` |
| `irtt` | A UDP session against `irtt server` | 2112 | `packet_size`, `probe_port` |

```toml
[targets."Klanten/GemeenteX/portaal"]
host = "portaal.gemeentex.nl"
address_family = "v4"
probe_type = "https"
http_path = "/health"

[targets."Klanten/GemeenteX/resolver"]
host = "10.20.0.53"
address_family = "v4"
probe_type = "dns"
dns_query = "gemeentex.nl"
dns_rr_type = "A"
```

Semantics, loss rules and caveats per type are in [Probes](probes.md). Internal certificates
and the irtt HMAC keyfile are there too.

## Presentation and lifecycle

These are per-node and are not inherited.

| Key | Type | Default | Meaning |
| --- | --- | --- | --- |
| `title` | string | — | Display name; the path is used when absent |
| `notes` | string | — | Free-form description |
| `hidden` | bool | `false` | Keep measuring, but hide from the graph list |
| `disabled` | bool | `false` | Stop measuring; history is kept |
| `sort_order` | int | `0` | Ordering within the parent, before falling back to name |

`disabled` is the safe way to retire a target: probing stops, the data stays.

## Alert rules in TOML

Rules inherit by replacement, not accumulation; see [Alerting](alerting.md).

```toml
[default_alerts.loss]
metric = "loss"
op = ">"
threshold = 20
for_intervals = 3
clear_intervals = 3

[targets."Datacenter/Core/sw-core-01".alerts.latency]
metric = "median"
op = ">"
threshold = 5

# A distribution-shape rule. mode is auto or tunable; baseline (shape only) is
# rolling or golden. A golden rule's reference is captured from the UI or the
# API — it is measured data, not something a config file can state.
[targets."Datacenter/Core/sw-core-01".alerts."shape shift"]
metric = "shape"
op = ">"
threshold = 4   # in auto mode this is the z cutoff; 4 is only the UI form default
mode = "auto"
baseline = "rolling"
```

See [Alerting](alerting.md) for the semantics of each field.

Alert rules follow the same declarative rule as targets: one that is in the database
but absent from the file is **disabled**, not deleted. That applies to rules created in
the UI too, so a rule you want to keep belongs in the file — which is why `mode` and
`baseline` are writable here, and a shape rule can live in version control like any
other.

## A complete example

```toml
[defaults]
interval_s = 60
pings_per_interval = 20
probe_mode = "burst"
trace_interval_s = 300

[default_alerts.loss]
metric = "loss"
op = ">"
threshold = 20

[targets."Internet"]
title = "Internet"

[targets."Internet/cloudflare-v4"]
host = "1.1.1.1"
address_family = "v4"

[targets."Internet/cloudflare-v6"]
host = "2606:4700:4700::1111"
address_family = "v6"

[targets."Customer"]
title = "Customer sites"
interval_s = 30
agents = ["local", "ams-01", "rtd-01"]

[targets."Customer/gemeente-gw"]
host = "198.51.100.10"
address_family = "v4"
title = "Municipal gateway"
notes = "Terminates the IPsec tunnel; ask NOC before restarting."

[targets."Customer/gemeente-gw".alerts.strict-loss]
metric = "loss"
op = ">"
threshold = 5
for_intervals = 2
```

## Editing without TOML

Everything above is also editable in the web UI under **Targets**, by a user with the admin
role. The prober picks up changes without a restart, and `config export` includes them. An edit
from the UI or the API is one transaction from the read to the commit, so two edits that would
together make a cycle cannot both succeed. Deleting a node deletes its alert rules and their
state with it.
