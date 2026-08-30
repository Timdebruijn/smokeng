# smokeng — Design Document

Status: **agreed 2026-08-29** (including trade-off #9). Implementation follows this
document; deviations require explicit sign-off first.

## 1. Premise

smokeng keeps the full RTT distribution per measurement interval, forever, at full
resolution, and renders it as actual density. Every design decision below is subordinate
to that: if a choice makes the distribution cheaper but lossy, it is the wrong choice.
No consolidation, no rollup, no pre-aggregation — at write time or at render time.

## 2. Architecture overview

One Go binary, frontend embedded via `embed`. Modes:

- `smokeng serve` — the master: scheduler + probing engine + SQLite + HTTP API + web UI.
  In a single-host deployment this is the entire system.
- `smokeng agent` (v0.4) — a remote prober that runs the same probing engine and pushes
  signed measurement batches to the master. Not built until v0.4, but the wire format and
  schema are fixed now (§9).

**The prober is separable from the UI, and this is how.** `smokeng agent run` pointed at
loopback is a prober in its own process on the master's own host — the same signed path a
prober in another datacentre uses, deliberately not a second local-only mechanism. It buys
restarting the UI without interrupting measurement, and keeping the ICMP permission off
the half that serves web pages; it costs a signed HTTP round trip where there was a shared
writer, and a second unit to operate. One process stays the default because it is the
right shape for one host.

It also buys accuracy, but only for the types that need it. `icmp` is unaffected by
construction — kernel timestamps are taken as the packet crosses the wire, so no amount of
userspace work can move them, which is the whole reason §5.2 takes them there. The
userspace-timed types include scheduler delay in the RTT by definition, and a measured
comparison (docs/operations.md) put their p99 3–4× lower in a separate process under load,
with the median almost unchanged. That is the signature of GC and scheduler contention, and
on an instrument built to keep the distribution, a tail inflated by the prober itself is
the one kind of error the design cannot tolerate.

**A separate prober *binary* was considered and rejected.** Measured, it saves 2.7 MB of
18 — modernc.org/sqlite (~4.3 MB) and Arrow (~2.4 MB) dominate, and the agent needs both:
it buffers to SQLite and speaks Arrow ingest. The embedded frontend is 564 KB. Unused code
costs disk, not measurement: it never executes, so it cannot contend for anything. The
separation worth having is the process, not the artifact — and a second artifact would add
a version-skew axis for a 15% download.

What splitting processes does *not* fix is a panic taking everything down, since that
would only move it. Panics are contained where they happen instead: on the per-target loop
and on the per-probe goroutines, which hold no shared lock — narrow on purpose, because a
process that survives a panic into a deadlock is worse off than one that crashed. Systemd
restarts a crash and cannot see a wedge. Contained panics are counted in
`smokeng_probe_panics_total` and the target is rescheduled within `specRefresh`.

Data flow:

```
scheduler ──▶ probing engine ──▶ batch writer ──▶ SQLite
                                                    │
browser ◀── Arrow IPC ◀── HTTP API ◀────────────────┘
   │
   └─▶ Web Worker: density render → OffscreenCanvas
```

**Terminology decision.** The kickoff brief uses "probe" for two unrelated concepts: the
measurement method (SmokePing's probe: burst ICMP, continuous ICMP, …) and the remote
measurement node. This document uses:

- **probe mode** — how a target is measured (burst / spread), an inheritable target setting;
- **agent** — a measurement node. The master's built-in prober is agent 0, "local".

Wire headers accordingly become `X-Agent-Id` etc. This is a rename of the brief's
`X-Probe-Id` scheme, done now because the wire format must not break later.

## 3. Data model

### 3.1 Tables

```sql
CREATE TABLE targets (
  id             INTEGER PRIMARY KEY,
  parent_id      INTEGER REFERENCES targets(id),   -- NULL = root
  name           TEXT NOT NULL,
  host           TEXT,                              -- NULL for group nodes
  address_family TEXT CHECK (address_family IN ('v4','v6')),  -- NULL for groups
  title          TEXT,                              -- display name; falls back to name
  notes          TEXT,                              -- free-form remark, shown in UI
  hidden         INTEGER NOT NULL DEFAULT 0,        -- measured but not shown; ≠ enabled
  enabled        INTEGER NOT NULL DEFAULT 1,
  sort_order     INTEGER NOT NULL DEFAULT 0,
  -- Inheritable settings. NULL = inherit from parent. The root row must have
  -- all of these non-NULL (enforced at the application layer).
  interval_s         INTEGER,
  pings_per_interval INTEGER,
  probe_mode         TEXT CHECK (probe_mode IN ('burst','spread')),
  burst_gap_ms       INTEGER,   -- inter-ping gap within a burst
  timeout_ms         INTEGER,   -- per-ping reply timeout
  packet_size        INTEGER,   -- ICMP payload size in bytes (default 56)
  dscp               INTEGER,   -- DSCP value on outgoing pings (QoS path testing)
  agents             TEXT,      -- space-separated agent names measuring this subtree;
                                -- root default 'local'. See §9.
  UNIQUE (parent_id, name)
);

CREATE TABLE measurements (
  target_id INTEGER NOT NULL,
  agent_id  INTEGER NOT NULL DEFAULT 0,
  ts        INTEGER NOT NULL,             -- interval start, unix seconds, UTC-aligned
  sent      INTEGER NOT NULL,
  received  INTEGER NOT NULL,             -- invariant: == sample count in blob
  flags      INTEGER NOT NULL DEFAULT 0,  -- see §3.4
  samples    BLOB NOT NULL,               -- encoded sorted RTTs, see §3.3
  icmp_error INTEGER,                     -- schema v2; see §3.4
  PRIMARY KEY (target_id, agent_id, ts)
) WITHOUT ROWID;

CREATE TABLE agents (                     -- reserved now, used from v0.4
  id        INTEGER PRIMARY KEY,          -- row 0 = 'local'
  name      TEXT NOT NULL UNIQUE,
  pubkey    BLOB,                         -- Ed25519, NULL for 'local'
  enabled   INTEGER NOT NULL DEFAULT 1,
  last_seen INTEGER
);

CREATE TABLE resolutions (                -- DNS change log, written only on change
  target_id INTEGER NOT NULL,
  ts        INTEGER NOT NULL,
  address   TEXT NOT NULL,
  PRIMARY KEY (target_id, ts)
);
```

Notes:

- `measurements` is `WITHOUT ROWID`: the table is clustered on the primary key, so the
  dominant query (range scan of one series over `[from, to)`) is a sequential B-tree walk.
- The database is the single source of truth for targets from v0.1 on. TOML is an
  import/export format (§7.3), never a file the server watches or patches.
- Address families are separate targets, per the brief. A dual-stack host is two rows.
- `received` is deliberately denormalized (derivable from the blob) so loss queries never
  decode blobs. On any discrepancy the blob is authoritative.
- 100% loss still writes a row (`received = 0`, empty sample section) — the loss rail
  needs those intervals.

### 3.2 Probe modes

Two modes, both v0.1, modeled as the inheritable `probe_mode` setting:

- **burst** — all N pings back-to-back with `burst_gap_ms` spacing (SmokePing-classic).
  The distribution is a snapshot of one moment in the interval.
- **spread** — N pings evenly spaced across the interval (20 pings / 60 s = one per 3 s).
  The distribution samples the whole interval; with a short interval this is effectively
  SmokePing's continuous ping probe.

Measuring one host both ways = two sibling targets (same pattern as address families).

**Further probe types are built** (decided 2026-08-29, revising the kickoff brief's
"no probe buffet" ban; landed 2026-08-30). The seam was already in place —
`measurements` is protocol-agnostic (sent / received / sorted RTT samples) — so it was
the additive migration §13 #6 predicted. The guardrail that replaces the ban: every
probe type must produce an RTT distribution of N samples per interval. A check that
yields a single scalar or an up/down status does not belong here, ever — that is the
road to a generic uptime dashboard. See §3.2b.

### 3.2b Probe types (v0.7)

`probe_mode` says *when* the N probes of an interval go out. `probe_type` says *what*
they are. It is inheritable like every other setting, defaults to `icmp`, and a host
measured two ways is two sibling targets — the same pattern address families already use.

**The guardrail from §3.2 is the admission test, and it is not negotiable:** a probe type
must produce a distribution of N round-trip times per interval. A check yielding one
number, or up/down, does not belong here however useful it might be elsewhere. That is
what keeps this a latency instrument rather than a monitoring suite.

Six types, all built:

| Type | What it times | Why it earns its place |
| --- | --- | --- |
| `icmp` | Echo request to reply | The baseline, and kernel-timestamped |
| `dns` | Query sent to answer received | A resolver going slow looks like a healthy network on ICMP; kernel-timestamped |
| `tcp` | SYN to the handshake completing | Sees firewalls, SYN drops and a port closing, which ICMP cannot |
| `http` / `https` | Request to response headers | Service latency including TLS, for the thing users actually wait on |
| `irtt` | A UDP session against `irtt server` | Ordinary UDP: not control-plane rate-limited like ICMP, and never answered by a middlebox |

**IRTT's one-way delays are not stored.** The type was admitted partly because it can say
*which* direction is slow, and that capability is deliberately left on the floor: a
measurement here is one distribution per interval, and a second and third distribution per
row would change what a measurement *is* rather than add to it. Storing it properly means
revisiting §6, not widening a column.

**IRTT is also the one type that is not N independent probes.** It is a session: the far
end paces the train and reports every packet, so the engine calls it once per interval
rather than once per probe. It still honours `probe_mode` — switching a target between
`icmp` and `irtt` changes what the packets are, not when they leave.

**An irtt server can require an HMAC key, and smokeng carries it as a local secret, never
in the tree.** An open irtt server is a UDP reflection/amplification vector; `--hmac` on
the server closes it to all but key-holders. The key is a shared secret, so putting it in
the target tree — where it would travel through the API and the exported TOML — is exactly
wrong. It lives instead in a keyfile on each prober host, mapping the target's configured
`host:port` to the secret, loaded with `--irtt-hmac-keys`. This is the same shape as
`--tls-ca-file`, with one difference that follows from it being a secret rather than a
public certificate: the master does **not** hand it down to agents (assignments carry only
data), so an agent measuring an HMAC-protected endpoint gets its own keyfile. Keying on the
configured host rather than the resolved address keeps the mapping stable across DNS
changes and lets different servers hold different keys.

**`icmp` and `dns` are kernel-timestamped; the rest are not.** dns runs on a socket of our
own rather than the library's, precisely so SO_TIMESTAMPING can be set on it — a resolver's
latency deserves the same standard as a ping's. `tcp`, `http`, `https` and `irtt` carry
`FlagUserspaceTX|FlagUserspaceRX` unconditionally (§5.2), and cannot be fixed the same way:
their handshakes complete inside the kernel and userspace only observes the call returning,
so there is no packet of ours left to stamp.

The gap is measured, on a two-core Debian VM under load from the instance's own API, as
spread within an interval — the width of the band the graph draws. icmp 8 µs idle and 8 µs
loaded; dns 46 µs and 41 µs; tcp 139 µs and 1764 µs. The kernel-stamped types do not move
at all. That is the whole argument for taking the timestamp in the kernel, in one table.

**Two failure modes are recorded as loss rather than as a sample**, because the alternative
draws a healthy band over an outage. An HTTP response of 400 or above, and a refused TCP
connection. Both are named once per interval in the log, so "100% loss" does not send an
operator looking at the network for a fault in the application.

**Certificate trust is split deliberately into two unequal halves.** Adding a CA
(`--tls-ca-file`, process-wide, additive to the system roots) keeps verification on and is
the answer for an internal PKI. Turning verification off (`tls_skip_verify`, inheritable,
per target) is the escape hatch, and is shown on the target's page and beside its graph —
such a measurement says something answered, not that it was the right service, and that is
a thing the reader has to be told rather than left to find in a settings table.

The asymmetry is the point. The CA is a deployment fact, so it is a flag: a PEM in the
target tree would mean shipping certificates through the API and inheriting them down the
tree. Skipping verification is a decision about one endpoint, so it is a setting, and there
is deliberately **no instance-wide switch** for it — one flag that quietly weakened every
https target at once is not a decision anyone reviewed.

**Agents receive the master's CAs with their assignments** (§9), replaced rather than
accumulated so a withdrawal reaches them, and logged with subject and fingerprint on every
change. This is the one place assignments carry something other than "what to measure", so
it is bounded explicitly: the pool reaches https probes only and never an agent's own
connection to its master, which verifies against the host's trust store. A master therefore
cannot use it to vouch for itself, and there is a test in internal/agent that pins the
separation. The residual capability a compromised master gains — making an agent report a
green measurement for an intercepted endpoint — is over endpoints whose address it already
dictates. `--no-remote-cas` declines the whole arrangement.

**`tcp` has no default port and none is guessed.** A `tcp` target with no `probe_port`
resolved is refused by `validateSpec` before it is ever scheduled, named in the log once,
and flagged on its page in the UI. Guessing 80 would produce a graph of something nobody
asked for.

**Per-type settings** are columns, not a JSON blob, so they inherit and carry provenance
like everything else — a value shown in the UI must be able to say where it came from.
Shared where the meaning genuinely coincides:

- `probe_port` — dns 53, tcp required, http 80, https 443, irtt 2112
- `dns_query`, `dns_rr_type` — what to ask for; the target's host is the server
- `http_path` — what to request; the target's host is the server

A setting belonging to an inactive type is inherited and stored as usual, and simply not
read. The UI shows only the ones the active type uses, because a form offering
`dns_rr_type` on an ICMP target invites a change that does nothing.

**Accuracy is reported, not assumed.** Only `icmp` can carry kernel timestamps: a DNS
query or a TCP handshake is timed around a userspace call, and its RTT therefore includes
scheduler jitter in exactly the way §5.2 describes. Those measurements carry
`FlagUserspaceTX` and `FlagUserspaceRX` from the start. Drawing them without the flags
would imply an accuracy the method cannot deliver, and the whole point of the flags is
that a widened band always has an attributable cause.

`irtt` is the exception in the other direction: it timestamps in the remote agent as well
as locally, which is what lets it separate the two directions — and it needs an irtt
server at the far end, so it is only usable towards hosts the operator runs.

**Load is not symmetrical across types.** Twenty ICMP echoes a minute is nothing; twenty
HTTPS requests a minute to someone else's service is a decision, not a default. Types
carry their own sensible `pings_per_interval` default rather than inheriting the ICMP
one blindly, and the documentation says plainly what each one costs the other end.

### 3.3 Sample encoding

Blob layout, format version 1:

```
byte 0        version (0x01)
varint        first RTT, in MICROSECONDS (1 µs units)
varint*       deltas to each next RTT (non-negative by construction: sorted ascending)
```

- Unit is 1 µs, not the brief's 10 µs: LAN/homelab targets sit at 100–500 µs RTT, where
  10 µs quantization produces visible banding in the smoke. 1 µs is below the noise floor
  of even kernel timestamping, so nothing renderable is lost. Kernel timestamps (ns) are
  rounded to µs at encode time.
- Sample count is implicit (varints are self-delimiting; decode to end of blob) and must
  equal `received`.
- The version byte costs ~3% and buys format evolution without a table migration
  (e.g. a future ns-unit flag).
- Typical size at 20 pings: 1 + ~3 (first value, e.g. 20 ms = 20000 µs) + 19 × ~1.5
  (deltas) ≈ **32 bytes**. No upper bound on RTT, no precision cliff.

Nothing fancier (bit-packing, per-blob compression) is worth it at ~32 bytes; sorted
delta-varint is effectively optimal for this shape of data.

### 3.4 Measurement flags

Bitfield recording *how* the measurement was taken, so degraded accuracy is observable
per row rather than silent (brief requirement):

- bit 0 — TX timestamp from userspace (kernel TX timestamping unavailable)
- bit 1 — RX timestamp from userspace
- bit 2 — raw-socket fallback (datagram ICMP socket not permitted)
- bit 3 — the socket's receive queue overflowed: some loss is ours, not the network's
- bit 4 — at least one probe drew an ICMP error; `icmp_error` names which
- bit 5 — the wall clock stepped during the interval, so its RTTs are unreliable
- bit 6 — the local stack refused to transmit some probes (no route, local firewall)

Added after the design was agreed (2026-08-29), on the same principle as the original
three: a measurement is worth what its conditions are worth, and every way it can mean
less than it appears to is recorded rather than inferred. `icmp_error` (schema v2) holds
the ICMP type and code most often reported in the interval, packed as `type<<8 | code`,
and is NULL when nothing was refused. It exists because total loss with a stated reason
("administratively prohibited") and total loss in silence are different findings that
call for different responses; collapsing them, as the first cut did, throws that away.
A probe the kernel declines to send counts as attempted and lost — otherwise an
unreachable target renders as an empty graph rather than as a failing one.

The frontend can badge series rendered from degraded data.

### 3.5 Storage size, honestly

Per series at 1-min intervals: 525 600 rows/yr × ~55 B (blob + key + B-tree overhead)
≈ **28 MB/yr**. So:

- 100 targets, one agent: ~3 GB/yr. Trivial.
- 300 agents × 50 targets = 15 000 series: ~420 GB/yr and ~250 inserts/s. Writes are fine
  (batched inserts, §6), but a single SQLite file at that size is past its comfort zone.

Practical ceiling for the SQLite backend: **low thousands of series** (~30–60 GB/yr).
Beyond that, the `Store` interface (§6) is the migration seam — a columnar backend
(ClickHouse, or DuckDB-over-Parquet partitions) slots in without touching prober or API.
Not built now; designed for.

## 4. Inheritance and provenance

### 4.1 Resolution

Effective value of an inheritable setting = first non-NULL walking from the node up to
the root. The root is required to be complete, so resolution always terminates with a
value. The tree is small (hundreds of nodes); it is loaded into memory once, resolved
there, and invalidated on any target write. No recursive CTEs on the hot path.

Non-inheritable, therefore plain NOT NULL/nullable-with-meaning columns: `name`, `host`,
`address_family`, `title`, `notes`, `hidden`, `enabled`, `sort_order`.

`agents` is the one multi-valued inheritable setting (SmokePing's per-target `slaves`
list, same semantics: which agents measure this subtree). It is a space-separated TEXT
column rather than a join table precisely so it participates in the NULL=inherit model
and gets provenance like every other setting; the master's built-in prober simply skips
targets whose effective agent list does not include `local` (SmokePing's `nomasterpoll`,
for free).

### 4.2 API shape

The API never returns a flattened effective config. Every inheritable field is an object:

```json
{
  "id": 42, "name": "cloudflare-v4", "host": "1.1.1.1", "address_family": "v4",
  "settings": {
    "pings_per_interval": {
      "local": null,
      "effective": 20,
      "source": { "id": 7, "name": "Production", "path": "/Production" }
    },
    "interval_s": {
      "local": 30,
      "effective": 30,
      "source": "local"
    }
  }
}
```

This is exactly what the admin UI needs for "20 pings, inherited from Production
[override]". Setting `local` to null via the API reverts to inheritance.

### 4.3 Alert rules

Alert rules are rows attached to tree nodes, inheriting downward. The open question —
does a child's rules *replace* or *add to* the parent's — is **decided (2026-08-29):
replace**. The nearest ancestor that defines any rules defines the whole set for its
subtree. Two reasons, pulling the same way: it is how every other inheritable setting in
this system behaves, so there is one rule to learn rather than two; and it is what
SmokePing does, so an imported configuration keeps its meaning. Accumulation would also
raise a question with no good answer — how does a child *remove* an inherited rule?

A rule tests one metric (`loss`, `median`, `p95`, `spread`) against a threshold, and
carries its own hysteresis: `for` consecutive matching intervals to fire, `clear_for`
consecutive non-matching to clear. `p95` and `spread` exist only because the full
distribution is kept; a tool storing one RTT per interval cannot express them.

Two properties matter more than the DSL. Continuity is literal: a gap in the series
resets the streak rather than bridging intervals nobody measured. And the quality flags
of §3.4 gate evaluation — a measurement taken while our own receive queue overflowed
does not count towards a loss rule, and one taken across a clock step does not count
towards a latency rule. Alerting on those would page someone about smokeng rather than
about the network.

### 4.4 Agent assignment (revised v0.6)

`agents` names the vantage points that measure a node and its subtree. It was a
space-separated string matched by exact equality against agent names, validated
nowhere. A typo produced a target that no agent measured, with no error, no badge and no
metric — an empty graph indistinguishable from a target that is measured and answering
nothing. That is the one place smokeng hid something it knew, and it contradicts the rest
of the design.

It becomes referential:

- The TOML form is an array: `agents = ["local", "ams-01"]`. A space-separated string is
  still accepted and means the same thing, because existing configs use it.
- `local` stays reserved for the master's own prober and cannot be an agent's name.
- Every name is checked against the enrolled agents at import and on every API write. An
  unknown name is an error that names the offender and lists what does exist.
- `config import --allow-unknown-agents` downgrades that to a warning, for the case where
  the tree is applied before the agents that will serve it are enrolled. It is a flag
  rather than the default because the common case is a typo, not a bootstrap.
- A target whose effective agent set resolves to nothing measurable is a config error, and
  `smokeng_targets_unmeasured` counts any that slip through anyway.

Inheritance stays replace-not-accumulate, for the reason §4.3 gives: a node's effective
set should be readable in one place instead of reconstructed from four ancestors. No
`agents_add`/`agents_remove`. The ergonomic problem that would solve — "everything from
local, and this subtree also from ams-01" — is a UI problem, and is solved in the UI: the
field is a multi-select pre-filled with the inherited set, so adding one vantage point is
one click that writes the whole list.

## 5. Probing engine

### 5.1 Sockets

- One ICMP datagram socket per **(address family, DSCP)** pair (`SOCK_DGRAM` +
  `IPPROTO_ICMP` / `IPPROTO_ICMPV6`), not one per family: a DSCP marking is set with
  `setsockopt` on the whole socket, so two targets that want different traffic classes
  cannot share one. Unprivileged via `net.ipv4.ping_group_range` (documented in README).
  In datagram mode the kernel owns the echo ID; demux is by sequence number, with a
  random per-ping token in the payload as a validity check. DSCP is six bits and there
  are two families, so the set is bounded at 128 sockets regardless of how the tree is
  edited; sockets are opened lazily and never closed.
- Fallback: raw socket with a clear error message explaining the sysctl, and flags bit 2
  set on all measurements (§3.4).
- Sequence allocation: single 16-bit counter per socket, pending-map `seq → (target, tx
  state)`. 65 536 in-flight pings per address family is far above any realistic load.

### 5.2 Timestamping

Full kernel timestamping on Linux, both directions, from v0.1 — not the half version:

- RX: `SO_TIMESTAMPING` (`SOF_TIMESTAMPING_RX_SOFTWARE`), timestamp read from the control
  message alongside the reply.
- TX: `SOF_TIMESTAMPING_TX_SOFTWARE`, timestamp read back from the socket error queue
  (`MSG_ERRQUEUE`), correlated to the ping by the errqueue's returned packet data. This is
  the fiddliest part of the prober and is isolated in its own package with a
  userspace-clock implementation behind the same interface.
- macOS (development platform): no `SO_TIMESTAMPING`; the userspace implementation is
  used and flags bits 0+1 are set. Dev on darwin works; accuracy claims are Linux-only.
- NIC hardware timestamping (`SOF_TIMESTAMPING_RAW_HARDWARE`) is a possible later
  refinement behind the same interface; not in scope.

### 5.3 Scheduling and interval alignment

- Interval buckets are wall-clock aligned, UTC: `ts = floor(now / interval_s) * interval_s`.
  Every agent computes the same bucket timestamps independently, so with hundreds of
  agents the shared crosshair and cross-agent comparisons align by construction — no
  clock negotiation protocol needed (agents just need sane NTP, which is a documented
  requirement anyway for a latency tool).
- Per-target phase offset within the bucket, derived deterministically from
  `hash(target_id)`: burst mode starts its burst at `bucket_start + offset`; spread mode
  shifts its evenly-spaced train by a sub-spacing offset. This prevents 100 targets
  from firing synchronized bursts while keeping recorded `ts` globally aligned.
- A bucket is finalized and written at `bucket_end + timeout_ms` (late replies within the
  timeout still count; after finalization a reply is dropped and counted in a metrics
  counter, never mutated into a written row).

### 5.4 DNS

- Hostname targets are resolved with direct DNS queries (miekg/dns against the system
  resolver) because `net.Resolver` does not expose TTLs.
- The resolved address is cached and re-resolved when its TTL expires (clamped to
  [30 s, 24 h]), asynchronously — the ping loop never blocks on DNS; it keeps using the
  previous address until a new resolution succeeds.
- On address change: log line + row in `resolutions` (§3.1). This makes anycast/failover
  flips visible in the UI later without storing anything per measurement.
- `address_family` strictly selects A vs AAAA. Never "whatever resolves".

## 6. Storage engine

**SQLite, and it is genuinely sufficient — with numbers.** The write load is hundreds of
rows *per minute* single-host, and even the hundreds-of-agents scenario is ~250 rows/s,
which batched transactions handle with an order of magnitude of headroom. The concern
"is SQLite suited for this many points" conflates row count with write rate: the row
count only costs disk (§3.5), and the query pattern (clustered range scan per series) is
exactly what a B-tree is good at. Time-series databases earn their complexity through
compression, downsampling and high-cardinality ingest — the first two are things this
project *refuses to do on principle* (downsampling) or already does better in the
application layer (the delta-varint blob is domain-specific compression a generic TSDB
cannot match), and the third is not our load profile.

Alternatives considered and rejected for now:

- **VictoriaMetrics / Prometheus-family** — one float per sample; the distribution dies
  at write time. Disqualified by the premise, not by taste.
- **ClickHouse / TimescaleDB** — a second server process; kills the single-binary
  deployment. Correct future backend at >1000s of series, reachable via `Store`.
- **DuckDB** — embedded and columnar, but optimized for analytical reads over bulk data,
  weaker at continuous small-batch ingest, and drags in CGO.

Concrete SQLite setup:

- WAL mode; `synchronous=NORMAL`; single dedicated writer goroutine; inserts batched in
  one transaction per flush tick (~1 s) — readers are never blocked (WAL).
- Ingest is idempotent: `INSERT OR REPLACE` keyed on the PK, so replayed agent batches
  (§9) and prober restarts are no-ops.
- Driver: `modernc.org/sqlite` (pure Go) so the single static binary cross-compiles
  without CGO. Escape hatch: if batch-insert or range-scan throughput measures >2× worse
  than mattn/go-sqlite3 in a real benchmark, swap drivers behind `database/sql` — no
  schema or code impact.
- Backup story for ops: the WAL setup is Litestream-compatible out of the box; documented
  in the README, not built in.

The `Store` interface stays narrow and concrete — roughly: write measurement batch, read
series range, target CRUD, agent CRUD. No speculative abstraction beyond what the API
and prober actually call.

## 7. HTTP API

### 7.1 Endpoints (v0.1)

```
GET  /api/v1/targets                            tree incl. provenance objects (§4.2)
GET  /api/v1/measurements?target_id=&agent_id=&from=&to=
                                                Arrow IPC stream (§7.2)
GET  /healthz
GET  /metrics                                   Prometheus metrics about smokeng itself
POST /api/v1/ingest                             v0.4, signed (§9); 404 until then
```

Target CRUD (POST/PATCH/DELETE) arrives with the admin UI in v0.2; until then the tree
is mutated via TOML import. `/metrics` exposes operational health only (write latency,
socket errors, pending pings, DNS failures, per-agent last_seen) — measurement data never
goes through Prometheus, by design.

One request per target series; the frontend fetches stacked plots in parallel. A batched
multi-series endpoint is a later optimization if HTTP overhead ever measures as real.

v0.1 ships without auth: default listen `127.0.0.1:8080`; binding non-loopback without
`--i-know-this-is-unauthenticated` refuses to start. OIDC lands in v0.3 (coreos/go-oidc,
session cookie, viewer/admin from a configurable claim).

### 7.2 Arrow schema

The server decodes sample blobs and serves real Arrow — the varint encoding is a storage
detail that never crosses the API boundary:

```
ts          Timestamp(second, UTC)
sent        UInt16
received    UInt16
flags       UInt8
samples     List<UInt32>        -- RTTs in µs, sorted ascending
icmp_error  UInt16, nullable    -- type<<8|code; null when nothing was refused (§3.4)
```

Content type `application/vnd.apache.arrow.stream`, record batches of ~8k rows streamed
as they are read. A month of 1-min data is ~43k rows / ~900k sample values ≈ 4–5 MB on
the wire, landing in typed arrays browser-side with no parse step. Decode cost
server-side is a linear varint walk — negligible against I/O.

### 7.3 Config as TOML, DB as source of truth

The DB is authoritative from v0.1. TOML exists for GitOps and bootstrapping:

```
smokeng config export > targets.toml
smokeng config import targets.toml [--prune]
```

Tree representation: path-keyed tables (TOML nests poorly, paths do not):

```toml
[targets."Production"]
interval_s = 30                      # group node: settings cascade down

[targets."Production/DNS/cloudflare-v4"]
host = "1.1.1.1"
address_family = "v4"
pings_per_interval = 40              # local override
```

Import is a declarative sync: upsert by path; targets present in DB but absent from the
file are **disabled**, not deleted (measurement history is never silently destroyed);
`--prune` deletes them explicitly. Unset keys in the file mean "inherit" (NULL), matching
§4. Export writes only local values, so export→import round-trips exactly.

The SmokePing `Targets` importer (`+`/`++`/`+++` hierarchy → this tree) is v0.2, alongside
the admin UI. Import detail: a SmokePing host entry with a literal IP maps to that
family; a hostname maps to a v4 target, `--also-ipv6` duplicates to v6.

### 7.4 Scoped authorisation (v0.3)

Until v0.3 there were two roles, `viewer` and `admin`, and both were global: a viewer read
everything, an admin wrote everything. That is right for one team and wrong the moment a
customer should see their own targets and nothing else.

**Grants.** A grant is (OIDC group, target node, role). It applies to that node and its
whole subtree, the same way every other setting on this tree inherits. Roles within a
grant are `viewer` (read) and `editor` (read plus write inside the subtree). The global
`admin` role, from the existing admin claim, is unchanged and unscoped.

Grants live in smokeng's own table, are edited in the UI, and round-trip through TOML like
the target tree. The identity provider supplies group membership and nothing else. The
alternative — paths carried in an ID-token claim — was rejected: it puts authorisation in
a second place, in a provider-specific format, where it cannot be read next to the tree it
applies to, which is exactly the question one does not want to be answering during an
incident.

Grants are keyed on group, never on an individual. A single person is a group of one in
the provider. One concept.

**Isolation is total.** A scoped user's subtree is presented as though it were the whole
installation: the granted node is their root, paths are rendered relative to it, and
nothing above or beside it exists in any response. This is the requirement — a
municipality must not learn that other municipalities are customers — and it is what makes
the read side, not the write side, the hard part.

Two consequences fall out of it:

- **Provenance stops at the boundary.** The API reports every setting as
  `{local, effective, source}`, and `source` names the ancestor a value came from. Above a
  user's root that ancestor is a path they may not know. Such a value is reported with
  `source: "outside"` — the effective value, honestly labelled, with no path. Suppressing
  the value instead would be worse: they would see a number they cannot explain.
- **Agents are global infrastructure, and are not scoped by grants.** A scoped user sees
  the names and liveness of the agents that measure targets inside their scope, because
  otherwise "from ams-01" on their own graph is unreadable. They see no public keys, no
  enrolment tokens, and cannot enrol, rename, disable or remove anything. Those are
  global-admin actions.

**What stays global admin only:** agents and enrolment tokens, the root defaults,
`/metrics` (it counts and names things across the whole installation), and `config
import`/`export`, which are declarative over the entire tree and cannot express a partial
apply.

**Writes.** An editor may create, edit and delete within their subtree. Creation and moves
check *both* endpoints: the node's current parent and its proposed parent must each be
inside the scope, or a move becomes a way to smuggle a target across a boundary.

**Enforcement is at the API boundary**, not in the store. Sessions exist there and nowhere
else, and threading a scope through every query would spread the check across every layer
that can read a row. The risk this accepts is the obvious one — a new endpoint that
forgets to filter is a leak, not a bug — so it is paid for with a test that enumerates the
routes the mux actually has and fails when one is neither explicitly scoped nor explicitly
declared global. Forgetting becomes a red build rather than a disclosure.

**Migration.** Adding the first grant must not silently lock out everyone who could
already read. What an authenticated user with no grant gets is therefore an explicit
setting, `--default-role`, which is `viewer` — today's behaviour — until an operator
chooses `none`. A security change that happens as a side effect of unrelated
configuration is a change nobody reviewed.

## 8. Frontend and rendering pipeline

### 8.1 Stack

React + TypeScript + Vite, Arrow IPC reader library per §11, custom canvas rendering
(no chart library — the density renderer *is* the product). React is chosen for
maintainability of the later admin UI (forms, tree editing, OIDC flows have mature
ecosystem support), not for the plot itself, which is framework-agnostic canvas code.
State stays at React context + hooks until something measurably hurts. `npm run build`
output is embedded via `embed`; Node is a build-time dependency only.

### 8.2 Rendering pipeline

Per plot: one density canvas (transferred once to a Web Worker as `OffscreenCanvas`),
plus a DOM/SVG overlay on the main thread for axes, crosshair and brush — so interaction
never waits on a render.

Worker render pass, per pixel column:

1. **Pool** all samples of every interval overlapping the column's time span. Sorted
   per-interval runs make the pooled median a cheap k-th selection merge.
2. **Deposit** each sample as an impulse into a `Float32Array` column at its y pixel
   position (linear interpolation across the two neighboring pixels), after the y
   transform — linear or log10. Binning in *pixel* space is what makes the log axis free.
3. **Blur** the column once with a 1-D Gaussian (σ ≈ 1–2 px). Convolution is linear, so
   impulses-then-blur is mathematically identical to per-sample kernels but
   O(samples + height·kernel) instead of O(samples·kernel) — the difference dominates
   when zoomed out and a column pools thousands of samples.
4. **Map density → alpha** and write RGBA into the plot-wide `ImageData`; single
   `putImageData` for the whole plot at the end.
5. **Median line** drawn on top as a crisp stroke, from the *pooled* per-column median —
   never a median-of-medians; we have every sample and that is the point of the tool.

**Gaps are rendered as gaps.** A column whose time span contains no finalized bucket
draws nothing and breaks the median line; density and median are never interpolated
across missing data (scheduler downtime, disabled periods). RRDtool gets this right with
its UNKNOWN handling and a reimplementation that connects lines across outages would be
quietly lying.

**Variable settings over time are a feature, not a corruption case.** SmokePing fixes
`step` and `pings` into the RRD at creation — changing either means deleting history.
Here every row is self-describing (`ts`, `sent`, blob), so the renderer must assume
neither a constant interval nor a constant sample count within a range; pooling per
column and count-aware alpha normalization (§8.2) already provide that. Changing a
target's interval or ping count preserves all history by construction.

Density→alpha mapping was the one aesthetic unknown; it is decided and there is no
runtime toggle between the two options (§13):

- (a) per-column max normalization — smoke equally visible everywhere, columns not
  comparable over time. Not built.
- (b) sample-count normalization with a fixed curve `alpha = 1 − exp(−k·density)`,
  `k = 14` — comparable over time. **Shipped**, the only mode the renderer implements
  (`web/src/render.worker.ts`).

Budget check: 1600×400 px, month view ≈ 900k samples → deposit ~1M ops + blur ~6M ops,
well under a frame in a worker. Measure before reaching for WASM; the expectation is
that WASM is never needed.

### 8.3 Loss rail

Separate rail (~12 px) directly under each plot: per-column mean loss → viridis, with 0%
rendered as background (so the rail is visually silent when nothing is wrong). Loss never
touches the median line's color channel. Hover shows exact sent/received via crosshair.

### 8.4 Interaction (all v0.1)

- **Shared crosshair** across stacked plots: one time cursor in a tiny pub/sub store;
  each plot draws its own overlay line + value readout. O(1) per mousemove (binary
  search into the column index), zero density re-renders.
- **Brush-zoom** on the overlay; on commit: refetch `[from, to)` at full resolution and
  re-render. There is no in-worker cache of previously fetched ranges — every zoom and
  pan issues a fresh `fetch(..., { cache: 'no-store' })`, including zooming back out to
  a range already seen. No LOD tiers, no server aggregation — full resolution at every
  zoom level, by premise. Simple over clever until refetching measurably hurts.
- **Log y-axis** toggle per plot group (falls out of step 2 above).
- Free time ranges; no fixed day/week/month presets as the *mechanism* (quick-select
  buttons may exist as UI sugar over the free range).

## 9. Agent protocol (designed now, implemented v0.4)

Push model, Ed25519 per-agent keypair, exactly as the kickoff brief, with two hardening
changes:

**Signing input** (domain-separated, canonical):

```
smokeng-ingest-v1\n
<METHOD>\n
<PATH>\n
<agent_id>\n
<timestamp>\n
<nonce>\n
<hex(sha256(body))>
```

METHOD and PATH are added so a captured signature can never be replayed against a
different future endpoint. Headers: `X-Agent-Id`, `X-Timestamp` (unix seconds),
`X-Nonce` (16 random bytes, base64), `X-Signature` (base64).

**Validation order** (reject with one generic error, log the real reason + agent id):
agent exists and enabled → `|now − ts| ≤ 300 s` → nonce unseen (in-memory, TTL 600 s)
→ signature over the canonical string rebuilt from received headers + actual body hash
→ every measurement in the batch belongs to a target assigned to this agent.

**Idempotency is the real replay defense.** The nonce cache is in-memory and empties on
master restart; the timestamp window alone would then admit replays. Because ingest
upserts on `(target_id, agent_id, ts)` (§6), a replayed batch is a byte-identical no-op.
The nonce cache stays (cheap, blocks log spam), but correctness does not depend on it.

Body: the same Arrow IPC schema as §7.2 plus a `target_id` column — one serializer,
one decoder, no second wire format. TLS required; `--insecure-allow-http` for local dev
only. Rate limit per agent id, cap body size, manual enrolment
(`smokeng agent add --name ams --pubkey <base64>`). Deliberately no mTLS. Agents buffer
locally (same SQLite store, same schema) while the master is unreachable and drain on
reconnect — offline tolerance falls out of reusing the store.

**Target assignment (decided, see §13 #9).** The kickoff brief bans "config
distribution where probes fetch their configuration from the master", but target
assignments live in the master's database (`agents` setting, §3.1) and the stated goal
is hundreds of agents managed from an admin dashboard. Without any distribution, every
assignment change means hand-editing config on every agent — that does not survive
contact with 100 agents. Note that SmokePing's version of this feature is genuinely
horrifying — slaves have no local config at all and *eval Perl code sent by the master*
(remote code execution as a design pattern) — and is presumably what the ban is aimed at.
The proposed narrow version shares none of that: agents poll
`GET /api/v1/agent/targets` (authenticated with the same Ed25519 request signing) and
receive pure data — their assigned targets with resolved effective settings, nothing
else, never code, pull-only, master remains the single source of truth. Decision
(2026-08-29): exactly this is allowed; the ban stands for everything beyond it.

## 9a. Path correlation (v0.5)

Designed 2026-08-29; the roadmap named it, nothing specified it.

**The question this answers.** When the smoke changes shape — latency steps up, the
distribution goes bimodal, loss appears — did the path change? That is the one question
worth asking of a traceroute in a latency tool, and it is one SmokePing cannot ask at
all. Everything else a traceroute could offer is a different product.

**Scope, stated as refusals.** No standalone traceroute view, no per-hop latency graphs,
no path-quality scoring, no MTR. A route is recorded, its changes are marked on the
timeline, and the smoke is left to speak for itself.

**How.** TTL-limited echo requests on a dedicated short-lived socket, with the hop
address read from the ICMP time-exceeded reply. This is the machinery already built for
§3.4's ICMP errors: `IP_RECVERR` puts the error on the socket error queue together with
the offending datagram, whose sequence number identifies which probe it answers. A
separate socket keeps `IP_TTL` off the measurement socket, where it would corrupt every
other target sharing it.

Where the error queue is unavailable the tracer reports no path rather than a wrong one,
and the absence is visible — the same rule the timestamping fallback follows.

**Storage: changes only.** Paths are stable for days and then are not. Recording every
run would store the same list thousands of times over, so a row is written only when the
path differs from the last one, exactly as `resolutions` already does for DNS:

```sql
paths(target_id, agent_id, ts, hops TEXT)   -- schema v6
```

`hops` is the comma-separated hop list, `*` for a hop that did not answer. Text, because
it is diffed far more often than parsed, and being readable in a `sqlite3` session is
worth more here than bytes.

**Frequency.** A separate inheritable `trace_interval_s` (0 disables), defaulting to
something in the minutes: a traceroute costs a round trip per hop, and a path that
changes between two runs is caught by the next one either way. It is emphatically not
per measurement interval.

**Rendering.** Path changes are vertical marks on the time axis and the path is named in
the crosshair readout. That places "the path changed at 14:02" next to "the smoke
widened at 14:03" without needing a second view, which is the whole point.

## 9b. Enrolment tokens (v0.6)

Enrolling an agent meant copying an Ed25519 public key from the agent host to the master
by hand and running a CLI command there. That is fine for two agents and hostile for
twenty, and it cannot be done from the UI at all.

A token flow replaces it, without weakening what the keypair guarantees:

1. An admin mints a token **for a chosen name**: `POST /api/v1/agent-tokens {name}`. The
   master returns the plaintext once and stores only `sha256(token)`. The token is
   `smk_` + 32 random bytes, base64url — recognisable in a log or a secret scanner.
2. On the agent: `smokeng agent run --master https://… --token smk_…`. It generates its
   keypair if absent and calls `POST /api/v1/agent/enrol {token, pubkey}`.
3. The master verifies the hash, creates the agent under the name the token carries, marks
   the token spent in the same transaction, and returns the agent id. The agent persists
   the id and proceeds exactly as before.

Decisions worth stating, because they are not the only defensible ones:

- **The token carries the name, the agent does not choose it.** An agent that named itself
  could claim a name a target is already assigned to. Naming is an administrative act.
- **Single use, and short-lived** (default one hour, set at mint time). A reusable
  enrolment token is precisely the credential that ends up in a wiki. Provisioning twenty
  agents means minting twenty tokens through the API, which a loop does fine.
- **Spending is atomic with agent creation.** A token that half-enrolled an agent would be
  worse than no token.
- **A name collision rejects and does not spend the token**, so the admin can retry with a
  different name rather than mint again.
- The endpoint is unauthenticated by design — the token *is* the authentication — so it is
  the one place a plain-HTTP master leaks a usable credential. The agent already refuses a
  non-HTTPS master without `--insecure-allow-http`; that refusal now matters more than it
  did, and the flag's help says so.

Nothing about the steady-state protocol changes: after enrolment an agent is the same
signed-request client §9 describes, and the paste-the-public-key path (`smokeng agent add`)
stays for anyone who would rather not have a token in flight at all.

## 10. Package layout

```
cmd/smokeng/            main; subcommands: serve, config, agent, version
internal/
  tree/                 target tree, inheritance resolution, provenance
  probe/                scheduler, ICMP engine, seq/pending tracking
  probe/timestamp/      SO_TIMESTAMPING (linux) + userspace fallback, one interface
  probe/dnscache/       TTL-respecting resolver (miekg/dns)
  probe/trace/          TTL-limited path discovery, ICMP time-exceeded on the error queue
  store/                Store interface + SQLite implementation, migrations
  store/enc/            samples blob codec (encode/decode, versioned)
  api/                  HTTP handlers, Arrow serialization
  auth/                 OIDC login flow, session cookies
  alert/                rule evaluation, hysteresis, webhook notification
  agent/                remote agent mode: pull assignments, probe, push measurements
  metrics/              /metrics Prometheus exposition (smokeng's own health, never data)
  config/               TOML import/export; smokeping importer
  ingest/               signed ingest: canonical string, verification
web/                    Vite + React app; dist/ embedded via embed
```

`store/enc` and `probe/timestamp` are deliberately tiny leaf packages: they are the two
places with subtle correctness requirements, and they get focused unit tests (golden
blobs; a fake clock/socket).

## 11. Toolchain & dependencies

Verified against upstream on 2026-08-28 (not from memory):

**Go side** — Go 1.27 (2026-08). Dependencies, all confirmed actively maintained:

| Dependency | Version | Note |
|---|---|---|
| `golang.org/x/net` | v0.58.0 | `icmp` package present and maintained; ≥ v0.55 for security fixes |
| `golang.org/x/sys` | v0.50.x | `SO_TIMESTAMPING` / `MSG_ERRQUEUE` constants and cmsg parsing |
| `codeberg.org/miekg/dns` | v2 | **moved off GitHub** — the github.com v1 module is unsupported; import the Codeberg v2 path |
| `modernc.org/sqlite` | v1.44.x | embeds SQLite 3.51.x; pure Go, no CGO |
| `github.com/apache/arrow-go/v18` | v18.5.0 | correct current module path (moved out of the apache/arrow monorepo) |
| `github.com/pelletier/go-toml/v2` | v2.2.x | BurntSushi/toml is stale (no release since 2018) — do not use |
| `github.com/coreos/go-oidc/v3` | v3.18 | v0.3 timeframe; re-verify then (zitadel/oidc is the alternative if requirements grow) |

Escape hatch `github.com/mattn/go-sqlite3` (v1.14.x) is alive but slow-cadence and needs
CGO — only if the modernc benchmark gate (§6) trips. Litestream (v0.5.x, Fly.io) is
actively maintained, so the backup story in §6 holds.

**Frontend** — React 19.2 · Vite 8 (Rolldown) · TypeScript 7 · `@uwdata/flechette` 2.5
as the Arrow IPC reader (maintained, read-only, a fraction of `apache-arrow`'s bundle —
we only read; trade-off #4 is thereby resolved). Tailwind 4 enters with the admin UI in
v0.2, not before.

**Browser support constraint**: `OffscreenCanvas` + 2D in workers is fine everywhere
(Safari ≥ 16.4, Firefox ≥ 105), but Firefox still does not support *module* workers.
Non-issue in practice — Vite bundles worker code into a single classic-compatible file
(`worker.format: 'iife'`) — but it is a hard constraint: no `type: "module"` workers at
runtime, enforced by config, documented here so nobody "modernizes" it into a Firefox
breakage.

## 12. Roadmap — where this stands

The original two-weekend v0.1 grew by explicit choice (config in DB from day one, both
probe modes, full TX+RX timestamping, full interaction set), and the project has kept
growing since. Releases through **v0.4.0** have shipped (`git tag --list`); the sections
below describe what each cut delivered, and what has landed on top of it.

- **v0.1 — core**: prober (burst + spread, full kernel timestamping with observable
  fallback), SQLite store, targets in DB + TOML import/export, Arrow API, frontend with
  density smoke, median, loss rail, stacked plots, shared crosshair, brush-zoom, log axis.
- **v0.2 — admin**: target tree editor with provenance/override UI, target CRUD API,
  SmokePing `Targets` importer, referential agent assignment (§4.4), and manual agent
  enrolment (`smokeng agent add`).
- **v0.3 — access & alerting**: OIDC (viewer/admin), scoped authorisation — grants,
  viewer/editor roles on a subtree, total isolation (§7.4) — and webhook alerting
  (Alertmanager-compatible payload, edge-triggered with hysteresis per §4.3). Full
  distributions make rules on percentiles/spread possible, which SmokePing cannot do.
- **v0.4 — distributed, and the current UI**: `smokeng agent`, signed ingest, per-agent
  series; one-time enrolment tokens (§9b) alongside manual enrolment; a persisted alert
  transition log (`GET /api/v1/alert-events`) on top of the live firing state; and a
  reworked frontend — overview and per-target detail screens, a command palette, TOML
  import/export from the UI — replacing the original v0.1 layout.
- **Path correlation** (§9a): TTL-limited traceroute, recorded on change and marked on
  the time axis, has also shipped.

What is not built: additional latency probe types beyond ICMP (§3.2 designs the seam;
TCP-connect is not implemented), and NIC hardware timestamping (§5.2). Unchanged from
the original brief: no non-latency checks or status probes, no notification matrix, no
user management, no RRD import, no config distribution beyond the narrow agent-target
pull (§9), no multi-tenancy.

## 13. Open trade-offs

| # | Decision | Options | Recommendation |
|---|----------|---------|----------------|
| 1 | "probe" naming | brief's probe-as-node vs probe-mode + **agent** | agent (§2) — rename is free now, breaking later |
| 2 | Density→alpha default | per-column max vs count-normalized `1−exp(−k·d)` | **decided**: count-normalized, `k = 14`, the only mode built — no runtime toggle |
| 3 | SQLite driver | modernc.org/sqlite (pure Go) vs mattn (CGO) | modernc; benchmark escape hatch (§6) |
| 4 | Arrow JS reader | apache-arrow (large) vs flechette (small, read-only) | **resolved**: flechette 2.5 — verified maintained (§11); we only read |
| 5 | Sample unit | 1 µs vs 1 ns | µs — below timestamping noise; version byte keeps ns possible |
| 6 | Probe modes modeling | inheritable `probe_mode` vs `probe_defs` table | inheritable column; `probe_type` and its per-type settings landed 2026-08-30 as the predicted additive migration, with no change to `measurements` (§3.2b) |
| 7 | Multi-series endpoint | per-target requests vs batched | per-target now; batch only if measured overhead |
| 8 | Alert rule inheritance | child replaces vs child adds | **decided 2026-08-29**: replace, consistently with every other setting and with SmokePing (§4.3) |
| 9 | Agent target assignment | hand-configured per agent vs narrow pull of assigned targets from master | **agreed 2026-08-29**: pull (data-only, signed, pull-only — see §9) |
| 10 | Alternative hierarchies | SmokePing `parents`/multi-hierarchy vs single-parent tree | single-parent only — inheritance and provenance hang on the tree; alternative *views* (tags/saved selections) can come later as presentation, never as a second parent axis |
