# Remote agents

A single vantage point tells you that a target is slow. Two tell you *where* it is slow.
An agent is the same binary in a different mode: it pulls its assignments from the master,
probes them itself, and pushes the measurements back.

Each agent's measurements are stored as a separate series, so a target probed from three
places gives you three plots, not one blended average.

## Enrolling an agent

The short way, from the UI. Under **Agents**, choose a name and mint an
enrolment token. It is shown once — it is stored only as a hash, so it cannot be
shown again — together with the command to run on the agent host:

```bash
smokeng agent run --master https://smokeng.example.org --token smk_...
```

The address in that command is the master's own `--external-url` when one is
set, and the address you opened the page on otherwise. Set it whenever a proxy
sits in front: an admin reaching the master directly, past the proxy everyone
else uses, would otherwise be handed a command naming an address no agent can
resolve. The UI says which address it used when the two differ.

The agent generates its keypair if it has none, enrols itself, and records the
id it was given next to its key. A unit file may keep carrying `--token`: the
recorded id wins, so a restart does not try to spend a token that is already
spent.

Tokens are single use and short lived — one hour by default, 24 hours at most. A
reusable enrolment token is precisely the credential that ends up in a wiki.
Provisioning twenty agents means minting twenty tokens, which a loop does fine.
The token carries the name, so an agent cannot decide what to call itself:
naming is an administrative act, and an agent that named itself could claim a
name your targets are already assigned to.

The token is the only credential on that request, which is why the agent refuses
a plain-HTTP master unless it is on loopback or you pass `--insecure-allow-http`.
That refusal is not a preference about transports.

### Without a token

If you would rather not have a token in flight at all, the key can be carried by
hand. On the agent host, run it once with no token to print the public key:

```bash
smokeng agent run --master https://smokeng.example.org --agent-id 0
```

Then, on the master:

```bash
smokeng agent add --db smokeng.db --name ams-01 --pubkey <base64 from above>
```

and rerun the agent with the id that prints.

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

Each agent reports what version it is running when it submits, and the Agents
page shows it. A fleet upgrades one host at a time, and that column is how you
see which agents still predate a fix to the measurement path — not a cosmetic
question when the fix was to the timestamps themselves. The version travels in
an unsigned header on a signed request: it says what an already-authenticated
agent claims to be, and is not something to make a trust decision on.

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
unless you pass `--insecure-allow-http`.

A master on a **literal loopback address** is the one exception and needs no flag: those
packets never reach a network interface, so there is nothing for TLS to protect them from.
That is the [prober-as-its-own-process](operations.md#running-the-prober-as-its-own-process)
arrangement. A hostname that merely resolves to loopback does not qualify — the exemption
rests on the packets provably not leaving the machine.

### Certificates the agent has to trust

Two different trust relationships live here, and only one of them is the master's business.

The agent verifies the **master's** certificate the way any HTTP client does, so a master
behind an internally-issued certificate needs that CA in the agent host's system trust
store.

Separately, an agent measuring an `https` **target** verifies that target's certificate
itself, and needs the same `--tls-ca-file` the master was given:

```bash
smokeng agent run --master https://smokeng.example.org --agent-id 3 \
    --tls-ca-file /etc/ssl/certs/gemeentex-root.pem
```

The master does not distribute CAs. It could — they are public certificates, not secrets —
but an agent that trusted whatever the master sent would have a wider relationship with it
than the design gives it anywhere else: assignments are pure data, never anything that
changes what the agent trusts. See [Configuration](configuration.md#internal-certificates).
