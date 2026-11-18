# Troubleshooting

For whoever is on call when smokeng misbehaves. Find the symptom, run the check, apply the fix.
Every log line is quoted as the code writes it; `…` marks a part that varies. Under systemd
the log is `journalctl -u smokeng` (and `-u smokeng-prober` for a standalone agent).

Symptoms:

- [The service will not start](#the-service-will-not-start)
- [The UI loads but there is no data](#the-ui-loads-but-there-is-no-data)
- [ICMP: permission denied, or "raw-socket fallback"](#icmp-permission-denied)
- [Dropped replies](#dropped-replies)
- [An agent is not reporting](#an-agent-is-not-reporting)
- [A remote agent shows no path-change marks](#a-remote-agent-shows-no-path-change-marks)
- [Login fails, or a change is refused with 403](#login-fails-or-403-behind-a-reverse-proxy)
- [An admin is not an admin](#an-admin-is-not-an-admin)
- [Alerts are not delivered](#alerts-are-not-delivered)
- [Everything is up, but the graph says loss](#everything-is-up-but-the-graph-says-loss)
- [Quality badges](#quality-badges)

## The service will not start

`smokeng serve` refuses before it touches the database where it can. The message is the first
line of the log. Flags are in [Configuration](configuration.md) and
[Authentication](authentication.md).

| You see | Cause and fix |
|---|---|
| `refusing to listen on non-loopback "…" without authentication: configure --oidc-issuer, or pass --i-know-this-is-unauthenticated to override` | `--listen` is not loopback and no OIDC is set. Configure OIDC, or listen on `127.0.0.1:…` behind a proxy that authenticates. |
| `--oidc-issuer is set without --oidc-admin-value, which would make every user the provider authenticates an administrator. …` | OIDC needs an admin decision. Set `--oidc-admin-value GROUP`, or `--oidc-everyone-is-admin` if that really is what you want. |
| `--oidc-everyone-is-admin and --oidc-admin-value both given: choose one` | Pass one of them. |
| `--oidc-everyone-is-admin does nothing without --oidc-issuer` | Remove the flag, or add the issuer. |
| `--oidc-client-secret and --oidc-client-secret-file both given: …` | Pass one. Prefer the file. |
| `--oidc-client-secret-file: … is empty`, or `--alert-webhook-file: … is empty` | The file exists but holds nothing (whitespace is trimmed). |
| `--alert-webhook and --alert-webhook-file both given: pass one` | Pass one. |
| `the alert webhook is not an http or https URL with a host` | Fix the URL. The message does not repeat it, because it is often a credential. |
| `--default-role must be viewer or none, got "…"` | Use `viewer` or `none`. |
| `--external-url must be an absolute URL like https://smokeng.example.org, got "…"` or `--external-url is a base address and takes no path, got "…"` | Give scheme and host only, with no path. |
| `--trusted-proxies: …` | The value is a comma-separated list of CIDRs or bare addresses. Check for a typo. |
| `--tls-ca-file: …` | The file is unreadable, holds no PEM certificate, or holds a private key or any non-certificate block. A CA file may hold certificates only. |
| `--irtt-hmac-keys: …` | Not a TOML map of `"host:port" = "key"`, or a key is empty. |
| `--alert-repeat must be positive, got …`, `--retention-interval must be positive, got …` | Use a positive duration. |
| `auth: discover …` | The OIDC issuer URL was unreachable at start-up, or is not an issuer. |
| A Go error from `store.Open` (`store: …`, or the OS error for the path) | The database path is not writable by the service user, or its directory is missing. |

A secret file that is group- or world-readable does not stop the start. It logs
`warning: … holds a secret but is mode …`. Run `chmod 600` on it.

Check what the flags actually are:

```bash
systemctl cat smokeng | grep -A20 '^ExecStart'
```

## The UI loads but there is no data

Check in this order:

1. **Is anything measuring?** Look for `probe engine: …` in the log. If the engine stopped, the UI
   still loads. An ICMP socket failure is the usual cause: see [below](#icmp-permission-denied).
2. **Is the target skipped?** The log says `probe: target … (…) is not being measured: …`:
   - `probe_type tcp needs a probe_port, and there is no sensible port to guess`: set `probe_port`.
   - `probe_type irtt needs a positive send interval; in burst mode that means burst_gap_ms > 0`:
     set `burst_gap_ms`.
   - `probe_type "…" has no prober behind it`: the type is misspelled.
3. **Does the target name an agent that does not exist?** A target whose `agents` list names no
   enrolled agent is measured by nobody. The `smokeng_targets_unmeasured` metric counts them.
   Fix the `agents` value, or enrol the agent: [Agents](agents.md).
4. **Is the target disabled?** Disabled targets are skipped.
5. **Does the name resolve?** `probe: target … ("…"): resolve: …` means no measurement for that
   interval, which the graph shows as a gap. `smokeng_dns_failures_total` counts them.
6. **Is the time window right?** The range buttons go back 30 days, and `custom` takes any
   window; see [Reading graphs](reading-graphs.md#long-windows). The raw intervals are in the
   API (Arrow): [API](api.md). There is no CSV export of measurements.
7. **Are writes failing?** `probe: write … measurements: …; they are dropped and not alerted on`
   (also `smokeng_measurement_write_errors_total`), or `probe: results backlog full, dropping
   measurement for target …` (also `smokeng_measurements_dropped_total`). Check disk space and
   that the database file is writable.

`/metrics` needs an admin session unless the server runs with `--metrics-public`.

## ICMP permission denied

You see, in the log, with the service running and no ICMP data:

```
probe: cannot open v4 ICMP socket (datagram and raw both failed): … — on Linux, allow unprivileged ping sockets with: sysctl -w net.ipv4.ping_group_range="0 2147483647"
```

or, when only the raw fallback works, the `raw socket` badge and:

```
probe: v4 datagram ICMP socket not permitted, using raw-socket fallback (degraded, flagged)
```

Cause: the service user's group is outside `net.ipv4.ping_group_range`, and the process has no
`CAP_NET_RAW`.

```bash
sysctl net.ipv4.ping_group_range
id -G "$(systemctl show -p User --value smokeng)"
```

Fix, preferred: put the service group's gid inside the range, and persist it in
`/etc/sysctl.d/99-smokeng.conf`. The recipe is step 2 of [Getting started](getting-started.md).
The wide range in the log line is a hint, not the advice: give it to the service group only.
The Ansible role does exactly that.

Fix, alternative: `sudo setcap cap_net_raw+ep /usr/local/bin/smokeng`. **This does not work
under the
hardened unit.** `NoNewPrivileges=true` stops a file capability from taking effect. Use
`AmbientCapabilities=CAP_NET_RAW` with `CapabilityBoundingSet=CAP_NET_RAW` and drop
`NoNewPrivileges`. An ambient capability outside the bounding set is dropped without an error:
the unit starts, and ICMP fails only when the socket opens. Check it:

```bash
grep CapAmb "/proc/$(systemctl show -p MainPID --value smokeng)/status"
```

`CapAmb` must not be all zeros. The Ansible role sets the sysctl for you, to the service
group's gid only. Detail: [Operations](operations.md), [Getting started](getting-started.md).

## Dropped replies

You see the `dropped replies` badge, loss that appears on many targets at once, or one of these
log lines:

```
probe: v4 socket receive buffer is … bytes, asked for 4194304 (raise net.core.rmem_max to allow more); replies may be dropped under load
probe: v4 socket drop counter unavailable; loss on this socket may include replies we failed to read in time, and cannot be told apart
```

- The first line says what the kernel granted, not that anything was lost. The deciding signal
  is the
  `dropped replies` badge, or `smokeng_socket_overflow_measurements_total` rising.
- The second line means smokeng cannot read the kernel's drop counter for that socket, so it
  cannot flag overflow. Loss on that socket may include replies smokeng failed to read in time.
  The cause is not logged. Treat loss there with care.

Check, and fix by raising the ceiling, then restarting (a socket's queue is sized when it opens):

```bash
sysctl net.core.rmem_max
echo 'net.core.rmem_max = 4194304' | sudo tee /etc/sysctl.d/60-smokeng-rmem.conf
sudo sysctl -p /etc/sysctl.d/60-smokeng-rmem.conf
sudo systemctl restart smokeng
```

The Ansible role does this through `smokeng_rmem_max`. Detail: [Operations](operations.md).

## An agent is not reporting

Agents sign every request. The master logs the reason for a refusal; the agent only sees a
status. On the master:

```bash
journalctl -u smokeng --since "1 hour ago" | grep -E 'ingest:|enrol:'
smokeng agent list --db /path/to/smokeng.db
```

| You see | Cause and fix |
|---|---|
| Master: `enrol: refused a token from …: store: enrolment token is not valid: …`. Agent: `agent: enrolment refused: rejected` | The token ends with `no such token`, `already redeemed at …` or `expired at …`. A token is single-use and expires. Mint a new one in the UI under Agents. The agent records its id after the first success, so `--token` is not spent again on restart. |
| Master: `ingest: rejected: agent "…" timestamp is … away (check its clock)` | Clock skew over 5 minutes in either direction. Fix NTP on the agent host: `timedatectl`. |
| Master: `ingest: rejected: agent "…" is disabled` | Enable it in the UI, or `smokeng agent enable --db … ID`. |
| Master: `ingest: rejected: unknown agent …` | The agent id is not enrolled here. It was removed, or it points at a different master or database. |
| Master: `ingest: rejected: agent "…" signature does not verify` | The key on the agent is not the one enrolled. Print the agent's key with `smokeng agent key --key …` and re-enrol it, or use a fresh token. |
| Master: `ingest: rejected: agent "…" reused a nonce` or `is over its rate limit` | A replayed request, or an agent submitting faster than the limit. Look for two agents sharing one key. |
| Agent: `agent: master rejected the batch with 401 Unauthorized (keeping … buffered)` | One of the lines above. The agent keeps its measurements and retries. |
| Agent: `agent: master URL "…" is neither HTTPS nor on loopback; pass --insecure-allow-http only for local development` | Wrong or plain-HTTP master URL. Use the `https://` address. |
| Agent: `agent: refusing to send an enrolment token to "…" over plain HTTP …` | The same, at enrolment. |
| Agent: `agent: push: … x509: …` or `agent: initial pull failed: … x509: …` | The master's certificate is not trusted by the agent host. `--tls-ca-file` does **not** apply to the connection to the master. Install the CA in the agent host's system trust store. |
| Agent: `agent: pull assignments: …` with a timeout or connection error | Wrong `--master` URL, DNS, or a firewall. |
| Agent: `agent: master could not decode a batch of … measurement(s) …; discarding it` | The master answered 400 to a batch it could not decode. The agent discards that batch, because re-sending the same bytes would block every later one. This is rare: columns are resolved by name, so master and agent may be upgraded in either order. See [Operations](operations.md) for the upgrade rules. If it repeats, compare the versions on both sides. |
| Agent: `this agent is not enrolled yet. Either: …` | No `--token` and no recorded id. Follow the message. |

Check reachability from the agent host:

```bash
curl -m8 -sS -o /dev/null -w '%{http_code}\n' https://smokeng.example.org/healthz
```

`/healthz` only proves the process answers. `smokeng_agent_last_seen_seconds` and
`smokeng_ingest_rejected_total` show whether the agent is reaching the master. The refusal
reason is in the log only: a metric label would tell a caller which check they failed.
Detail: [Agents](agents.md).

## A remote agent shows no path-change marks

Check, in order:

1. **Versions.** The master has to be v0.26.0 or later to ask for paths and accept them; the
   agent too. An agent whose master has no route logs
   `agent: the master has no route for path changes` and keeps them buffered.
2. **`trace_interval_s`** above zero for the target, as on the master.
3. **The agent's platform and sysctl.** Path discovery is Linux only and needs the same
   `ping_group_range` as ICMP; elsewhere the agent logs
   `probe: path discovery unavailable on this platform; no routes will be recorded`.
4. **Time.** The first traceroute runs a few seconds after the target is assigned, and an agent
   sends its buffer every 15 seconds. The first path recorded is a mark too; after that, a mark
   means the hop list changed. The hover readout names the route in force.

Detail: [Reading graphs](reading-graphs.md#path-change-marks).

## Login fails, or 403 behind a reverse proxy

| You see | Cause and fix |
|---|---|
| `login expired, try again` after returning from the IdP | The browser did not send the state cookie back. The cookie is `Secure` while the browser is on plain http (for example `--external-url https://…` but a visit over http), or the login took over 10 minutes. Use the https address. |
| `bad login state`, `state mismatch`, `nonce mismatch` | A stale or replayed login. Start again from `/auth/login`. |
| `login failed` | The log has the reason: `auth: token exchange: …` (usually a wrong client secret or redirect URL) or `auth: verify id_token: …` (for example the clock, audience or issuer). |
| `provider returned no id_token` | The IdP's token response held no ID token. Check that the client is an OIDC client. |
| The IdP rejects the redirect URL | The URL registered at the IdP must equal `--oidc-redirect-url`. When that flag is unset it is `--external-url` plus `/auth/callback`, else `http://<--listen>/auth/callback`. |
| 403 `a change must be made from this site, not from another; if this is the site and a proxy rewrites the Host header, set --external-url to the address people use` | The origin check on a change (POST/PUT/PATCH/DELETE). The browser's `Origin` does not match the `Host` the server sees. Set `--external-url https://smokeng.example.org`, or make the proxy pass `Host` through. |
| 403 `this server answers only to localhost while it has no authentication; set --external-url if it is reached through a proxy under another name` | No OIDC is configured, so only loopback host names are accepted. Set `--external-url`, or configure OIDC. |
| Log: `the session cookie is not marked Secure, because nothing says browsers arrive over https; behind a proxy that terminates TLS, set --external-url or --trusted-proxies` | The proxy terminates TLS but smokeng was never told. Set `--external-url https://…`, or `--trusted-proxies` with the proxy's address so `X-Forwarded-Proto` counts. |
| 401 `not signed in` from the API | No session cookie. smokeng has no API tokens: automation needs a browser session. |

`--trusted-proxies` takes a comma-separated list of CIDRs or addresses whose `X-Forwarded-For`
and `X-Forwarded-Proto` are believed. Without it, the client address in the log is the proxy's.
Detail: [Authentication](authentication.md), [Operations](operations.md).

## An admin is not an admin

Anyone the IdP lets through who does not match the admin value is a viewer. That is deliberate:
a provider that renames or stops sending the claim demotes people instead of promoting them.

- The claim is `groups` unless `--oidc-admin-claim` says otherwise. Spell it exactly.
- The claim may be an array of strings, or one string split on **commas only**. A space is part
  of a group name: `Domain Admins` is one group, and `"a, b"` is the two groups `a` and `b`.
- Matching is exact and case-sensitive against `--oidc-admin-value`.
- A provider that does not put group names in the ID token cannot make admins here. Fix it at
  the IdP, or use `--oidc-everyone-is-admin` behind an authenticating proxy.
- A user's role is fixed in their session cookie. After changing groups, sign out and in.

Check what the IdP sends: use its token or claim preview for that client, and compare the claim
character by character with the flag. Do not paste a live token into a ticket. Detail:
[Authentication](authentication.md), [Access control](access-control.md).

## Alerts are not delivered

The log tells you which of three states you are in:

| Log line at start-up | Meaning |
|---|---|
| `alerting evaluated but not delivered: no --alert-webhook or --alert-webhook-file is set` | Rules are evaluated. Firing state and the transition log are in the UI. Nothing is posted anywhere. Set a webhook. |
| `alerting enabled, posting to … (repeating every …)` | Delivery is on. The URL is redacted. |

If delivery is on and nothing arrives, check `smokeng_webhook_deliveries_total` on `/metrics`:
`error` or `dropped` rising means POSTs are failing. The reason is in the log:

```bash
journalctl -u smokeng --since "1 hour ago" | grep 'alert:'
```

- `alert: deliver … alert(s): alert: webhook …: …`: the receiver is unreachable or timed out.
- `alert: deliver … alert(s): alert: webhook … returned 4xx/5xx …`: the receiver answered with
  an error.
- `alert: the delivery queue is full; dropped a batch of … alert(s), the oldest`: the receiver is
  slow or down for long enough to fill the queue (64 batches). The oldest batches are lost.
- No line at all: the rule is silenced, or has not been in breach for `for_intervals` yet.

Set up a log alert on `alert: deliver`, or you will find out from the users. Detail:
[Alerting](alerting.md).

## Everything is up, but the graph says loss

A TCP refusal and an HTTP status of 400 or higher count as loss, and so does a rejected
certificate. The log names the first two once per interval. Tables and the fixes (for example
`http_path`, `--tls-ca-file`): [Probes](probes.md).

A probe this host could not send carries the `not sent` badge with a reason (see below). Fix
the local cause (`ulimit -n`, `LimitNOFILE=`, route, firewall), not the target.

## Loss on a graph that might be rate limiting

The graph shows loss on one target, and the target, its CPE or a router in front of it may be
rate limiting ICMP. On a graph that looks like network loss. In the order the probes were sent
it does not: a rate limiter answers the first probes of a burst and drops the tail.

The master looks for this by itself. Intervals whose loss sits at fixed send positions carry the
`positional loss` badge on the graph; hover it for the positions. The command below is for
looking at the evidence behind the badge, and for a window or a target you want to judge
yourself.

```bash
smokeng diag loss-pattern --db smokeng.db --target /Production/web --since 24h
```

Sample (the list of recent intervals is shortened here):

```
/Production/web from local, 2026-11-16 12:00Z to 2026-11-17 12:00Z

90 intervals, 21 with loss, 20 of those with recorded positions
1 had loss without recorded positions: measured before positions were recorded, total loss, an irtt probe, or an interval flagged as partly smokeng's own doing

Judged: the last 20 intervals with recorded loss, from 2026-11-16 21:00Z to 2026-11-17 11:00Z, 10 probes per interval, burst

Losses per send position:
   1                                            0
   2                                            0
   3                                            0
   4                                            0
   5                                            0
   6                                            0
   7                                            0
   8  ██████████████████████████████            15
   9  ████████████████████████████████████████  20
  10  ████████████████████████████████████████  20

p = 0.0001

Last 20 intervals with recorded loss, newest last (. answered, x lost, in send order):
  2026-11-16 21:00Z  .......xxx
  2026-11-16 22:00Z  .......xxx
  2026-11-16 23:00Z  .......xxx
  …
  2026-11-17 09:00Z  .......xxx
  2026-11-17 10:00Z  .......xxx
  2026-11-17 11:00Z  .......xxx

Loss is concentrated at probes 8–10. That does not fit loss that falls where it happens to; it is consistent with rate limiting by the target, and is not proof of it.
```

How to read it: the histogram counts, per send position, how many of the judged intervals lost
that probe. Loss from the network falls where it happens to and gives a flat histogram; here
all the loss is at positions 8 to 10 and positions 1 to 7 lose nothing. The p-value is how
often chance alone, on loss with the same shape, made the profile this lopsided. The command
flags a window below p = 0.0005. The conclusion line says which of the three cases it is.

What it can and cannot tell you:

- **A flag means "does not fit random loss", not "is rate limiting".** Anything else that
  drops the same probes at the same positions looks the same.
- **No flag is not a clean bill.** It says the loss was not lopsided enough to rule chance
  out, which is also what a short or weak pattern looks like.
- It needs at least twelve intervals with lopsided recorded loss, and ten lost probes, in the
  window. Less is reported as `Not enough to judge`, with the reason.
- It only sees intervals measured since loss positions were recorded. Older intervals, total
  loss, intervals flagged as partly smokeng's own doing (`not sent`, `cut short`, `dropped
  replies`, `clock step`) and `irtt` targets have counts but no positions; the output says how
  many.
- It cannot see a pattern that is its own mirror image, such as the first and the last probe
  lost equally often.
- It judges the newest recorded intervals, at most 60 and none more than a day older than the
  newest, and stops at a change of probe count or mode.

Detail: [Command-line reference](cli.md#diag-loss-pattern).

## Quality badges

Meanings: [Reading graphs](reading-graphs.md). What to do:

| Badge | Action |
|---|---|
| `userspace TX`, `userspace RX` | Expected on macOS. On Linux, find out why kernel timestamping is not in use before trusting sub-millisecond detail. |
| `raw socket` | Set `ping_group_range`: [see above](#icmp-permission-denied). |
| `dropped replies` | Raise `net.core.rmem_max` and restart: [see above](#dropped-replies). |
| `clock step` | Check NTP: `timedatectl`, `chronyc tracking`. Ignore the spike. |
| `cut short` | Nothing to fix. Do not read a one-off loss there as an outage. |
| `not sent` plus a reason | See the next table. |
| `positional loss` | Run `smokeng diag loss-pattern` ([above](#loss-on-a-graph-that-might-be-rate-limiting)). If it is rate limiting, try `probe_mode = "spread"` ([Probes](probes.md#rate-limiting-and-burst-or-spread)). The badge does not change the loss figures. Alert rules on `loss` read flagged intervals as loss-free unless `positional_loss = "flag_only"` ([Alerting](alerting.md#loss-at-fixed-send-positions)). |
| An ICMP error name (`host unreachable`, `administratively filtered`, `TTL exceeded`, ...) | Look at the router or firewall the error names. `administratively filtered` is a filter rule. |

The `not sent` reasons, and where to look:

| Reason | Look at |
|---|---|
| `the socket refused the write` | This host: load, descriptors, firewall. |
| `connection refused — the far end, or a router speaking for it, rejected the traffic` | The target side. |
| `no route to the target` | Routing on this host. |
| `no address to send to` | DNS or the target's address family. |
| `the session was refused` | The irtt server refused the session. |
| `the session broke part-way` | The path or the irtt server. |
| `the session got no reply — silence, which is not the same as refusal` | A filter or a server that is down. |
| `this prober's own deadline expired before sending — nothing was asked of the target` | This host: it is overloaded. |
| `the session sent fewer probes than asked — this prober fell short, not the network` | Retired. It can appear only on old data. |

## Still stuck

Collect the log around the event and the output of `smokeng agent list`, then compare against
[Operations](operations.md). If a graph needs proof rather than a guess, see
[Proving a problem](proving-a-problem.md). Flags: [CLI](cli.md).
