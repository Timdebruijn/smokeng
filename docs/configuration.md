# Configuration

The database is the source of truth. TOML is how you get configuration into it and back
out of it again, so a target tree can live in Git and be applied like any other
infrastructure change.

```bash
smokeng config import --db smokeng.db targets.toml   # apply a file
smokeng config export --db smokeng.db > targets.toml # write the tree back out
```

`import` is declarative and idempotent. Anything present in the file is created or
updated; anything missing from it is **disabled**, so a mistake in the file never silently
destroys history. Add `--prune` to delete instead of disable.

`export` writes only *local* values — settings a node sets itself, not the ones it
inherits — so an export can be re-imported without flattening the tree.

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

## Probe settings reference

All of these are inheritable, and all are valid in `[defaults]` and in any `[targets."…"]`
table.

| Key | Type | Root default | Constraint | Meaning |
| --- | --- | --- | --- | --- |
| `interval_s` | int | `60` | > 0 | Seconds between measurement intervals |
| `pings_per_interval` | int | `20` | > 0 | Echo requests sent per interval — this is the width of your distribution |
| `probe_mode` | string | `"burst"` | `burst` or `spread` | See below |
| `burst_gap_ms` | int | `10` | ≥ 0 | Milliseconds between pings in burst mode |
| `timeout_ms` | int | `1000` | > 0 | How long a reply may take before it counts as lost |
| `packet_size` | int | `56` | 12–65000 | ICMP payload bytes |
| `dscp` | int | `0` | 0–63 | DSCP marking, for measuring a specific traffic class |
| `agents` | string | `"local"` | non-empty | Space-separated agent names that probe this target |
| `trace_interval_s` | int | `300` | ≥ 0 | Seconds between traceroutes; `0` disables path discovery |

In burst mode the pings go out back to back, `burst_gap_ms` apart, and the distribution
describes the network *at one moment*. In spread mode they are distributed evenly across
the interval, and the distribution describes the network *over the interval*. Both are
legitimate; they answer different questions. Burst is the SmokePing-compatible default.

A burst must fit inside its interval: `pings_per_interval × burst_gap_ms` must be less
than `interval_s × 1000`, or the import is rejected.

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

Alert rules are inheritable too, but they **replace** rather than accumulate: a node that
declares any rules uses exactly those, and ignores its ancestors'. This makes an
exception explicit — you can see the whole rule set of a node in one place, instead of
reconstructing it from four ancestors.

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
```

See [Alerting](alerting.md) for the semantics of each field.

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
agents = "local ams-01 rtd-01"

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

Everything above is also editable in the web UI under **Targets**, by a user with the
admin role. Changes made there are picked up by the prober without a restart, and
`config export` will include them.
