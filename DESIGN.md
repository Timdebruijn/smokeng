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

**Further probe types are planned** (decided 2026-08-29, revising the kickoff brief's
"no probe buffet" ban): TCP-connect first, more later. The extension seam is already in
place — `measurements` is protocol-agnostic (sent / received / sorted RTT samples), and
adding an inheritable `probe_type` setting (default `icmp`) plus per-type settings is a
plain additive schema migration when the second protocol lands; see §13 #6. The
guardrail that replaces the ban: every probe type must produce an RTT distribution of N
samples per interval. A check that yields a single scalar or an up/down status does not
belong here, ever — that is the road to a generic uptime dashboard.

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

## 5. Probing engine

### 5.1 Sockets

- One ICMP datagram socket per address family (`SOCK_DGRAM` + `IPPROTO_ICMP` /
  `IPPROTO_ICMPV6`). Unprivileged via `net.ipv4.ping_group_range` (documented in README).
  In datagram mode the kernel owns the echo ID; demux is by sequence number, with a
  random per-ping token in the payload as a validity check.
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
ts        Timestamp(second, UTC)
sent      UInt16
received  UInt16
flags     UInt8
samples   List<UInt32>        -- RTTs in µs, sorted ascending
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

Density→alpha mapping is the one aesthetic unknown; both modes ship behind a dev toggle
and the default is chosen by eye (§13):

- (a) per-column max normalization — smoke equally visible everywhere, columns not
  comparable over time;
- (b) sample-count normalization with a fixed curve `alpha = 1 − exp(−k·density)` —
  comparable over time, k needs tuning. Recommended default.

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
  re-render. Debounced; an in-worker cache of already-fetched ranges avoids refetching
  on zoom-out. No LOD tiers, no server aggregation — full resolution at every zoom
  level, by premise.
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

## 10. Package layout

```
cmd/smokeng/            main; subcommands: serve, config, agent (v0.4)
internal/
  tree/                 target tree, inheritance resolution, provenance
  probe/                scheduler, ICMP engine, seq/pending tracking
  probe/timestamp/      SO_TIMESTAMPING (linux) + userspace fallback, one interface
  probe/dnscache/       TTL-respecting resolver (miekg/dns)
  store/                Store interface + SQLite implementation, migrations
  store/enc/            samples blob codec (encode/decode, versioned)
  api/                  HTTP handlers, Arrow serialization
  config/               TOML import/export; smokeping importer (v0.2)
  ingest/               signed ingest: canonical string, verification (v0.4)
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

## 12. Roadmap and the v0.1 cut line

The original two-weekend v0.1 has grown by explicit choice (config in DB from day one,
both probe modes, full TX+RX timestamping, full interaction set). Honest estimate:
**three to four weekends**. If it must shrink, the cut line inside v0.1 is, in order:
shared crosshair → log axis → spread mode — each is additive and none changes the schema
or wire format. Nothing else in v0.1 is cuttable without violating the premise.

- **v0.1 — core**: prober (burst + spread, full kernel timestamping with observable
  fallback), SQLite store, targets in DB + TOML import/export, Arrow API, frontend with
  density smoke, median, loss rail, stacked plots, shared crosshair, brush-zoom, log axis.
- **v0.2 — admin**: target tree editor with provenance/override UI, target CRUD API,
  SmokePing `Targets` importer.
- **v0.3 — access & alerting**: OIDC (viewer/admin), webhook alerting
  (Alertmanager-compatible payload), alert-rule inheritance decision (§4.3). Scope note:
  rules must be edge-triggered and evaluated over a window of consecutive intervals with
  hold-down/hysteresis — SmokePing's alert patterns and matchers are the reference bar; a
  naive instantaneous threshold is explicitly not enough and would flap on every blip.
  Having full distributions also allows rules on percentiles/spread, which SmokePing
  cannot do; design then, not now.
- **v0.4 — distributed**: `smokeng agent`, signed ingest, enrolment, per-agent series in UI.
- **v0.5 — traceroute correlation.**

Explicit non-goals, updated 2026-08-29: additional *latency* probe types are now planned
(§3.2) and the narrow agent-assignment pull is allowed (§9); unchanged from the brief
remain — no non-latency checks or status probes, no notification matrix, no user
management, no RRD import, no config distribution beyond §9, no multi-tenancy.

## 13. Open trade-offs

| # | Decision | Options | Recommendation |
|---|----------|---------|----------------|
| 1 | "probe" naming | brief's probe-as-node vs probe-mode + **agent** | agent (§2) — rename is free now, breaking later |
| 2 | Density→alpha default | per-column max vs count-normalized `1−exp(−k·d)` | count-normalized, dev toggle ships, decide by eye |
| 3 | SQLite driver | modernc.org/sqlite (pure Go) vs mattn (CGO) | modernc; benchmark escape hatch (§6) |
| 4 | Arrow JS reader | apache-arrow (large) vs flechette (small, read-only) | **resolved**: flechette 2.5 — verified maintained (§11); we only read |
| 5 | Sample unit | 1 µs vs 1 ns | µs — below timestamping noise; version byte keeps ns possible |
| 6 | Probe modes modeling | inheritable `probe_mode` vs `probe_defs` table | inheritable column now; an inheritable `probe_type` (+ per-type settings) is an additive migration when TCP-connect lands — expected, per the 2026-08-29 decision to allow more latency probes (§3.2) |
| 7 | Multi-series endpoint | per-target requests vs batched | per-target now; batch only if measured overhead |
| 8 | Alert rule inheritance | child replaces vs child adds | **decided 2026-08-29**: replace, consistently with every other setting and with SmokePing (§4.3) |
| 9 | Agent target assignment | hand-configured per agent vs narrow pull of assigned targets from master | **agreed 2026-08-29**: pull (data-only, signed, pull-only — see §9) |
| 10 | Alternative hierarchies | SmokePing `parents`/multi-hierarchy vs single-parent tree | single-parent only — inheritance and provenance hang on the tree; alternative *views* (tags/saved selections) can come later as presentation, never as a second parent axis |
