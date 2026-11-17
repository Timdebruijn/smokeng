# Command-line reference

For operators and anyone writing a unit file or playbook. It answers: which subcommands exist,
which flags each takes, and what the defaults are.

There is one binary, `smokeng`. Six commands: `serve`, `config`, `agent`, `alerts`, `diag` and `version`.

- **No configuration file and no environment variables.** The binary never reads either. Run
  settings are flags. The target tree and everything else lives in the SQLite database
  (`--db`). The target tree is edited in the UI or imported from TOML, see
  [Configuration](configuration.md).
- **Secrets are never required on the command line.** A command line is readable by every local
  user, so each secret flag has a `-file` variant that reads the value from a file. smokeng logs
  a warning if such a file is readable by group or others; fix it with `chmod 600`.
- **Go flag syntax.** `-flag` and `--flag` are equal, and `--flag=value` works. Flags go before
  positional arguments (`config import --prune FILE`, not `FILE --prune`).
- **`-h` prints the flags** of any subcommand and exits without running it.

Legend used below: **secret** means the value is a credential, so use the `-file` variant where
one exists. **dangerous** means the flag removes a safety check or cannot be undone.

```
smokeng serve [flags]
smokeng config import [--db PATH] [--prune] [--allow-unknown-agents] FILE
smokeng config import-smokeping [--db PATH] [--also-ipv6] [--include-root DIRS] [--dry-run] [--prune] FILE
smokeng config export [--db PATH]
smokeng agent add --name NAME --pubkey KEY [--db PATH]
smokeng agent list [--db PATH]
smokeng agent enable|disable|remove [--db PATH] ID
smokeng agent key [--key PATH]
smokeng agent run --master URL [--agent-id ID | --token TOKEN] [flags]
smokeng alerts replay [--db PATH] (--rule NAME | --rules-file FILE [--rule NAME]) (--since 30d | --from TIME --to TIME) [flags]
smokeng diag loss-pattern [--db PATH] --target PATH [--vantage NAME] (--since 24h | --from TIME --to TIME) [--format text|json]
smokeng version
```

There is no `probe` subcommand. Probing happens inside `serve` (on the master) and
`agent run` (on a remote node).

## serve

Runs the master: prober, API and web UI, in one process. It stops cleanly on SIGINT and
SIGTERM.

```
smokeng serve --db /var/lib/smokeng/smokeng.db --listen 127.0.0.1:8080
```

### Listen, proxy and TLS

smokeng serves plain HTTP. It has no TLS flags. Terminate TLS in a reverse proxy and set
`--external-url`. See [Operations](operations.md).

| Flag | Default | Meaning |
| --- | --- | --- |
| `--listen` | `127.0.0.1:8080` | Address to listen on. |
| `--external-url` | empty | Address others reach the master at, e.g. `https://smokeng.example.org`. Absolute URL, no path. Set it behind a proxy: the enrolment command in the UI and the default OIDC redirect URL are built from it. |
| `--trusted-proxies` | empty | Comma-separated CIDRs whose `X-Forwarded-For` is believed. Affects log lines only; nothing is authorised on a client address. |
| `--i-know-this-is-unauthenticated` | `false` | **Dangerous.** Allows a non-loopback `--listen` without `--oidc-issuer`. Everyone who can reach the port can use the UI and API. |

Without `--oidc-issuer`, `serve` refuses to start on a non-loopback `--listen` unless the last
flag is given.

### Storage and retention

| Flag | Default | Meaning |
| --- | --- | --- |
| `--db` | `smokeng.db` | Path to the SQLite database. Created if absent. |
| `--retention-interval` | `1h` | How often measurements past each target's `retention_s` are pruned. Must be positive. Retention itself is per target, see [Configuration](configuration.md). |

### Authentication and OIDC

Setting `--oidc-issuer` turns authentication on. Background and provider setup:
[Authentication](authentication.md); roles and grants: [Access control](access-control.md).

| Flag | Default | Meaning |
| --- | --- | --- |
| `--oidc-issuer` | empty | OIDC issuer URL. Enables authentication. |
| `--oidc-client-id` | empty | OIDC client id. |
| `--oidc-client-secret` | empty | **Secret.** OIDC client secret. Use `--oidc-client-secret-file` instead. |
| `--oidc-client-secret-file` | empty | File holding the client secret (whitespace trimmed; must not be empty). Mutually exclusive with `--oidc-client-secret`. |
| `--oidc-redirect-url` | derived | Redirect URL. Default: `<external-url>/auth/callback`, or `http://<listen>/auth/callback` without `--external-url`. |
| `--oidc-admin-claim` | `groups` | ID-token claim that lists the user's groups. |
| `--oidc-admin-value` | empty | Group whose members are administrators. Required with `--oidc-issuer` unless `--oidc-everyone-is-admin` is given. |
| `--oidc-everyone-is-admin` | `false` | **Dangerous.** Every user the identity provider lets in is an administrator. Mutually exclusive with `--oidc-admin-value`; an error without `--oidc-issuer`. |
| `--default-role` | `viewer` | What an authenticated user with no grant may do: `viewer` or `none`. Any other value is refused. |

### Alerting

How rules, states and the payload work: [Alerting](alerting.md). Without a webhook, alerts are
evaluated and shown but not delivered.

| Flag | Default | Meaning |
| --- | --- | --- |
| `--alert-webhook` | empty | **Secret** (the URL is often a credential). Alertmanager v2 endpoint to POST firing and resolved alerts to. Use `--alert-webhook-file`; the process list shows this one. |
| `--alert-webhook-file` | empty | File holding the webhook URL. Mutually exclusive with `--alert-webhook`. Must be an `http` or `https` URL. |
| `--alert-repeat` | `1m` | How often still-firing alerts are re-posted so Alertmanager does not expire them. Used only when a webhook is set. |

### Metrics

| Flag | Default | Meaning |
| --- | --- | --- |
| `--metrics-public` | `false` | Serve `/metrics` without a session so Prometheus can scrape it. Anyone who can reach the port can read it. |

### Prober

| Flag | Default | Meaning |
| --- | --- | --- |
| `--tls-ca-file` | empty | Comma-separated PEM files that `https` probes trust in addition to the system roots. Keeps verification on, unlike a target's `tls_skip_verify`. |
| `--irtt-hmac-keys` | empty | **Secret file.** TOML map of irtt `"host:port"` to its shared HMAC secret. The secrets stay in this file and never enter the tree, the API or an export. |

Probe behaviour, CA handling and the HMAC file format: [Probes](probes.md).

## config

Reads or writes the target tree in the database. Run these on the master host. They open the
database directly, so a running `serve` is not needed. Format and fields:
[Configuration](configuration.md); moving off SmokePing:
[Migrating from SmokePing](migrating-from-smokeping.md).

All `config` subcommands share one flag set. A flag a subcommand does not use is accepted and
ignored.

### config import

Syncs the target tree from a TOML file. Prints a summary; warnings go to stderr.

| Flag | Default | Meaning |
| --- | --- | --- |
| `--db` | `smokeng.db` | Path to the SQLite database. |
| `--prune` | `false` | **Dangerous.** Delete targets absent from the file. Without it they are disabled instead. |
| `--allow-unknown-agents` | `false` | Accept `agents` entries naming no enrolled agent, reporting them as warnings. See [Agents](agents.md). |

### config import-smokeping

Translates a SmokePing `Targets` file and imports it. `@include` is followed relative to the
file. Unsupported SmokePing settings are reported on stderr. Unknown slaves are always accepted,
since they cannot be enrolled yet.

| Flag | Default | Meaning |
| --- | --- | --- |
| `--db` | `smokeng.db` | Path to the SQLite database. |
| `--dry-run` | `false` | Print the translated TOML to stdout and write nothing. |
| `--also-ipv6` | `false` | Also create a v6 target for every hostname (address families are separate targets). |
| `--include-root` | empty | Comma-separated directories, besides the file's own, that `@include` may read from. |
| `--prune` | `false` | **Dangerous.** Delete targets absent from the translated config instead of disabling them. |

### config export

Writes the target tree as TOML to stdout. Only `--db` applies. The export holds no secrets.

## agent

Enrols and runs remote measurement nodes. Concepts and the enrolment flow:
[Agents](agents.md). `add`, `list`, `enable`, `disable` and `remove` run on the master host
and act on its database. `key` and `run` run on the agent host.

### agent add

Enrols an agent by its public key. Prints the new id alone on stdout; messages go to stderr.
Idempotent: the same name with the same key prints the existing id. The same name with a
different key is refused, because re-keying orphans the old key's measurements. Remove the
agent first if that is intended.

| Flag | Default | Meaning |
| --- | --- | --- |
| `--name` | required | Agent name, as used in a target's `agents` setting. |
| `--pubkey` | required | Base64 public key, as printed by `agent key` or `agent run`. |
| `--db` | `smokeng.db` | Path to the SQLite database. |

### agent list

Prints enrolled agents with their id, name, state and last-seen time. Flag: `--db`
(default `smokeng.db`).

### agent enable, disable, remove

Take the agent id as the one positional argument. Flag: `--db` (default `smokeng.db`).

- `disable`: the master rejects its submissions. `enable` reverses it.
- `remove`: deletes the agent. Refused if the agent has ever reported a measurement
  (`ErrAgentHasHistory`); use `disable` to retire one with history. The local agent cannot
  be removed. Re-enrolling needs a new `agent add`.

### agent key

Creates the agent's private key if it does not exist and prints the public key alone on
stdout. Safe to re-run: an existing key is printed and left unchanged. Useful in provisioning,
where the key is needed before the unit that would print it.

| Flag | Default | Meaning |
| --- | --- | --- |
| `--key` | `probe.key` | Path to the agent's private key. |

### agent run

Runs the agent: probes locally, buffers in a local database, pushes to the master. It stops
cleanly on SIGINT and SIGTERM and makes a last attempt to drain the buffer first (up to 30 s).
It prints its public key at start.

Needs `--master` plus either `--agent-id` or `--token`. Once enrolled, the id is stored next to
the key file, so `--token` is spent once and later restarts do not need it.

| Flag | Default | Meaning |
| --- | --- | --- |
| `--master` | empty | Master base URL, e.g. `https://smokeng.example.org`. |
| `--agent-id` | `0` | This agent's id, from `agent add`. Read from the stored id if omitted. |
| `--token` | empty | **Secret.** One-time enrolment token minted in the UI under Agents. Enrols the agent and records its id. There is no `-file` variant; it is single-use, so remove it from the unit file after the first start. |
| `--key` | `probe.key` | Path to the agent's private key. Created if absent. |
| `--db` | `smokeng-agent.db` | Local buffer database. |
| `--insecure-allow-http` | `false` | **Dangerous.** Allow a plain-HTTP master URL that is not on loopback. The enrolment token travels in the clear. Loopback needs no flag. |
| `--tls-ca-file` | empty | Comma-separated PEM files that `https` probes trust, in addition to the system roots and the CAs the master hands down. |
| `--irtt-hmac-keys` | empty | **Secret file.** Same format as on the master. The master does not hand these down; deploy the file to the agent host. |
| `--no-remote-cas` | `false` | Refuse the CA certificates the master supplies. They only reach `https` probes, never the agent's own connection to the master. |

## alerts

Reads alert history. Only `replay` exists.

### alerts replay

Replays an alert rule, one stored in the database or one in a TOML file that is not imported,
over stored measurements, and reports when it would have fired and resolved. What the output
means, and a worked example: [Testing a rule before you enable it](alerting.md#testing-a-rule-before-you-enable-it).

It opens the database **read-only**: it writes nothing, does not migrate and does not create
the file, so it is safe to run next to a running `serve`. It refuses a database whose schema
version differs from the binary's, so run it with the same smokeng version as the server.
Measurements the server writes while it runs may or may not be in the window.

```
smokeng alerts replay --db smokeng.db --rules-file rules.toml --since 30d
smokeng alerts replay --db smokeng.db --rule loss --from 2026-11-01T00:00:00Z --to 2026-11-08T00:00:00Z --format csv
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `--db` | `smokeng.db` | Path to the SQLite database. Must exist. |
| `--rule` | empty | Name of the rule to replay. Required unless `--rules-file` is given. A name is unique per node, not globally: every stored rule with this name is replayed. |
| `--rules-file` | empty | Replay the alert rules of this TOML file instead of the stored ones. Only the alert rules are read; settings, hosts and grants in the file are ignored. With `--rule`, only that rule. |
| `--since` | empty | Window ending now: a number with `d`, `h` or `m` (`30d`, `36h`, `90m`). |
| `--from`, `--to` | empty | Window `[from, to)` as RFC 3339 times. Give `--since`, or both of these, not a mix. |
| `--target` | empty | Only targets under this node, written as tree paths are (`/Production/web`). An unknown path is an error. |
| `--vantage` | empty | Only this agent's measurements; `local` is the master's own prober. An unknown name is an error. |
| `--silences` | `report` | `report` lists every episode and marks silenced ones, `respect` leaves out episodes whose fire was silenced (counting them as suppressed), `ignore` does not consult silences. A silence counts if it covered the transition at its own time, not now. |
| `--format` | `text` | `text`, `json` or `csv`. |

Output goes to stdout. Warnings are written to stderr in every format; `text` and `json` carry
them in the report as well. `-h` prints the flags.

`json` is the whole report as indented JSON with `"schema": 1`: `from`, `to`, `silences`,
`series`, `totals`, `warnings` and `notes`. Timestamps are RFC 3339 UTC. Each series carries
`rule`, `rule_node`, `condition`, `target`, `target_id`, `vantage`, `agent_id`, `intervals`,
`fired`, `resolved`, `firing_seconds`, `longest_seconds`, `episodes` and `warnings`; each episode
`start`, `end` (absent while still firing), `open`, `seconds`, `crossed_at`, `peak`,
`fire_silenced` and `resolve_silenced`. `totals.suppressed` counts the episodes whose fire fell
under a silence: listed and marked under `report`, left out under `respect`, always 0 under
`ignore`.

`csv` has one row per episode, with the header
`rule,rule_node,target,vantage,start,end,seconds,crossed_at,peak,open,fire_silenced,resolve_silenced`
(`end` is empty for an episode still firing).

## diag

Looks at stored measurements for a cause. Only `loss-pattern` exists.

### diag loss-pattern

For one target, shows after the fact whether its loss sits at particular places in the send
order, which is what ICMP rate limiting at the target looks like, or is spread the way network
loss is. It reads stored measurements and sends nothing. What the output means and what it
cannot see: [Loss on a graph that might be rate limiting](troubleshooting.md#loss-on-a-graph-that-might-be-rate-limiting).

It opens the database **read-only**, like `alerts replay`: it writes nothing, does not migrate
and does not create the file, so it is safe to run next to a running `serve`. It refuses a
database whose schema version differs from the binary's, so run it with the same smokeng
version as the server.

```
smokeng diag loss-pattern --db smokeng.db --target /Production/web --since 24h
smokeng diag loss-pattern --db smokeng.db --target Production/web --vantage ams-01 --from 2026-11-16T00:00:00Z --to 2026-11-17T00:00:00Z --format json
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `--db` | `smokeng.db` | Path to the SQLite database. Must exist. |
| `--target` | empty | Required. The target as a tree path (`/Production/web`, the leading `/` is optional). It must be a target with a host: a group or an unknown path is an error. |
| `--vantage` | empty | Only this agent's measurements; `local` is the master's own prober. An unknown name is an error. Without it, every agent with measurements for the target in the window gets its own section, in name order. |
| `--since` | empty | Window ending now: a number with `d`, `h` or `m` (`24h`, `7d`, `90m`). |
| `--from`, `--to` | empty | Window `[from, to)` as RFC 3339 times. Give `--since`, or both of these, not a mix. |
| `--format` | `text` | `text` or `json`. |

Output goes to stdout; `-h` prints the flags. A target with no measurements in the window is
reported as such and is not an error.

`text` has one section per agent: the interval counts, what the verdict was based on, a
histogram of losses per send position (positions numbered from 1; past 40 probes several
positions share a row), the p-value, the last 20 intervals with recorded loss as a pattern in
send order (`.` answered, `x` lost; cut after 80 positions), and a one- or two-sentence
conclusion.

`json` is one indented object with `"schema": 1`: `target`, `from`, `to` and `series`.
Timestamps are RFC 3339 UTC. Each series carries `vantage`, `agent_id`, `intervals`,
`with_loss`, `recorded`, `verdict`, `recent` and `conclusion` (the sentence the text form
ends with). `verdict` has `tested`, `why`, `flagged`, `p`, `p_at_least`, `probes`, `mode`
(`burst` or `spread`), `window_intervals`, `lost`, `counts` (per position), `window_from`,
`window_to` (null when no interval was judged) and `where`. `recent` is a list of `ts` and the
full `pattern`, not cut at 80.

## version

`smokeng version` prints `smokeng <version> <os>/<arch> (<go version>)`. A build without a
stamped version says `dev`.

## Exit codes

The code defines no named exit codes. In practice:

- `2`: no or unknown command (the usage text is printed to stderr).
- `1`: any other failure (the error is logged to stderr).
- `0`: success, including a clean shutdown of `serve` or `agent run` on SIGINT or SIGTERM.
