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
destroys history. Add `--prune` to delete instead of disable — a command-line-only
option. The same operation is available in the web UI under **Targets** ("Import TOML"),
which calls `PUT /api/v1/config`; it never prunes, so from the UI absence always means
disabled, never deleted.

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
| `probe_type` | string | `"icmp"` | `icmp`, `dns`, `tcp`, `http`, `https`, `irtt` | What the N probes of an interval are — see [Probe types](#probe-types) |
| `interval_s` | int | `60` | > 0 | Seconds between measurement intervals |
| `pings_per_interval` | int | `20` | > 0 | Probes sent per interval — this is the width of your distribution |
| `probe_mode` | string | `"burst"` | `burst` or `spread` | See below |
| `burst_gap_ms` | int | `10` | ≥ 0 | Milliseconds between probes in burst mode |
| `timeout_ms` | int | `1000` | > 0 | How long a reply may take before it counts as lost |
| `packet_size` | int | `56` | 12–65000 | Payload bytes; `icmp` and `irtt` only |
| `dscp` | int | `0` | 0–63 | DSCP marking, for measuring a specific traffic class |
| `probe_port` | int | unset | 1–65535 | Port for the types that have one; unset means the type's default |
| `dns_query` | string | unset | a domain name | What a `dns` probe asks for |
| `dns_rr_type` | string | unset | `A`, `AAAA`, `CNAME`, `MX`, `NS`, `PTR`, `SOA`, `SRV`, `TXT` | Which record a `dns` probe asks for |
| `http_path` | string | unset | a path | What an `http` or `https` probe requests; unset means `/` |
| `tls_skip_verify` | bool | `false` | | Turn off certificate verification for an `https` probe — read the caveat below |
| `agents` | array | `["local"]` | every name must be enrolled | Which vantage points measure this target |
| `trace_interval_s` | int | `300` | ≥ 0 | Seconds between traceroutes; `0` disables path discovery |

`agents` is checked against the agents you have actually enrolled. A name that
matches none of them is refused, with the offending name and the list of what
exists — because the alternative is a target measured by nobody, drawing exactly
like one that is measured and never answers. Pass `--allow-unknown-agents` when
the tree is deliberately applied before the agents that will serve it.

A space-separated string (`agents = "local ams-01"`) is still accepted and means
the same thing; an export always writes the array form.

In burst mode the pings go out back to back, `burst_gap_ms` apart, and the distribution
describes the network *at one moment*. In spread mode they are distributed evenly across
the interval, and the distribution describes the network *over the interval*. Both are
legitimate; they answer different questions. Burst is the SmokePing-compatible default.

A burst must fit inside its interval: `pings_per_interval × burst_gap_ms` must be less
than `interval_s × 1000`, or the import is rejected.

## Probe types

`probe_mode` says *when* the probes of an interval go out. `probe_type` says *what* they
are. It is inherited like everything else, so a whole subtree can be measured over HTTPS
by setting it once on the group.

Every type produces a distribution of N round-trip times per interval. That is the
admission rule, not a coincidence: a check that yields one number, or an up/down verdict,
is a different kind of instrument and does not belong on these graphs.

| Type | Measures | Default port | Extra settings |
| --- | --- | --- | --- |
| `icmp` | Echo request to echo reply | — | `packet_size` |
| `dns` | A query against this host **as a resolver** | 53 | `dns_query`, `dns_rr_type`, `probe_port` |
| `tcp` | The TCP handshake — SYN to SYN-ACK | none; **required** | `probe_port` |
| `http` | Request to first response byte | 80 | `http_path`, `probe_port` |
| `https` | As `http`, including the TLS handshake | 443 | `http_path`, `probe_port` |
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

### What each one is for

**`icmp`** is the default and the cheapest. It measures the path, not the service.

**`dns`** points at a resolver and times a query against it. The host is the resolver
being measured, not the name being asked about. A resolver going slow is invisible to
ICMP — the box answers pings promptly while every lookup behind it crawls.

**`tcp`** measures a handshake, which goes through a queue ICMP does not share. A router
that deprioritises ICMP, or a middlebox that answers pings on a dead host's behalf, both
look healthy to an echo request. There is no default port and none is guessed: a `tcp`
target without `probe_port` is **not measured at all**, and says so in the log and on the
target's page in the UI.

**`http`/`https`** measure the service rather than the path. Each probe builds a fresh
connection — TCP, then TLS for `https`, then the request — because reusing one would make
the first sample of a session include a handshake the others skipped, and a distribution
split between two unrelated populations is worse than a slower one. Timing stops at the
response headers: how fast a page transfers is a bandwidth question, and mixing it in
would make a large page look like a slow network.

Two consequences worth knowing before you point one at production:

- **A response of 400 or above counts as loss.** The transport worked and the server
  answered, but it did not serve what was asked for, and drawing a healthy green band over
  an outage would be the wrong answer. The status is named once per interval in the log,
  so "100% loss" does not send you looking at the network for a fault in the application.
- **Redirects are not followed.** A 3xx is one round trip to this host and is recorded as
  such. Following it would add a second round trip, to a second host, and report the pair
  as one measurement of the first.
- **Certificates are verified**, and an unverifiable one reads as total loss. For an
  internal PKI the right answer is to trust the issuing CA — see below — which keeps
  verification on. `tls_skip_verify = true` turns it off for one target, as a last resort.

**`irtt`** needs [`irtt server`](https://github.com/heistp/irtt) running at the far end,
so it only works where you control both sides. What it buys is a measurement the network
has no reason to treat specially: ordinary UDP, not rate-limited by the control plane the
way ICMP echo is on most routers, and never answered by a middlebox on the target's
behalf. It runs one session per interval rather than N independent probes — the far end
paces the train itself and reports each packet — and it honours `probe_mode`, so switching
a target between `icmp` and `irtt` changes what the packets are, not when they go out.

IRTT also measures one-way delay in each direction, which is more than smokeng stores.
Only the round trip is kept: a measurement here is one distribution per interval, and
splitting it would change what a measurement *is*.

### Internal certificates

Most `https` targets worth watching are internal, and an internal certificate is issued by
a CA the host does not trust. Two ways out, and they are not equivalent.

**Trust the CA.** Verification stays on; the probe simply knows who signed the certificate.

```bash
smokeng serve --tls-ca-file /etc/ssl/certs/gemeentex-root.pem
```

The flag takes a comma-separated list, and each file may hold any number of certificates.
They are added to the system roots rather than replacing them, so public endpoints keep
working — a file that parses to no certificate at all is an error rather than a silent
no-op, because the intent was to trust something.

It is a flag rather than a target setting because a CA is a property of the deployment,
not of one measurement — and trusting a CA is not the same as measuring anything through
it.

**Remote agents get these CAs from the master**, delivered with their assignments, so a
root is placed in one file and a rotation reaches every agent within a poll. They apply to
https probes only, never to an agent's own connection to its master. An agent can add its
own with `--tls-ca-file`, or refuse the master's with `--no-remote-cas`; see
[Remote agents](agents.md#certificates-the-agent-has-to-trust).

**Or turn verification off for that target.**

```toml
[targets."Klanten/GemeenteX/oud-apparaat"]
host = "10.20.0.9"
address_family = "v4"
probe_type = "https"
tls_skip_verify = true
```

It is inheritable like any other setting, so it can cover a subtree — but prefer not to.
A measurement taken this way says something answered on port 443; it does not say it was
the service this target names. smokeng shows it on the target's page and next to the graph
for exactly that reason, rather than leaving it to be discovered in a settings table.

There is no instance-wide switch for it. Turning verification off is a decision about one
endpoint, and a flag that quietly weakened every https target at once is not a decision
anybody reviewed.

### Timing accuracy

Only `icmp` can be kernel-timestamped. Every other type is timed around a userspace call,
so every measurement they produce carries the `userspace TX/RX` quality flag and is
labelled as such on the graph. That is not a defect being hidden — it is the reason the
flag exists. A band widened by a busy prober must never be readable as a slow service.

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

Everything above is also editable in the web UI under **Targets**, by a user with the
admin role. Changes made there are picked up by the prober without a restart, and
`config export` will include them.
