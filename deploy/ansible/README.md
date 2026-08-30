# Deploying smokeng with Ansible

An idempotent role that installs smokeng as a systemd service on a Debian-family
host: release binary verified against the release's own `SHA256SUMS`, a system
user, unprivileged ICMP granted to that user's group alone, a hardened unit, and
an optional declarative import of the target tree.

## Requirements

- Ansible with the `ansible.posix` collection: `ansible-galaxy install -r requirements.yml`
- A Debian-family target with systemd, reachable over SSH with sudo

## Quick start

```bash
cp inventory.example.ini inventory.ini
cp group_vars/smokeng.example.yml group_vars/smokeng.yml
$EDITOR inventory.ini group_vars/smokeng.yml
ansible-galaxy install -r requirements.yml
ansible-playbook -i inventory.ini site.yml
```

`inventory.ini`, `group_vars/*.yml` (except the examples) and `targets.local.toml`
are gitignored: they name your hosts.

## What it does

| Step | Detail |
| --- | --- |
| Binary | Downloads `smokeng-linux-<arch>` for the pinned version and verifies it against the release's `SHA256SUMS`. Architecture comes from the host's facts. |
| User | System user and group, no login shell, no home. |
| ICMP | Sets `net.ipv4.ping_group_range` to the service group's gid **only**, so unprivileged ICMP is not a machine-wide capability. |
| Unit | `NoNewPrivileges`, `ProtectSystem=strict`, `ProtectHome`, `MemoryDenyWriteExecute`, and `ReadWritePaths` limited to the state directory. smokeng needs no privileges beyond that sysctl. |
| Targets | Optional: ships a `targets.toml` and imports it. The import is declarative, so it runs every play and reports a change only when it made one. |

Re-running the play changes nothing. That is the point: it is safe from cron,
CI or Semaphore.

## Variables

The full set is in [`roles/smokeng/defaults/main.yml`](roles/smokeng/defaults/main.yml).
The ones that matter:

| Variable | Default | Notes |
| --- | --- | --- |
| `smokeng_version` | `v0.7.2` | Pin it. An unattended "latest" is an unreviewed upgrade. |
| `smokeng_listen` | `127.0.0.1:8080` | |
| `smokeng_external_url` | `""` | The address agents and browsers reach this master at, when a proxy sits in front. Set it or the enrolment command names the listen address. |
| `smokeng_trusted_proxies` | `""` | CIDRs whose `X-Forwarded-For` may be believed. Log accuracy only. |
| `smokeng_allow_unauthenticated` | `false` | Required to bind off loopback without OIDC. The role refuses otherwise. |
| `smokeng_oidc_issuer` | `""` | Setting it enables authentication; see [../../docs/authentication.md](../../docs/authentication.md) |
| `smokeng_metrics_public` | `false` | Prometheus cannot present a session cookie |
| `smokeng_alert_webhook` | `""` | Empty means rules are stored but never evaluated |
| `smokeng_targets_file` | `""` | Path on the controller to a `targets.toml` |
| `smokeng_targets_prune` | `false` | Delete absent targets instead of disabling them |
| `smokeng_tls_ca_files` | `""` | PEM files on the host whose certificates https probes trust, on top of the system roots. Deploy the same file to any agent that measures an internally-signed target. |
| `smokeng_prober_enabled` | `false` | Run the prober as its own process next to the master — see below |
| `smokeng_prober_agent_name` | `local-probe` | The name a target's `agents` must list to be measured there |

Put the OIDC client secret in a vault, not in `group_vars` in the clear.

## A note on exposure

The role refuses to bind off loopback unless you have either configured OIDC or
explicitly set `smokeng_allow_unauthenticated`. An unauthenticated instance on a
routable address lets anyone who reaches it edit your targets. On a trusted LAN
that may be an acceptable trade for a test box — but it should be a decision,
which is why it has to be written down.

The durable arrangement is a TLS-terminating reverse proxy in front and OIDC
behind it.

## Splitting the prober off

`smokeng_prober_enabled: true` installs a second unit, `smokeng-prober`, that runs the
probing engine in its own process and reports to the master over loopback. The role
generates its key, enrols it, and templates the unit with the resulting agent id; every
step is idempotent, so the play can be re-run.

It changes nothing by itself. The master keeps its built-in prober under the name `local`,
and a target only measures in the new process once its `agents` setting names
`local-probe`. Why you might want that, and what it costs, is in
[../../docs/operations.md](../../docs/operations.md#running-the-prober-as-its-own-process).

## Adding a remote agent

The role installs a master. To measure the same targets from somewhere else, see
[../../docs/agents.md](../../docs/agents.md): enrol the agent's public key with
`smokeng agent add` on this host, then run `smokeng agent run` on the other one.
Each vantage point becomes its own series, never an average of the two.
