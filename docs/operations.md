# Operations

For the person who installs, runs and upgrades a smokeng master. It answers: how do I keep it
running, see that it is measuring, back it up and get it back. Agents have their own page:
[agents](agents.md).

1. [Install and run as a service](#install-and-run-as-a-service)
2. [Reverse proxy and TLS](#reverse-proxy-and-tls)
3. [Monitoring smokeng itself](#monitoring-smokeng-itself)
4. [Logs](#logs)
5. [Backup and restore](#backup-and-restore)
6. [Upgrade and rollback](#upgrade-and-rollback)
7. [Tuning](#tuning)
8. [Running the prober separately](#running-the-prober-separately)
9. [Storage growth and sizing](#storage-growth-and-sizing)
10. [Limits](#limits)
11. [Uninstall](#uninstall)

## Install and run as a service

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
PrivateDevices=true
ProtectSystem=strict
ProtectHome=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX AF_NETLINK
RestrictNamespaces=true
LockPersonality=true
MemoryDenyWriteExecute=true
ProtectClock=true
ProtectKernelLogs=true
ProtectHostname=true
ProtectProc=invisible
RestrictSUIDSGID=true
RestrictRealtime=true
RemoveIPC=true
SystemCallArchitectures=native
CapabilityBoundingSet=
UMask=0077

[Install]
WantedBy=multi-user.target
```

The service user needs to be in a group covered by `net.ipv4.ping_group_range` — see
[Getting started](getting-started.md). It does **not** need root, and `NoNewPrivileges`
above is compatible with unprivileged ICMP. It is not compatible with the `CAP_NET_RAW`
fallback: if you rely on `setcap` instead of the sysctl, drop that line and replace the
empty `CapabilityBoundingSet=` with `CapabilityBoundingSet=CAP_NET_RAW` next to
`AmbientCapabilities=CAP_NET_RAW`. An ambient capability outside the bounding set is
dropped *without an error*: the unit starts, `CapAmb` in `/proc/<pid>/status` stays empty,
and ICMP fails when the socket is opened (checked on systemd 259).

Configure [authentication](authentication.md) and put a TLS-terminating reverse proxy in
front of it if it is reachable by anyone but you.

The Ansible role installs the same unit (`ReadWritePaths=` in place of `StateDirectory=`)
and, optionally, a second unit for the standalone prober. Remote agents are not covered by
the role; see [agents](agents.md).

### Hardening, and what was left out

The directives above are the ones the Ansible role's units carry. They were tried on one
Linux host (Fedora CoreOS, systemd 259): every interval of every probe type arrived, none
was lost. Debian's systemd and kernel were not tried; `ProtectProc=invisible` needs
systemd 247, and an older one logs that it ignores it. The example unit itself was not
scored separately; how the hardening score was measured is in
[DESIGN.md](../DESIGN.md#appendix-a-operations-benchmarks-and-provenance).

Three directives that look free are not in it, on purpose:

- **`ProcSubset=pid`** hides `/proc/net`, where the kernel counts the replies a socket dropped
  for want of queue space. smokeng still runs and still measures, but logs `drop counter
  unavailable` and can no longer tell a reply it failed to read from one the network lost, so
  its loss figures claim more than they know. This one was tried.
- **`SystemCallFilter=`** worked for `icmp`, `tcp`, `http`, `dns` and path discovery, but `irtt`,
  `https` and the alert webhook were not tried under it, and a refused syscall fails one probe
  type quietly. If you add it, start with `SystemCallErrorNumber=EPERM` and read the log.
- **`PrivateUsers=`** worked on that kernel; how ICMP socket permission (checked against group
  ids) behaves in a user namespace has changed between kernels.

### What the web server does for itself

- **Cross-site changes are refused.** A request that changes something (anything but
  GET, HEAD and OPTIONS) is refused with 403 when the browser says it came from another
  site: `Sec-Fetch-Site` other than `same-origin` or `none`, or, from a browser that does
  not send that, an `Origin` whose scheme, host and port are neither those of the `Host` it
  was sent to (an http origin is refused where the request is known to be https) nor those
  of `--external-url`.
  A request that sends neither header is not from a browser (`curl`, an agent) and is
  let through: it carries no session cookie to ride on. `SameSite=Lax` alone does not stop
  a page on a sibling subdomain.
- **Security headers** on every response: a Content-Security-Policy that allows only the
  UI's own scripts, styles, fonts and workers and forbids framing, `nosniff`,
  `Referrer-Policy: no-referrer`, and same-origin cross-origin policies. HSTS is left to the
  proxy, which knows whether the name should be pinned to https. If you serve other content
  on the same name through the proxy, it inherits these headers too.
- **Without authentication, only its own names are answered.** A server started with no
  `--oidc-issuer` on a loopback address answers only `localhost`, loopback addresses and the
  host of `--external-url`; anything else gets 403. A page on any site can otherwise
  point a name of its own at `127.0.0.1` and read the API from the user's browser. If you
  put a proxy that does its own login in front of an unauthenticated smokeng, set
  `--external-url` to the name the proxy serves. The same setting covers a proxy that
  rewrites the `Host` header over plain http, where the browser sends only `Origin`.
- **Timeouts.** 10 s to send headers, 2 min for the whole request, 5 min to write a response,
  2 min idle.
- **Ingest memory.** At most four agent batches are decoded at once; a fifth gets `503`
  with `Retry-After`, which an agent treats as "keep it buffered", not as a bad batch. A
  schema that expands to more than 256 fields (counting nested ones and metadata
  entries) or nests more than 4 deep is refused.

### Liveness check

`GET /healthz` needs no authentication and always returns `ok` (`internal/api/server.go`).
It proves the process is up and serving HTTP. It does not check the database or the
prober, so it is a liveness check only, not a readiness check. To learn whether smokeng is
*measuring*, watch `smokeng_measurements_written_total` (below).

## Reverse proxy and TLS

`--external-url` is the address everyone else reaches it at. smokeng cannot work that out
for itself — it only sees the address it binds to. It needs it for three things: the
enrolment command shown in the UI has to name somewhere an agent can connect, it becomes
the default base for the OIDC redirect, and it decides whether the session cookie carries
`Secure`. Leave it unset only when the listen address really is the address people use.

`--trusted-proxies` takes a comma-separated list of CIDRs whose `X-Forwarded-For` may be
believed, so log lines name the real client instead of the proxy:

```
--trusted-proxies 10.0.0.0/8,192.168.1.1
```

The header is read right to left, and the first hop that is not on the list is the
answer. It affects logging and nothing else: smokeng authorises agents by signature and
browsers by session cookie, never by address, so a forged header cannot get past anything.

When `--external-url` is not set but the proxy is on `--trusted-proxies`, a request that
arrives with `X-Forwarded-Proto: https` from one of them also gets a `Secure` cookie. With
neither, smokeng cannot know the browser is on https, and with authentication enabled it
says so at startup.

Start smokeng with both flags when a proxy on the same host terminates TLS:

```
--listen 127.0.0.1:8080 --external-url https://smokeng.example.org --trusted-proxies 127.0.0.1/32
```

Caddy (`Caddyfile`; it obtains the certificate itself and sets `X-Forwarded-For` and
`X-Forwarded-Proto`):

```
smokeng.example.org {
	reverse_proxy 127.0.0.1:8080
}
```

nginx (inside a `server` block that already has `listen 443 ssl` and your certificate):

```
location / {
    proxy_pass http://127.0.0.1:8080;
    proxy_set_header Host $host;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
}
```

HSTS is left to the proxy. Agents connect to the same `--external-url`, so the proxy must
pass their requests through unchanged.

## Monitoring smokeng itself

`GET /metrics` exposes Prometheus metrics about smokeng's own health. Measurement data is
deliberately **not** exported this way: sending a distribution through Prometheus means
reducing it to summary statistics, which is the thing smokeng refuses to do.

Without `--metrics-public`, `/metrics` needs an admin session. A scraper cannot present a
session cookie and there are no API tokens, so start smokeng with `--metrics-public` to
scrape it. That serves the endpoint to anyone who can reach it; it names agents, so keep
it off the public internet (restrict it in the proxy, or scrape the loopback listener).

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

A scrape job and rules for the signals worth an alert:

```yaml
scrape_configs:
  - job_name: smokeng
    metrics_path: /metrics
    static_configs:
      - targets: ["127.0.0.1:8080"]
```

```yaml
groups:
  - name: smokeng
    rules:
      - alert: SmokengNotMeasuring
        expr: rate(smokeng_measurements_written_total[10m]) == 0
        for: 10m
      - alert: SmokengDroppingMeasurements
        expr: increase(smokeng_measurements_dropped_total[10m]) > 0
      - alert: SmokengSocketOverflow
        expr: increase(smokeng_socket_overflow_measurements_total[10m]) > 0
      - alert: SmokengAgentSilent
        expr: time() - smokeng_agent_last_seen_seconds > 300 and smokeng_agent_enabled == 1
        for: 5m
      - alert: SmokengTargetUnmeasured
        expr: smokeng_targets_unmeasured > 0
        for: 10m
      - alert: SmokengProbePanic
        expr: increase(smokeng_probe_panics_total[1h]) > 0
```

`SmokengNotMeasuring` is the nearest thing to "is it measuring" that `/healthz` cannot
tell you. It also fires when no targets are enabled.

Five of these metrics are worth an alert of their own:

- `smokeng_measurements_dropped_total` increasing means smokeng is throwing away
  measurements it took. Nothing else should be able to cause a gap in the data.
- `smokeng_socket_overflow_measurements_total` increasing means loss in the graphs may be
  smokeng's rather than the network's. Those intervals are flagged in the UI too. The fix is
  a larger receive buffer; see [Tuning](#tuning). The counter is what decides, not the
  startup log line.
- `time() - smokeng_agent_last_seen_seconds` exceeding a few minutes means an agent is
  gone, and the absence of its graph is not the absence of a problem.
- `smokeng_targets_unmeasured` above zero means a target is assigned only to agents that
  do not exist — usually because one was removed after the assignment was written. Its
  graph is empty and looks exactly like a target that is measured and never answers.
- `smokeng_probe_panics_total` above zero is a bug in smokeng, not in your network. The
  measurement was contained rather than allowed to end the process, so the damage is a gap
  in one series — but the log holds the stack, and it is worth reporting.

**Failed webhook deliveries have no metric.** A refused or unreachable alert webhook shows up
only in the log (`alert: deliver N alert(s): ...`). A dead Alertmanager therefore does not
alert on its own: watch the log. This is a limit. `smokeng_alerts_firing` says rules are
firing, not that anyone was told.

## Logs

smokeng logs with Go's standard `log` package to stderr, with UTC timestamps
(`cmd/smokeng/main.go`). Under systemd that is the journal:

```bash
journalctl -u smokeng -f
journalctl -u smokeng --since "1 hour ago" | grep -i 'dropped\|panicked\|deliver'
```

There is one verbosity and no level flag or log-file option. Lines read `component: message`.
The ones to know, as they appear in the source:

| Line (start of text) | Means |
| --- | --- |
| `smokeng <version> listening on http://... (db: ...)` | Started. Check the version after an upgrade |
| `probe: <v4\|v6> socket receive buffer is N bytes, asked for M` | The kernel granted less than asked. Not a problem by itself; see [Tuning](#tuning) |
| `probe: ... datagram ICMP socket not permitted, using raw-socket fallback` | Unprivileged ICMP is not allowed for the service group. Measurements are flagged degraded |
| `probe: ... socket drop counter unavailable` | `/proc/net` is hidden; loss figures cannot separate a failed read from network loss |
| `probe: write N measurements: ...; they are dropped and not alerted on` | A write failed: data loss |
| `probe: results backlog full, dropping measurement for target N` | The writer cannot keep up |
| `probe: target N (...) is not being measured: ...` | A target is configured but skipped, with the reason |
| `probe: target N (...) ... panicked` / `probe loop panicked` | A bug; the stack follows. Report it |
| `probe: target N (...) path changed` | Path discovery saw a different route |
| `alert: deliver N alert(s): ...` | Webhook delivery failed. The only trace of it |
| `alert: the delivery queue is full; dropped a batch of N alert(s)` | The webhook has been failing long enough to lose notifications |
| `alerting evaluated but not delivered: no --alert-webhook ...` | Rules run, nobody is told |
| `ingest: agent "name" turned away, N batches are already being decoded` | Back-pressure; the agent keeps its batch |
| `ingest: agent "name" sent an undecodable batch` | The agent will discard that batch |
| `enrol: refused a token from ...` | A failed enrolment attempt |
| `auth: token exchange: ...` / `auth: verify id_token: ...` | OIDC login failures |
| `store: ... is readable by other users` | The database file was too open; the line says whether it was fixed |
| `retention: pruned N measurement(s) past their horizon` | Retention ran |
| `shutting down` | Clean stop on SIGTERM or interrupt |

## Backup and restore

The database is a single SQLite file in WAL mode. Do not copy it with `cp` while smokeng
is running; use SQLite's own online backup:

```bash
(umask 077 && sqlite3 /var/lib/smokeng/smokeng.db ".backup '/var/backups/smokeng-$(date +%F).db'")
```

**Treat the database, and every copy of it, as a secret.** It holds the key that signs
session cookies; whoever can read it can forge a session as any user, an admin included.
smokeng creates it mode 0600, and closes an existing one to other users (keeping what you
gave its group) with a log line at startup, but a backup is made by whoever runs the
command above, under their umask, in a directory you chose. The `umask 077` in that
command is what keeps it private; `/var/backups` itself is usually world-listable, so
check the mode of what lands there. If a copy was ever readable by others, rotate the key:
stop smokeng, run `sqlite3 /var/lib/smokeng/smokeng.db "DELETE FROM settings WHERE key = 'session_key'"`,
and start it again. A new key is generated, and everyone signs in again.

Keep your `targets.toml` in Git as well. Between the TOML and the database you can rebuild
either one: `config export` regenerates the file from the database, and `config import`
rebuilds the tree from the file.

Agent hosts hold `probe.key`. Losing one is not a disaster — generate a new key and
re-enrol the agent — but the buffered measurements in the agent's local database are lost
if you delete that too.

### Restore

Stop smokeng, put the backup in place, remove the stale WAL files, start it:

```bash
sudo systemctl stop smokeng
sudo install -o smokeng -g smokeng -m 0600 /var/backups/smokeng-2026-11-04.db /var/lib/smokeng/smokeng.db
sudo rm -f /var/lib/smokeng/smokeng.db-wal /var/lib/smokeng/smokeng.db-shm
sudo systemctl start smokeng
```

The `-wal` and `-shm` files belong to the database that was replaced. Left behind next to
a different file, SQLite may apply them to it. Everything measured after the backup was
taken is gone; agents re-send only what is still in their local buffers.

## Upgrade and rollback

1. Take a backup (above). Keep it until you are sure of the new version.
2. Stop the service, replace the binary, start it. The database schema migrates forward on open.
3. Check `journalctl -u smokeng` for the new version in the `listening` line.

Migrations run in one transaction (`internal/store/sqlite.go`), so a failed migration
leaves the database at its old version rather than half-changed. The Ansible role does not
take a backup before an upgrade; do it first.

### Rollback

Downgrades are not supported. To go back, stop the service, put the previous binary back,
and restore the pre-upgrade backup as described under Restore. Measurements taken since the
backup are lost.

Do not skip the restore. An older binary opens a newer database without complaint, and one
from before target, agent and rule ids stopped being reused will reuse them again, because
the counters that prevent it live in tables it does not know about. Upgrading again does
not repair that: the counters are seeded once, by the migration. If you did downgrade and
ids were created in between, restore the backup instead.

### Compatibility between master and agents

**Master and agents may be upgraded in either order.** The submission format
gains columns over time, and both directions tolerate the difference: a master
resolves columns by name and ignores one it has never heard of, so a newer
agent's batch is read by an older master; and every column added so far is
optional, so an older agent's batch is read by a newer master and simply carries
less. Both are pinned by tests, because this is the kind of property that is
easy to believe and easy to break.

The rule that keeps it true is that **a column added to the batch format must be
optional**. Making one required would refuse every older agent's batch, and an
agent discards a batch the master calls undecodable — that is deliberate, since
re-sending the same bytes forever means never delivering anything again, but it
means the cost of breaking this rule is the buffered measurements, not just a
delay.

## Tuning

### Receive buffer

`smokeng_socket_overflow_measurements_total` counts measurements taken while a receive
queue overflowed. Every ICMP target shares one socket per address family and DSCP, so the
queue fills from the *total* burst rate, not from any one target. A handful of targets
never comes close; a few hundred can, and so can a prober that stalls long enough not to
drain. smokeng asks for a 4 MiB buffer and logs what the kernel granted:

```
probe: v4 socket receive buffer is 425984 bytes, asked for 4194304
(raise net.core.rmem_max to allow more); replies may be dropped under load
```

That line says what the kernel granted, not that anything was lost; a host can log it for
months and never drop a reply. The counter decides. The Ansible role raises the ceiling
anyway, through `smokeng_rmem_max`, so the warning does not fire at every start. By hand:

```bash
echo 'net.core.rmem_max = 4194304' | sudo tee /etc/sysctl.d/60-smokeng-rmem.conf
sudo sysctl -p /etc/sysctl.d/60-smokeng-rmem.conf
sudo systemctl restart smokeng   # the buffer is sized when the socket opens
sudo systemctl restart smokeng-prober   # too, if the standalone prober runs here
```

The restart is not optional: a socket's queue is sized when it opens, so a raised ceiling
reaches a running prober only through a new one. The setting belongs to the machine
smokeng runs on. On a virtual machine that is the guest's own kernel.

Timing accuracy per probe type is in [probes](probes.md).

## Running the prober separately

By default one process does everything: scheduler, probing engine, database, API and web
UI. That is the right shape for one host. `smokeng agent run` is the probing engine without
the UI, and pointed at loopback it is a separate prober on the same machine as the master.
Reasons to want that:

- the tail of the distribution gets cleaner for the userspace-timed probe types;
- restart or upgrade the UI without interrupting measurement, and the reverse;
- keep the unprivileged-ICMP permission on the half that needs it, off the half that
  serves a web page;
- contain a prober that is misbehaving without taking the API down with it.

The decision rule:

- **Only `icmp` or `dns` targets?** Change nothing. Their timestamps are taken in the
  kernel as the packet crosses the wire, so load in the process cannot move them.
- **`tcp`, `http`, `https` or `irtt` targets, on an instance whose UI gets real traffic?**
  Run the prober as its own process. A TCP or TLS handshake completes inside the kernel and
  userspace only observes the call returning, so these types cannot be stamped at the source,
  and CPU contention in the same process shows up in the graph as smoke.

The measurements behind this are in
[DESIGN.md](../DESIGN.md#appendix-a-operations-benchmarks-and-provenance). They come from
one machine and one hard load, so they show the direction, not what you will see. The
agent unit is in [agents](agents.md). The Ansible role can also write a `smokeng-prober` unit.

## Storage growth and sizing

smokeng keeps every measurement at full resolution forever. That is the whole point, and
it costs disk in a way that a fixed-size RRD file does not:

- One series at a 60-second interval is ~525 600 rows/year at roughly 55 bytes per row
  (encoded samples plus key and B-tree overhead) — about **28 MB per series per year**.
- 100 targets from one vantage point: ~3 GB/year.
- The same 100 targets from three agents: 300 series, ~8 GB/year.

The cost is per *series*, and a series is a (target, agent) pair. Ping count barely moves
it — samples are stored as sorted deltas in microseconds, so 20 pings and 60 pings differ
by a handful of bytes. Halving `interval_s` doubles it.

The SQLite backend is comfortable into the **low thousands of series** (roughly 30–60
GB/year). Past that a single SQLite file is the wrong shape, and the `Store` interface is
the seam where a columnar backend would go. That work is designed but not built; if you
are planning fifteen thousand series, say so before you start.

CPU, RAM and WAL size have not been measured, so there is no figure to give. Measure on
your own load, and watch `smokeng_measurements_dropped_total`.

Retention is opt-in and per target. `retention_s` is `0` — keep forever — by default.
Set it on a node (it inherits like every other setting, so a group sets the policy for its
subtree) and measurements older than that many seconds are pruned on a timer,
`--retention-interval` (default one hour), in time slices so clearing a backlog is a series
of short write locks rather than one that stalls the prober.

Retention never coarsens. An old interval is deleted whole, not averaged into a summary, so
history before the horizon reads as absent — never as a downsampled figure. Downsampling is
unimplemented and unplanned for that reason.

## Limits

- **One master, no HA.** There is no clustering or failover. A second instance is a second,
  unrelated installation.
- **A single SQLite writer.** One file, one process. Do not point two masters at it.
- **Recovery is restore from backup.** Measurements since the last backup are lost; agents
  re-send only what they still buffer.
- **Failed webhook deliveries are visible only in the log.** See
  [Monitoring smokeng itself](#monitoring-smokeng-itself).
- **No readiness check.** `/healthz` is liveness only.
- **No API tokens.** Prometheus scrapes need `--metrics-public`.

### Containers and Kubernetes

There is no container image, and running smokeng in a container is not supported. It
would need:

- unprivileged ICMP: the `net.ipv4.ping_group_range` sysctl for the container's group, or
  `CAP_NET_RAW`;
- exactly one replica, never a rolling update that runs two at once;
- a persistent volume for the database.

## Uninstall

```bash
sudo systemctl disable --now smokeng           # and smokeng-prober, if installed
sudo rm /etc/systemd/system/smokeng.service    # and smokeng-prober.service
sudo systemctl daemon-reload
sudo rm /usr/local/bin/smokeng
sudo rm -f /etc/sysctl.d/99-smokeng.conf /etc/sysctl.d/60-smokeng-rmem.conf
sudo userdel smokeng
```

The Ansible role writes both sysctls (`net.ipv4.ping_group_range`, `net.core.rmem_max`) into
`/etc/sysctl.d/99-smokeng.conf`. `60-smokeng-rmem.conf` is the manual drop-in from
[Tuning](#tuning). Removing a file does not undo the live values until the next reboot or
`sysctl --system`. If another service on the host needs unprivileged ping, keep the
`ping_group_range` setting.

Keep `/var/lib/smokeng` until you are sure: it holds the database, which is your history,
and anything else the role put there (webhook and OIDC secret files, `prober.key`,
`prober.db`). Delete it only once you have a backup you can restore, and treat that backup
as a secret (see [Backup and restore](#backup-and-restore)). Keep your `targets.toml`.
SmokePing users: [migrating from SmokePing](migrating-from-smokeping.md).
