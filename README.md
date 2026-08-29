# smokeng

A latency monitoring tool in the spirit of [SmokePing](https://oss.oetiker.ch/smokeping/),
rebuilt from scratch. The one thing that makes it worth existing: it keeps the **full RTT
distribution per measurement interval, forever, at full resolution**, and renders it as
actual density — no rollup, no consolidation, no single-value-per-check.

**Status: v0.2 feature-complete, unreleased.** The design is agreed and frozen in
[DESIGN.md](DESIGN.md). Working end to end: the ICMP prober (burst and spread, kernel
timestamping with observable fallback), the SQLite store, TOML import/export of the
target tree, a SmokePing `Targets` importer, the Arrow measurements API, the browser
renderer — density smoke, pooled median, loss rail, stacked plots with a shared
crosshair, brush-zoom to free time ranges, a log y-axis — and an admin UI that edits the
target tree and shows, per setting, whether a value is set here or inherited and from
where. Still to come: OIDC and alerting (v0.3), remote agents (v0.4).

Known gap before this is trustworthy in production: ICMP errors (unreachable, TTL
exceeded) are collapsed into plain loss rather than kept distinct.

## Build

Requirements: Go 1.27+. Node 22+ only when rebuilding the frontend (`web/dist` is
committed, so `go build` alone always produces a working binary).

```
make            # rebuild frontend + binary
make build      # binary only, no Node needed
make test
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

Settings cascade down the tree; an unset key means "inherit". `config export` writes the
tree back out, round-tripping exactly, and `config import --prune` deletes targets absent
from the file instead of merely disabling them.

Coming from SmokePing, import its `Targets` file directly:

```bash
smokeng config import-smokeping --db smokeng.db --dry-run /etc/smokeping/config.d/Targets
```

The `+`/`++`/`+++` hierarchy becomes the target tree, per-node keys become local settings
so inheritance survives, and `probe = FPing6` or a literal address decides the address
family that smokeng insists on stating. Anything smokeng deliberately does not implement
— alert definitions, alternative hierarchies, multi-host overlay graphs, `DYNAMIC` hosts
— is reported as a warning rather than dropped in silence. Drop `--dry-run` to write it.

There is no authentication before v0.3 (OIDC). `serve` therefore refuses to listen on a
non-loopback address unless you pass `--i-know-this-is-unauthenticated`.

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

Two of these deserve a note.

**Dropped replies.** One socket carries every target of an address family, and
simultaneous bursts can deliver thousands of packets at once — well past the usual
~208KB default receive buffer. smokeng asks for 4MB and warns if `net.core.rmem_max`
caps it lower. It then polls the kernel's per-socket drop counter from `/proc/net/icmp`
(or `raw`/`icmp6`/`raw6`), and any interval during which that counter moves is flagged.
Without this, a host that is merely busy reads as a lossy network. Note that the obvious
mechanism, `SO_RXQ_OVFL`, does not work here: `setsockopt` accepts it on a ping socket
and then never reports anything, because `ping_recvmsg` does not attach the counter.

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
| `cmd/smokeng` | the binary: `serve`, `config`, (v0.4) `agent` |
| `internal/tree` | target tree, inheritance resolution with provenance |
| `internal/probe` | scheduler + ICMP engine (skeleton) |
| `internal/store` | storage interface, SQLite backend, samples codec |
| `internal/api` | HTTP API, Arrow serialization |
| `internal/config` | TOML import/export, SmokePing importer (skeleton) |
| `internal/ingest` | signed agent ingest, v0.4 (skeleton) |
| `web` | React frontend, embedded into the binary |

See [DESIGN.md](DESIGN.md) for the data model, rendering pipeline, agent protocol and
the explicit non-goals.
