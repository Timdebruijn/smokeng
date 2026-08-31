# Operations

## Running as a service

smokeng is one binary with no runtime dependencies. A systemd unit:

```ini
[Unit]
Description=smokeng
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=smokeng
Group=smokeng
ExecStart=/usr/local/bin/smokeng serve \
  --db /var/lib/smokeng/smokeng.db \
  --listen 127.0.0.1:8080 \
  --external-url https://smokeng.example.org
Restart=always
RestartSec=5
StateDirectory=smokeng
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true

[Install]
WantedBy=multi-user.target
```

The service user needs to be in a group covered by `net.ipv4.ping_group_range` — see
[Getting started](getting-started.md). It does **not** need root, and `NoNewPrivileges`
above is compatible with unprivileged ICMP; it is not compatible with the
`CAP_NET_RAW` fallback, so if you rely on `setcap` instead of the sysctl, drop that line
and add `AmbientCapabilities=CAP_NET_RAW`.

Put a TLS-terminating reverse proxy in front of it if it is reachable by anyone but you,
and configure [authentication](authentication.md).

### Behind a proxy

`--external-url` is the address everyone else reaches it at. smokeng cannot work that out
for itself — it only sees the address it binds to — and it needs it for two things: the
enrolment command shown in the UI has to name somewhere an agent can actually connect,
and it becomes the default base for the OIDC redirect. Leave it unset only when the
listen address really is the address people use.

It also decides whether the session cookie carries `Secure`. That used to follow the
listen address, which gets it wrong in the most ordinary arrangement there is: a proxy
terminating TLS on the same host leaves smokeng on loopback while the browser is on
https, and the cookie went out without `Secure` — one downgrade away from travelling in
the clear.

`--trusted-proxies` takes a comma-separated list of CIDRs whose `X-Forwarded-For` may be
believed, so log lines name the real client instead of the proxy:

```
--trusted-proxies 10.0.0.0/8,192.168.1.1
```

The header is read right to left, and the first hop that is not on the list is the
answer — everything before it was written by someone with no reason to be believed. It
affects logging and nothing else: smokeng authorises agents by signature and browsers by
session cookie, never by address, so a forged header cannot get past anything. It can
only put a lie in your logs, which is precisely what this stops.

## Running the prober as its own process

By default one process does everything: scheduler, probing engine, database, API and web
UI. That is the right shape for one host, and it is what `smokeng serve` gives you.

It does not have to be. `smokeng agent run` is the probing engine without the UI, and
nothing about it requires the far end to be far away — pointed at loopback it is a
separate prober on the same machine as the master. Reasons to want that:

- **the tail of the distribution gets cleaner** for the userspace-timed probe types —
  measured, see below;
- restart or upgrade the UI without interrupting measurement, and the reverse;
- keep the unprivileged-ICMP permission on the half that needs it, off the half that
  serves a web page;
- contain a prober that is misbehaving without taking the API down with it.

### What it does to accuracy

Measured, not assumed. Two-core Debian VM, loopback targets so the network contributes
almost nothing, the machine under heavy load from the instance's own API in the loaded
column, and the load still running when collection stopped:

| spread within an interval | idle | prober in the master process, under load |
| --- | --- | --- |
| `icmp` (kernel-stamped) | 8 µs | **8 µs** |
| `dns` (kernel-stamped) | 46 µs | **41 µs** |
| `tcp` (userspace-timed) | 139 µs | **1764 µs** |

That is the whole story in one table. **The kernel-stamped types do not move**, because
their timestamps are taken as the packet crosses the wire and no amount of userspace work
can shift them. `tcp` widens thirteenfold and its p99 goes from 277 µs to 4484 µs, and none
of that is the network — it is GC and scheduler contention inside your own prober, drawn as
smoke.

So:

- **Only `icmp` or `dns` targets?** Change nothing. One process is correct and the prober
  is already immune.
- **`tcp`, `http`, `https` or `irtt` targets, on an instance whose UI gets real traffic?**
  Run the prober as its own process. There is no way to fix those types at the source: a
  TCP or TLS handshake completes inside the kernel and userspace only observes the call
  returning, so there is no packet of ours left to stamp.

Two caveats worth carrying. The figures are one machine and a deliberately hard load, so
they say the effect is real and which direction it goes, not what it will be on yours. And
an earlier version of this experiment left one arm on an idle machine and measured the
opposite sign, because CPU frequency scaling swamped everything — if you repeat it, keep
the machine equally busy in both arms and keep the load running until collection ends.

## Storage growth

smokeng keeps every measurement at full resolution forever. That is the whole point, and
it costs disk in a way that a fixed-size RRD file does not. The arithmetic is honest and
simple:

- One series at a 60-second interval is ~525 600 rows/year at roughly 55 bytes per row
  (encoded samples plus key and B-tree overhead) — about **28 MB per series per year**.
- 100 targets from one vantage point: ~3 GB/year.
- The same 100 targets from three agents: 300 series, ~8 GB/year.

Note that the cost is per *series*, and a series is a (target, agent) pair. Ping count
barely moves it — samples are stored as sorted deltas in microseconds, so 20 pings and 60
pings differ by a handful of bytes, not by a factor of three. Halving `interval_s` doubles
it.

The SQLite backend is comfortable into the **low thousands of series** (roughly 30–60
GB/year). Past that a single SQLite file is the wrong shape, and the `Store` interface is
the seam where a columnar backend would go. That work is designed but not built; if you
are planning fifteen thousand series, say so before you start.

Retention is opt-in and per target. `retention_s` is `0` — keep forever — by default, so
nothing is deleted unless you ask for it. Set it on a node (it inherits like every other
setting, so a group sets the policy for its subtree) and measurements older than that many
seconds are pruned on a timer, `--retention-interval` (default one hour). The prune runs in
time slices so clearing a long backlog is a series of short write locks rather than one
that stalls the prober.

What retention never does is coarsen. An old interval is deleted whole, not averaged into a
summary, so history before the horizon reads as absent — never as a downsampled figure,
which is the one thing smokeng exists not to fake. Downsampling stays unimplemented and
unplanned for that reason.

## Backups

The database is a single SQLite file in WAL mode. Do not copy it with `cp` while smokeng
is running; use SQLite's own online backup:

```bash
sqlite3 /var/lib/smokeng/smokeng.db ".backup '/var/backups/smokeng-$(date +%F).db'"
```

Keep your `targets.toml` in Git as well. Between the TOML and the database you can rebuild
either one: `config export` regenerates the file from the database, and `config import`
rebuilds the tree from the file.

Agent hosts hold `probe.key`. Losing one is not a disaster — generate a new key and
re-enrol the agent — but the buffered measurements in the agent's local database are lost
if you delete that too.

## Monitoring smokeng itself

`GET /metrics` exposes Prometheus metrics about smokeng's own health. Measurement data is
deliberately **not** exported this way: sending a distribution through Prometheus means
reducing it to summary statistics, which is the thing smokeng refuses to do.

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `smokeng_build_info` | gauge | `version` | Always 1; the label carries the version |
| `smokeng_targets_active` | gauge | | Targets this instance is probing |
| `smokeng_targets_unmeasured` | gauge | | Enabled targets whose agent list names no enrolled agent — **measured by nobody** |
| `smokeng_measurements_written_total` | counter | | Measurements written to the store |
| `smokeng_measurement_write_errors_total` | counter | | Failed write batches |
| `smokeng_measurements_dropped_total` | counter | | Finalised measurements discarded — **data loss** |
| `smokeng_probe_panics_total` | counter | | Panics contained instead of ending the process — **a bug in smokeng** |
| `smokeng_late_replies_total` | counter | | Replies that arrived after their interval closed |
| `smokeng_socket_overflow_measurements_total` | counter | | Measurements taken while the receive queue overflowed |
| `smokeng_dns_failures_total` | counter | | Intervals skipped because a hostname would not resolve |
| `smokeng_ingest_accepted_total` | counter | | Agent submissions that passed verification |
| `smokeng_ingest_rejected_total` | counter | | Agent submissions refused |
| `smokeng_agent_enabled` | gauge | `agent`, `id` | 1 if enabled |
| `smokeng_agent_last_seen_seconds` | gauge | `agent`, `id` | Unix time of the agent's last submission; absent if never |
| `smokeng_alerts_firing` | gauge | | Alert rules currently firing |

Five of these are worth an alert of their own:

- `smokeng_measurements_dropped_total` increasing means smokeng is throwing away
  measurements it took. Nothing else should be able to cause a gap in the data.
- `smokeng_socket_overflow_measurements_total` increasing means loss in the graphs may be
  smokeng's rather than the network's. Those intervals are flagged in the UI too. The fix
  is a larger receive buffer, and it is worth knowing why this is a counter rather than a
  setup step: every ICMP target shares one socket per address family and DSCP, so the
  queue fills from the *total* burst rate, not from any one target. A handful of targets
  never comes close; a few hundred can, and so can a prober that stalls long enough not to
  drain. smokeng logs the shortfall at startup —

  ```
  probe: v4 socket receive buffer is 425984 bytes, asked for 4194304
  (raise net.core.rmem_max to allow more); replies may be dropped under load
  ```

  — and that line on its own is not a problem to fix. It says what the kernel granted, not
  that anything was lost. Act when the counter moves:

  ```bash
  echo 'net.core.rmem_max = 4194304' | sudo tee /etc/sysctl.d/60-smokeng-rmem.conf
  sudo sysctl -p /etc/sysctl.d/60-smokeng-rmem.conf
  sudo systemctl restart smokeng   # the buffer is sized when the socket opens
  ```

  Note that this is the setting on the machine smokeng runs on. On a virtual machine that
  is the guest's own kernel; a generous value on the hypervisor does nothing for it.
- `time() - smokeng_agent_last_seen_seconds` exceeding a few minutes means an agent is
  gone, and the absence of its graph is not the absence of a problem.
- `smokeng_targets_unmeasured` above zero means a target is assigned only to agents that
  do not exist — usually because one was removed after the assignment was written. Its
  graph is empty and looks exactly like a target that is measured and never answers.
- `smokeng_probe_panics_total` above zero is a bug in smokeng, not in your network. The
  measurement was contained rather than allowed to end the process, so the damage is a gap
  in one series — but the log holds the stack, and it is worth reporting.

Scraping needs `--metrics-public`, since a scraper cannot present a session cookie.

## Upgrades

Stop the service, replace the binary, start it. The database schema migrates forward on
open. Downgrades are not supported — take a backup first if you are trying a new version
on real data.

## Health check

`GET /healthz` needs no authentication and is the right target for a load balancer or an
uptime check.
