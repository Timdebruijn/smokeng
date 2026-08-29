# Remote agents

A single vantage point tells you that a target is slow. Two tell you *where* it is slow.
An agent is the same binary in a different mode: it pulls its assignments from the master,
probes them itself, and pushes the measurements back.

Each agent's measurements are stored as a separate series, so a target probed from three
places gives you three plots, not one blended average.

## Enrolling an agent

**1. On the agent host**, generate a key. The first run creates one if the file is absent
and prints its public half:

```bash
smokeng agent run --master https://smokeng.example.org --agent-id 0
```

It will print the public key and exit, because agent id `0` is not enrolled yet. The
private key is written to `probe.key` (mode 0600) unless you pass `--key`.

**2. On the master**, enrol that public key under a name:

```bash
smokeng agent add --db smokeng.db --name ams-01 --pubkey <base64 from step 1>
```

This prints the agent id.

**3. Back on the agent**, run it for real with that id:

```bash
smokeng agent run \
  --master https://smokeng.example.org \
  --agent-id 3 \
  --key /etc/smokeng/probe.key \
  --db /var/lib/smokeng/agent.db
```

The name you chose in step 2 is what you use in a target's `agents` setting.

## Assigning targets

`agents` is an inheritable setting holding a space-separated list of agent names. The
default is `local`, which means the master's own prober.

```toml
[targets."Customer"]
agents = "local ams-01 rtd-01"      # everything below is probed from three places

[targets."Customer/only-from-ams"]
agents = "ams-01"                    # …except this one
```

Agents poll for their assignments every 60 seconds, so a change to the tree reaches them
without a restart.

## Managing agents

```bash
smokeng agent list --db smokeng.db
smokeng agent disable --db smokeng.db 3   # stop accepting its submissions
smokeng agent enable  --db smokeng.db 3
smokeng agent remove  --db smokeng.db 3
```

A disabled agent is refused at the door: its requests fail verification, and it keeps
buffering locally rather than losing measurements.

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

The master verifies in order: agent exists, agent is enabled, rate limit, timestamp within
±5 minutes, nonce not seen before, signature valid. The nonce cache and the skew window
stop replay in the short term; the real defence is that ingest is an **idempotent upsert**
keyed by (target, agent, interval start), so replaying a batch a week later overwrites
identical rows with identical values and changes nothing.

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
unless you pass `--insecure-allow-http`, which exists for development on a loopback
address and nothing else.
