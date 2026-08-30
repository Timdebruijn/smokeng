# smokeng

A latency monitoring tool in the spirit of [SmokePing](https://oss.oetiker.ch/smokeping/),
rebuilt from scratch. The one thing that makes it worth existing: it keeps the **full RTT
distribution per measurement interval, forever, at full resolution**, and renders it as
actual density — no rollup, no consolidation, no single-value-per-check.

![Three targets in the smokeng UI: the density smoke, the pooled median line and the loss
rail, stacked on a shared time axis](docs/images/smokeng.png)

**Status: released, [v0.7.1](https://github.com/timdebruijn/smokeng/releases/tag/v0.7.1).**
The design is agreed and frozen in
[DESIGN.md](DESIGN.md). Working end to end: the prober — ICMP, DNS, TCP-connect, HTTP(S)
and IRTT, burst and spread, kernel
kernel timestamping for ICMP and DNS with an observable fallback, and a userspace flag on
the types that cannot be stamped — the SQLite store, TOML import/export of the
target tree, a SmokePing `Targets` importer, the Arrow measurements API, the browser
renderer — density smoke, pooled median, loss rail, stacked plots with a shared
crosshair, brush-zoom to free time ranges, a log y-axis — and an admin UI that edits the
target tree and shows, per setting, whether a value is set here or inherited and from
where; alerting with webhook delivery; OIDC login, with grants that scope a user to one
subtree and show them nothing else; remote agents that enrol themselves with a one-time
token and push signed measurements; and path correlation.


## Documentation

The rest of this file is a technical overview. The task-oriented guides live in
[`docs/`](docs/README.md):

- [Getting started](docs/getting-started.md) — install, run, add your first target
- [Configuration](docs/configuration.md) — the complete TOML reference and the target tree
- [Reading the graphs](docs/reading-graphs.md) — what the smoke, the loss rail and the quality badges mean
- [Alerting](docs/alerting.md) · [Remote agents](docs/agents.md)
- [Authentication](docs/authentication.md) · [Access control](docs/access-control.md) — scoping a user to one subtree
- [Operations](docs/operations.md) — storage growth, backups, Prometheus metrics, systemd
- [Migrating from SmokePing](docs/migrating-from-smokeping.md)

## Install

Download a binary from the [releases page](https://github.com/timdebruijn/smokeng/releases)
— one static, CGO-free file with the frontend embedded, for linux/amd64, arm64, arm and
386, and darwin/amd64 and arm64:

```bash
curl -fsSLO https://github.com/timdebruijn/smokeng/releases/latest/download/smokeng-linux-amd64
curl -fsSL  https://github.com/timdebruijn/smokeng/releases/latest/download/SHA256SUMS |
  grep smokeng-linux-amd64 | sha256sum -c -
chmod +x smokeng-linux-amd64 && sudo mv smokeng-linux-amd64 /usr/local/bin/smokeng
```

Or build it:

```bash
go install github.com/timdebruijn/smokeng/cmd/smokeng@latest
```

## Build from source

Requirements: Go 1.27+. Node 22+ only when rebuilding the frontend (`web/dist` is
committed, so `go build` alone always produces a working binary — and CI fails if that
committed output ever drifts from the source).

```
make            # rebuild frontend + binary
make build      # binary only, no Node needed
make check      # what CI runs: gofmt, vet, tests, frontend typecheck
make dist       # release binaries for every supported platform, with checksums
```

The Linux-only tests — kernel timestamping, receive-queue drops, ICMP errors, path
discovery — skip unless unprivileged ICMP sockets are permitted, so run them with the
sysctl set or they will pass by not running:

```bash
sudo sysctl -w net.ipv4.ping_group_range="0 2147483647"
```

Run:

```
./bin/smokeng config import --db smokeng.db targets.toml
./bin/smokeng serve --db smokeng.db --listen 127.0.0.1:8080
```

A minimal `targets.toml`:

```toml
[defaults]
interval_s = 60
pings_per_interval = 20

[targets."Internet/cloudflare-v4"]
host = "1.1.1.1"
address_family = "v4"
```

Settings cascade down the tree; an unset key means "inherit". Alert rules live in the
same file, on the node they apply to:

```toml
[default_alerts."any loss"]        # on the root, so it covers everything
metric = "loss"
op = ">"
threshold = 10

[targets."Internet/cloudflare-v4".alerts."jitter"]
metric = "spread"                  # p95 − p5, in milliseconds
op = ">"
threshold = 25
for_intervals = 5                  # omitted hysteresis defaults to 3, never to 1
```

`config export` writes the whole thing back out, round-tripping exactly, so the file is a
complete description of the configuration rather than half of one. Import is declarative
in both: anything absent from the file is disabled, and `config import --prune` deletes
it instead — `--prune` is a command-line-only flag. The same file can be applied from the
web UI under **Targets → Import TOML**, which goes through `PUT /api/v1/config` and never
prunes, so from there absence always disables. The summary reports targets and rules
separately, because noticing "2 alert rules disabled" at import time is better than
discovering it during an incident.

Coming from SmokePing, import its `Targets` file directly:

```bash
smokeng config import-smokeping --db smokeng.db --dry-run /etc/smokeping/config.d/Targets
```

The `+`/`++`/`+++` hierarchy becomes the target tree, per-node keys become local settings
so inheritance survives, and `probe = FPing6` or a literal address decides the address
family that smokeng insists on stating. Anything smokeng deliberately does not implement
— alert definitions, alternative hierarchies, multi-host overlay graphs, `DYNAMIC` hosts
— is reported as a warning rather than dropped in silence. Drop `--dry-run` to write it.

Without `--oidc-issuer` there is no authentication, so `serve` refuses to listen on
anything but loopback unless you pass `--i-know-this-is-unauthenticated`. See
**Authentication** below.

## Unprivileged ICMP (Linux)

smokeng pings from unprivileged datagram ICMP sockets — no root, no `CAP_NET_RAW`, no
subprocess per measurement. The kernel gates this behind a sysctl listing the group IDs
allowed to create such sockets:

```
sudo sysctl -w net.ipv4.ping_group_range="0 2147483647"
```

Persist it in `/etc/sysctl.d/50-smokeng.conf`:

```
net.ipv4.ping_group_range = 0 2147483647
```

The setting covers ICMPv6 datagram sockets as well. Narrow the range to the GID smokeng
runs as if you prefer. If the datagram socket is not permitted, smokeng falls back to raw
sockets (which need `CAP_NET_RAW`) with a clear error message, and marks all affected
measurements with a degraded-accuracy flag.

## Timestamping accuracy

On Linux, packets are timestamped by the kernel in both directions
(`SO_TIMESTAMPING`: RX from the reply's control messages, TX read back from the socket
error queue), so scheduler jitter on a busy host does not widen the smoke. On other
platforms (including macOS, supported for development) userspace timestamps are used
instead — recorded per measurement in a flags field, never silently.

This is not a theoretical difference. Loopback bursts of 10 pings, kernel timestamping
versus the userspace fallback:

| Timestamps | Median RTT | Median intra-burst spread |
|---|---|---|
| kernel (Linux, `flags=0`) | ~15–31 µs | **26 µs** |
| userspace (macOS, `flags=3`) | ~270–650 µs | **563 µs** |

Roughly 20× more apparent jitter from the measurement method alone — noise that would be
drawn as smoke and read as network variance. The two rows are different hosts, so treat
the ratio as indicative rather than a controlled benchmark; the order of magnitude is
what matters.

One kernel quirk worth knowing: Linux gates RX timestamping behind a static key that
goes cold when nothing has requested timestamps for a few seconds. The first packets
after that arrive with no `SCM_TIMESTAMPING` at all. smokeng handles this by design —
those measurements are stamped in userspace and carry `FlagUserspaceRX` — so expect the
occasional flagged measurement right after startup, and on targets whose interval is long
enough that the socket goes quiet between bursts. Nothing is silently mis-measured; the
flag is the record.

## Alerting

Rules hang on nodes of the target tree and inherit downward, replacing rather than
adding: the nearest ancestor that defines any rules defines the whole set for its
subtree. A rule tests `loss`, `median`, `p95` or `spread` against a threshold — the last
two exist only because smokeng keeps whole distributions.

Rules are edge-triggered with hysteresis in both directions: the condition must hold for
N consecutive intervals before it fires, and fail for M before it clears. A single bad
interval never pages anyone, a gap in the series resets the count rather than bridging
intervals nobody measured, and state is persisted so an alert firing for an hour does not
resolve and re-fire across a restart. Measurements flagged as smokeng's own fault
(§ Measurement quality) are excluded from the metrics they would distort.

Delivery is one webhook, in Alertmanager's v2 format:

```bash
smokeng serve --db smokeng.db --alert-webhook http://alertmanager:9093/api/v2/alerts
```

Firing alerts are repeated once a minute, because Alertmanager expires an alert it stops
hearing about. Grouping, silencing, escalation and notification channels are
Alertmanager's job; smokeng does not reimplement them. Rules are evaluated whether or
not `--alert-webhook` is set — firing state and the transition history are live either
way; a missing webhook only means a transition is never posted anywhere.
`GET /api/v1/alerts` reports `enabled` (evaluated) and `delivering` (posted) separately,
rather than conflating the two.

## Path correlation

When the smoke changes shape, the next question is always whether the path changed.
smokeng records the route to each target and marks every change on the same time axis as
the measurements, so "the path changed at 14:02" sits beside "the smoke widened at
14:03". Hovering names the route in force at that instant.

That is the whole feature. There is no standalone traceroute view, no per-hop latency
graph and no path scoring: those are a different product, and this one exists to make the
distribution legible.

Enable it per target with the inheritable `trace_interval_s` (0 disables it), defaulting
to five minutes — a traceroute costs a round trip per hop, and a route changes on a scale
of days rather than seconds. Only changes are stored, the same way DNS resolutions are,
so a stable route costs one row rather than one per run.

Hops are found with TTL-limited probes, reading each router's address from the ICMP
time-exceeded reply on the socket error queue. That needs Linux; elsewhere no route is
recorded rather than a wrong one.

## Remote agents

A target can be measured from more than one place. Assign it with the inheritable
`agents` setting — `agents = "local ams rtm"` — and each vantage point becomes its own
series, drawn as its own plot. They are never averaged: two views of a path that disagree
are the finding, and averaging them destroys it.

Agents push to the master, so only the master needs to be reachable and an agent works
behind NAT with no inbound rules. The short way to enrol one is a one-time token, minted
in the UI under **Agents** (or `POST /api/v1/agent-tokens`), single-use and short-lived:

```bash
smokeng agent run --master https://smokeng.example.org --token smk_...
```

The agent generates its keypair if it has none, enrols itself, and records the id it was
given — a unit file may keep carrying `--token`, since the recorded id wins and a
restart does not try to spend a token that is already spent. Without a token, the key can
still be carried by hand, exactly as before:

```bash
# On the agent: generates a key on first run and prints its public half.
smokeng agent run --key /etc/smokeng/probe.key

# On the master:
smokeng agent add --db smokeng.db --name ams --pubkey <the printed key>

# Back on the agent, with the id the master just assigned:
smokeng agent run --master https://smokeng.example.org --agent-id 1 \
  --key /etc/smokeng/probe.key --db /var/lib/smokeng/agent.db
```

`smokeng agent list`, `enable`, `disable` and `remove` manage them afterwards. An agent
that has already reported **cannot be removed** — its measurements are labelled by agent
and deleting it would leave a series nothing can name — so disable it instead: probing
stops, the history stays. See [Remote agents](docs/agents.md) for the full protocol.

Every request carries an Ed25519 signature over a canonical string that binds the method,
path, agent, timestamp, nonce and a hash of the body — so a captured signature cannot be
replayed against another endpoint or with another payload. The master holds only public
keys, so compromising its database does not let an attacker impersonate an agent. A
submission is refused unless every target in it is assigned to that agent, and writes
upsert on `(target, agent, interval)`, which makes a replayed batch a byte-identical
no-op. That idempotency, not the nonce cache, is the real replay defense: the cache is
in-memory and empty after a restart.

Agents pull their assignments from `GET /api/v1/agent/targets` — resolved settings and
nothing else. Pure data, pull-only, never code. (SmokePing's equivalent has slaves
*evaluate Perl sent by the master*; this shares none of that.) Results are written to the
agent's own database first and only forgotten once the master confirms them, so an
unreachable master costs latency rather than measurements.

TLS is expected everywhere the traffic can be observed. A master on a literal loopback
address is exempt — those packets never reach an interface — and `--insecure-allow-http`
covers anything else that is not HTTPS, and says so.

## Authentication

There are no local accounts and no password handling. Point smokeng at an OIDC provider:

```bash
smokeng serve --db smokeng.db --listen 0.0.0.0:8080 \
  --oidc-issuer https://id.example.org/application/o/smokeng/ \
  --oidc-client-id smokeng --oidc-client-secret "$SECRET" \
  --oidc-redirect-url https://smokeng.example.org/auth/callback \
  --oidc-admin-value smokeng-admins
```

**admin** is global and comes from a claim (`--oidc-admin-claim`, default `groups`);
anything not recognised as an admin is not one, so a provider that renames or drops the
claim demotes people rather than promoting them. Leaving `--oidc-admin-value` empty makes
every authenticated user an admin, which is logged loudly at startup rather than left to
be discovered.

Everyone else gets what their **grants** give them. A grant gives an OIDC group `viewer`
or `editor` on one node and its subtree, and the isolation is total: that subtree is
presented as though it were the whole installation, so one customer never learns that
there are others. Grants live in smokeng's own table, round-trip through TOML, and never
reach agents, the root defaults, `/metrics` or `config import`. What an authenticated user
with no grant may do is `--default-role`, which stays `viewer` — the behaviour before
grants existed — until you set it to `none`. See
[Access control](docs/access-control.md).

Sessions are held in an HMAC-signed cookie whose key is stored in the database, so they
survive a restart; deleting the `session_key` row invalidates every session at once.
Without `--oidc-issuer`, smokeng runs unauthenticated and refuses to listen anywhere but
loopback unless explicitly overridden.

## Monitoring smokeng itself

`/metrics` serves Prometheus text exposition about smokeng's own health: targets being
probed, measurements written, write errors, dropped measurements, late replies, socket
overflows, DNS failures, agent enrolment and last contact, ingest accepted and rejected,
and alerts firing.

What it deliberately does not carry is measurement data. Latency and loss live in the
store at full resolution and are read as Arrow; pushing them through Prometheus would
flatten every interval to a single number, which is the exact loss this project exists to
avoid. These metrics answer "is smokeng healthy", never "what is the network doing" — and
a test asserts that no sample value ever comes from a measurement.

The endpoint names agents and counts targets, so it sits behind the session like every
other read. Open it for a scraper explicitly:

```bash
smokeng serve --db smokeng.db --metrics-public
```

Two counters deserve a standing alert of your own: `smokeng_measurements_dropped_total`
above zero means measurements were taken and then lost because the writer fell behind,
and `smokeng_socket_overflow_measurements_total` climbing means the loss you are looking
at is partly smokeng's rather than the network's.

## Measurement quality

Every measurement records the conditions it was taken under, because a number is only
worth as much as the conditions behind it. The graphs badge any target whose window
contains flagged measurements.

| Flag | Meaning |
|---|---|
| `userspace TX` / `userspace RX` | Timestamps taken in userspace; scheduler jitter widens the smoke. |
| `raw socket` | Unprivileged datagram ICMP was unavailable. |
| `dropped replies` | The socket's receive queue overflowed: some loss shown is smokeng's, not the network's. |
| `clock step` | The wall clock jumped during the interval, so its RTTs are unreliable. |
| *the ICMP error by name* | Probes were refused with an ICMP error instead of going unanswered. |
| `send refused` | The local stack would not transmit some probes: no route, or a local firewall rule. |

Two of these deserve a note.

**Dropped replies.** One socket carries every target of an address family, and
simultaneous bursts can deliver thousands of packets at once — well past the usual
~208KB default receive buffer. smokeng asks for 4MB and warns if `net.core.rmem_max`
caps it lower. It then polls the kernel's per-socket drop counter from `/proc/net/icmp`
(or `raw`/`icmp6`/`raw6`), and any interval during which that counter moves is flagged.
Without this, a host that is merely busy reads as a lossy network. Note that the obvious
mechanism, `SO_RXQ_OVFL`, does not work here: `setsockopt` accepts it on a ping socket
and then never reports anything, because `ping_recvmsg` does not attach the counter.

**ICMP errors.** A target at 100% loss because a firewall says "administratively
prohibited" and one at 100% loss in silence are different findings that call for
different responses, so smokeng keeps them apart. `IP_RECVERR` routes ICMP errors for our
probes onto the socket error queue, which returns the offending datagram alongside them —
our own echo request, whose sequence number attributes the error to an exact ping. The
graphs name the error rather than reporting a generic failure. A probe the kernel
declines to send is counted as attempted and lost for the same reason: an unreachable
target should render as total loss, not as an empty graph.

**Clock steps.** Kernel timestamps are `CLOCK_REALTIME`, so a clock jump lands directly
in the RTTs and would be drawn as a latency spike that never happened. smokeng compares
how far the wall clock and the monotonic clock advanced over each interval; a divergence
beyond what NTP slewing could explain marks the measurement. The tolerance is rate-based
because slew is a rate, and because platforms differ: Linux slews `CLOCK_MONOTONIC`
alongside `CLOCK_REALTIME`, while macOS leaves `mach_absolute_time` untouched, so on
macOS ordinary slewing shows up in this comparison and must not be mistaken for a step.

Verified on Linux 7.1: aarch64 natively (full kernel path, plus an end-to-end run where
every measurement was written with `flags=0`), and x86-64 under qemu-user, where the
missing `SO_TIMESTAMPING` support exercised the fallback path instead. The structures
read from control messages are architecture-independent — `sock_extended_err` is
fixed-width on every Linux port — and compile-time assertions in
`internal/probe/timestamp` fail the build if that ever stops being true. The package
builds for linux/amd64, arm64, 386, arm and riscv64.

## Layout

| Path | Contents |
|---|---|
| `cmd/smokeng` | the binary: `serve`, `config`, `agent`, `version` |
| `internal/tree` | target tree, inheritance resolution with provenance |
| `internal/probe` | scheduler + the probe engines (icmp, dns, tcp, http/s, irtt) |
| `internal/probe/timestamp` | kernel packet timestamping and the socket error queue |
| `internal/probe/dnscache` | TTL-respecting hostname resolution |
| `internal/probe/trace` | path discovery, to see whether the route changed |
| `internal/store` | storage interface, SQLite backend |
| `internal/store/enc` | versioned samples-blob codec |
| `internal/api` | HTTP API, Arrow serialization, embedded web UI |
| `internal/auth` | OIDC login and the two roles |
| `internal/config` | TOML import/export, SmokePing importer |
| `internal/alert` | rule evaluation, hysteresis, webhook notification |
| `internal/agent` | remote node: assignment pull, local buffer, signed push |
| `internal/ingest` | signed agent protocol: batch push, assignment pull |
| `internal/metrics` | Prometheus exposition of smokeng's own health |
| `web` | React frontend, embedded into the binary |

See [DESIGN.md](DESIGN.md) for the data model, rendering pipeline, agent protocol and
the explicit non-goals.

## Licence

MIT — see [LICENSE](LICENSE). `SPDX-License-Identifier: MIT`.

The released binaries statically link their dependencies, so each release ships a
`THIRD-PARTY-NOTICES` file with the licences of every module linked into it, generated
from the module cache by `make notices`. Everything smokeng depends on is permissively
licensed (MIT, BSD or Apache-2.0); nothing copyleft is linked in.
