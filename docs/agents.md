# Remote agents

For anyone adding a second vantage point: where to put agents, how to enrol and run them, and
what they cannot do.

An agent is the same binary in a different mode: it pulls its assignments from the master,
probes them itself, and pushes the measurements back. Each agent's measurements are stored
as a separate series, so a target probed from three places gives you three plots, not one
blended average.

## Where to put agents

Use at least two vantage points. One vantage point tells you a target is slow; two tell you
where.

- **An agent at home or in the office, and one in a datacenter.** If both see the problem,
  it is probably the target or its network. If only the first does, it is your own link.
  That is the difference between "my link" and "their network".
- Put an agent on the far side of anything you suspect: a different ISP, a different cloud
  region, the other end of a VPN.
- Assign the same targets to each (see [Assigning targets](#assigning-targets)) so the
  plots are comparable.

What one agent cannot tell you: whether the problem is on its side of the path or the
target's. It also cannot tell you where on the path, since agents run no path discovery
(see [Limits](#limits)). Several agents narrow it down; they do not replace a traceroute run
on the host.

## Enrolling an agent

Enrolment needs an admin session in the UI (or the equivalent API calls, see
[Enrolment at scale](#enrolment-at-scale)).

1. In the UI, open **Agents** and choose a name.
2. Mint a token. It is shown once and stored only as a hash. It is valid for one hour by
   default and 24 hours at most, and it works once.
3. Run the command shown on the agent host:

   ```bash
   smokeng agent run --master https://smokeng.example.org --token smk_...
   ```

   The agent generates its keypair if it has none, enrols itself, and records the id it was
   given in `<key path>.id`. A unit file may keep carrying `--token`: the recorded id wins,
   so a restart does not try to spend a spent token.
4. Add the agent's name to a target's `agents` setting.

The address in the command is the master's `--external-url` when set, and the address you
opened the page on otherwise. Set `--external-url` whenever a proxy sits in front, or an
admin who reached the master directly is handed an address no agent can resolve.

The token carries the name, so an agent cannot name itself: an agent that named itself could
claim a name your targets are already assigned to.

The token is the only credential on the enrolment request, so the agent refuses a plain-HTTP
master unless it is on loopback or you pass `--insecure-allow-http`.

### Without a token

To keep a token out of flight entirely, carry the key by hand. On the agent host, print the
public key without starting anything:

```bash
smokeng agent key --key /var/lib/smokeng-agent/probe.key
```

On the master:

```bash
smokeng agent add --db /var/lib/smokeng/smokeng.db --name ams-01 --pubkey <base64 from above>
```

It prints the new id. Start the agent with `--agent-id <id>` instead of `--token`.

## Running an agent as a service

The defaults of `--key` (`probe.key`) and `--db` (`smokeng-agent.db`) are relative to the
working directory. Under `ProtectSystem=strict` that directory is read-only and the agent
fails to start. Pass absolute paths under a state directory.

The ICMP prober needs the same `ping_group_range` setup as the master: see
[Getting started](getting-started.md). The service user's group must be in range.

```ini
# /etc/systemd/system/smokeng-agent.service
[Unit]
Description=smokeng agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=smokeng
Group=smokeng
ExecStart=/usr/local/bin/smokeng agent run \
  --master https://smokeng.example.org \
  --token smk_... \
  --key /var/lib/smokeng-agent/probe.key \
  --db /var/lib/smokeng-agent/agent.db
Restart=always
RestartSec=5
StateDirectory=smokeng-agent
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

This is the master unit from [Operations](operations.md) with a different command and state
directory; the hardening notes there apply, including the `CAP_NET_RAW` fallback. The
`smokeng` user must exist and the sysctl must be set before the first start. Add
`--irtt-hmac-keys` and `--tls-ca-file` as needed.

The token in `ExecStart` is spent on the first start; later starts use the recorded id. It
expires within 24 hours, so remove the `--token` line after the agent has reported.

## Enrolment at scale

The UI uses three routes:

- `POST /api/v1/agent-tokens` with `{"name": "...", "ttl_s": 3600}` mints a token; the
  response includes `token` once. `ttl_s` is optional and capped at 86400.
- `GET /api/v1/agent-tokens` lists tokens (never the secret).
- `DELETE /api/v1/agent-tokens/{id}` revokes one.

These routes require an admin browser session. There are no API tokens, so a script cannot
authenticate on its own; see [API](api.md). To enrol a few agents, the realistic loop is the
UI: mint a token per agent, paste the command, repeat. If you do script it, reuse an admin
session cookie from a browser, and treat that cookie as the admin credential it is.

The Ansible role installs only the master and, optionally, the local prober. It does not
install or enrol remote agents. For a handful of hosts, the `smokeng agent key` plus
`smokeng agent add` route is the one that fits a provisioning tool: both are idempotent.

## Assigning targets

`agents` is an inheritable setting listing the vantage points that measure a node and its
subtree. The default is `["local"]`, the master's own prober. Every name must belong to an
enrolled agent; one that does not is refused rather than quietly measured by nobody.

```toml
[targets."Customer"]
agents = ["local", "ams-01", "rtd-01"]   # everything below is probed from three places

[targets."Customer/only-from-ams"]
agents = ["ams-01"]                      # …except this one
```

Agents poll for their assignments every 60 seconds, so a change to the tree reaches them
without a restart.

## Managing agents

The Agents page shows the version each agent last reported. Use it to see which hosts still
predate a fix to the measurement path. The version travels in an unsigned header: it is what
an authenticated agent claims, not something to base a trust decision on.

```bash
smokeng agent list --db smokeng.db
smokeng agent disable --db smokeng.db 3   # stop accepting its submissions
smokeng agent enable  --db smokeng.db 3
smokeng agent remove  --db smokeng.db 3
```

All of this is also in the UI under **Agents**, which is the easier place to do it: it
shows when each agent last reported, and which targets it measures.

A disabled agent is refused at the door: its requests fail verification, and it keeps
buffering locally rather than losing measurements.

An agent that has reported **cannot be removed** — its measurements are labelled by agent,
and deleting it would leave a series nothing can name. Disable it instead: probing stops,
the history stays. This is the same reason targets are disabled rather than deleted.

Renaming an agent rewrites every `agents` list that referred to it, in one transaction. A
rename that left them behind would turn each of those targets into one measured by nobody.

## Clock

Agents sign every request with a timestamp, and the master refuses one more than 5 minutes
from its own clock. Run NTP on every agent host and
keep the offset well inside that window.

## Versions

The agent sends its version in an `X-Agent-Version` header, and the Agents page shows it. The
master records it and does not check it. No compatibility window is promised between master
and agent versions beyond the signed-request format (`smokeng-ingest-v1`); run matching
versions. Downgrades are not supported.

## Limits

- **Agents do no path discovery.** Path-change marks come from the master's local prober
  only; an agent uploads measurements, never paths.

## How the protocol works

Two endpoints, both Ed25519-signed:

- `GET /api/v1/agent/targets` — the agent's assignments
- `POST /api/v1/ingest` — a batch of measurements as an Arrow IPC stream

Every request carries four headers:

| Header | Contents |
| --- | --- |
| `X-Agent-Id` | The agent's numeric id |
| `X-Timestamp` | Unix seconds |
| `X-Nonce` | 16 random bytes, base64 |
| `X-Signature` | Ed25519 signature over the canonical string, base64 |

The canonical string is a newline-joined `smokeng-ingest-v1`, method, path, agent id,
timestamp, nonce, and the SHA-256 of the body. Signing the body hash means a batch cannot
be altered in flight; including the domain string means a signature cannot be replayed
against a future protocol version.

The nonce must be exactly 16 bytes of standard base64, which is what the agent has always
sent; the master keeps an accepted nonce for ten minutes, and its size is what that costs.

The master verifies in order: agent exists, agent is enabled, timestamp within ±5 minutes,
signature valid, nonce not seen before, rate limit. The two checks that keep state come
after the signature, so a stranger who only knows an agent's id cannot spend its budget,
and in that order: a replay is refused before it touches the rate budget, and a nonce is
stored only for a request that was admitted, so a rate-limited agent cannot grow the
cache.

A request older than the skew window is refused, and inside it the nonce cache refuses a
second copy. The cache is in memory and empty after a restart, so for the rest of that
window what stands between a captured request and the data is that a replay is
byte-identical: ingest is **first write wins**, keyed by (target, agent, interval start),
so storing it again changes nothing. The same rule goes further than a replay needs: a
*different* signed submission for an interval the agent has already reported changes
nothing either, so an agent cannot rewrite history it has reported. Rows timestamped later
than the clock skew allows, rows for targets not assigned to the agent, and rows claiming
more replies than probes are dropped, and the rest of the batch is still accepted:
refusing it would make the agent discard the whole batch, which is a larger loss than the
row.

A submission is refused outright (400, which makes an agent discard that batch) when it is
not the shape an agent sends: more than 10,000 rows, more than a schema message followed
by record batches, any compression, or any declared length that does not fit in the
payload. smokeng's own encoder never produces any of these.

## Buffering and back-pressure

An agent writes to a local SQLite buffer first and pushes every 15 seconds, up to 2000
measurements per batch. If the master is unreachable — maintenance, a broken tunnel, a
partitioned WAN — the agent keeps measuring and keeps buffering. When the link returns, the
backlog is delivered and the graphs fill in retroactively.

This matters more than it sounds: the moments you most want measurements from a remote
site are exactly the moments the link to it is unhealthy.

On shutdown the agent flushes its engine before draining the buffer, so the last completed
intervals are not stranded.

## Transport security

The signature authenticates the agent and protects the body's integrity; it does not
encrypt anything. Run the master behind TLS. smokeng refuses a plain-HTTP master URL
unless you pass `--insecure-allow-http`.

A master on a **literal loopback address** is the one exception and needs no flag: those
packets never reach a network interface, so there is nothing for TLS to protect them from.
That is the [prober-as-its-own-process](operations.md#running-the-prober-separately)
arrangement. A hostname that merely resolves to loopback does not qualify — the exemption
rests on the packets provably not leaving the machine.

### Certificates the agent has to trust

Two different trust relationships live here, and only one of them is the master's business.

The agent verifies the **master's** certificate the way any HTTP client does, so a master
behind an internally-issued certificate needs that CA in the agent host's system trust
store.

Separately, an agent measuring an `https` **target** verifies that target's certificate.
Those CAs **come down with the assignments**, so `--tls-ca-file` on the master is the only
place a private root has to be placed, and rotating it reaches every agent within a poll.

The set is replaced on each poll rather than added to, because withdrawing a CA has to
reach the agents too — a pool that only ever grew would keep trusting a root long after it
was retired. Anything given to the agent with its own `--tls-ca-file` survives regardless:
a local decision is not the master's to revoke.

Every certificate an agent accepts this way is logged with its subject and SHA-256
fingerprint when the set changes, so what an agent was told to trust is auditable after
the fact rather than silent:

```
probe: trusting CA from master: CN=GemeenteX Root (sha256:9f2c…)
```

`--no-remote-cas` refuses them, for an operator who would rather place every trust
decision on the agent host itself.

### Why this is narrower than it sounds

These CAs reach https **probes** and nothing else. They are never used for the agent's own
connection to the master — that verifies against the host's trust store — so a master
cannot use this to vouch for itself. There is a test that pins the separation.

What a compromised master gains is correspondingly small. It could make an agent report a
green measurement for a target that is being intercepted; but it already decides what
address that target names, so it could point the target anywhere regardless. The trust it
gains is over endpoints whose definition it already controls.

See [Configuration](probes.md#internal-certificates).

### irtt HMAC keys the agent needs

An agent measuring an HMAC-protected irtt server needs that server's key, and unlike the
CAs the master does **not** hand it down: the key is a shared secret, and assignments carry
only data, never secrets. Deploy the keyfile to the agent host and point the agent at it:

```bash
smokeng agent run --master https://… --agent-id 3 \
    --irtt-hmac-keys /var/lib/smokeng/irtt-hmac-keys.toml
```

The file has the same shape as the master's — `"host:port" = "secret"` per endpoint — and
holds only the endpoints this agent measures. Keep it in a vault and render it per host;
see [Configuration](probes.md#authenticating-to-an-irtt-server).
