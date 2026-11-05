# Deploying smokeng with Ansible

An idempotent role that installs smokeng as a systemd service on a Debian-family
host: release binary checked against the release's own `SHA256SUMS`, a system
user, unprivileged ICMP granted to that user's group alone, a hardened unit, and
an optional declarative import of the target tree.

## Requirements

- Ansible with the `ansible.posix` collection: `ansible-galaxy install -r requirements.yml`
- A Debian-family target with systemd, reachable over SSH with sudo

## Quick start

```bash
cp inventory.example.ini inventory.ini
cp group_vars/smokeng.example.yml group_vars/home.yml
$EDITOR inventory.ini group_vars/home.yml
ansible-galaxy install -r requirements.yml
ansible-playbook -i inventory.ini site.yml
```

Check it works. On the host:

```bash
systemctl status smokeng
curl -sS -m5 http://127.0.0.1:8080/healthz
sysctl net.ipv4.ping_group_range
```

`healthz` answers `ok` once the process serves HTTP (use your `smokeng_listen` port). The
sysctl must show the service group's gid twice. Then open the UI; the first measurement
appears after one interval. If you set `smokeng_targets_file`, the targets are there already.

`inventory.ini`, `group_vars/*.yml` (except the examples) and `targets.local.toml`
are gitignored: they name your hosts.

## What it does

| Step | Detail |
| --- | --- |
| Binary | Downloads `smokeng-linux-<arch>` for the pinned version and checks its SHA-256 against the release's `SHA256SUMS`. That list comes from the same release, so it catches a broken download, not a tampered release. Architecture comes from the host's facts. |
| User | System user and group, no login shell, no home. |
| ICMP | Sets `net.ipv4.ping_group_range` to the service group's gid **only**, so unprivileged ICMP is not a machine-wide capability. |
| Unit | `NoNewPrivileges`, `ProtectSystem=strict`, `ProtectHome`, `MemoryDenyWriteExecute`, no capabilities, the kernel, clock, hostname and proc protections, and `ReadWritePaths` limited to the state directory; see [the units](#the-systemd-units). smokeng needs no privileges beyond that sysctl. |
| Targets | Optional: ships a `targets.toml` and imports it. The import is declarative, so it runs every play and reports a change only when it made one. |

Re-running the play changes nothing. That is the point: it is safe from cron,
CI or Semaphore.

## Variables

The full set is in [`roles/smokeng/defaults/main.yml`](roles/smokeng/defaults/main.yml).
The ones that matter:

| Variable | Default | Notes |
| --- | --- | --- |
| `smokeng_version` | see [`defaults/main.yml`](roles/smokeng/defaults/main.yml) | Pin it. Moving it upgrades the host on the next run; see [Upgrades](#upgrades). |
| `smokeng_repo` | `Timdebruijn/smokeng` | GitHub repository the release is downloaded from. |
| `smokeng_user`, `smokeng_group` | `smokeng` | The service user and group. |
| `smokeng_bin` | `/usr/local/bin/smokeng` | |
| `smokeng_state_dir` | `/var/lib/smokeng` | Holds the database and every secret file the role writes. |
| `smokeng_db` | `<state_dir>/smokeng.db` | |
| `smokeng_listen` | `127.0.0.1:8080` | |
| `smokeng_external_url` | `""` | The address agents and browsers reach this master at, when a proxy sits in front. Set it or the enrolment command names the listen address. |
| `smokeng_trusted_proxies` | `""` | CIDRs whose `X-Forwarded-For` may be believed. Log accuracy only. |
| `smokeng_allow_unauthenticated` | `false` | Required to bind off loopback without OIDC. The role refuses otherwise. |
| `smokeng_oidc_issuer` | `""` | Setting it enables authentication; see [../../docs/authentication.md](../../docs/authentication.md). |
| `smokeng_oidc_client_id` | `""` | |
| `smokeng_oidc_client_secret` | `""` | Vault this. Written to a service-owned 0600 file and passed as `--oidc-client-secret-file`, never on the command line. |
| `smokeng_oidc_client_secret_file` | `<state_dir>/oidc-client-secret` | Where that file goes. |
| `smokeng_oidc_redirect_url` | `""` | Overrides the redirect URL; empty derives it from `smokeng_external_url`. |
| `smokeng_oidc_admin_claim` | `groups` | The claim that carries the groups. |
| `smokeng_oidc_admin_value` | `""` | Group granting admin. With OIDC on, set this or `smokeng_oidc_everyone_is_admin`; the role refuses otherwise. |
| `smokeng_oidc_everyone_is_admin` | `false` | Every user the identity provider authenticates is an admin. An explicit choice; not together with an admin group. See [version notes](#version-notes). |
| `smokeng_default_role` | `viewer` | What an authenticated user with no grant may do. Grants restrict nothing until this is `none`; see [../../docs/access-control.md](../../docs/access-control.md). |
| `smokeng_metrics_public` | `false` | Prometheus cannot present a session cookie. |
| `smokeng_alert_webhook` | `""` | Where firing and resolved alerts are posted. Empty means rules are evaluated and recorded but nothing is posted. Vault it if the URL carries a token; see [version notes](#version-notes). |
| `smokeng_alert_webhook_file` | `<state_dir>/alert-webhook` | Where the URL is written (0600). |
| `smokeng_targets_file` | `""` | Path on the controller to a `targets.toml`. |
| `smokeng_targets_prune` | `false` | Delete absent targets instead of disabling them. |
| `smokeng_tls_ca_files` | `""` | Comma-separated PEM files on the host whose certificates https probes trust, on top of the system roots. Deploy the same file to any agent that measures an internally-signed target. |
| `smokeng_irtt_hmac_keys` | `{}` | Map of `"host:port"` to the shared HMAC secret of an irtt server that requires one. Vault the values. Rendered to a 0600 keyfile on this host only; remote agents need their own entries. |
| `smokeng_irtt_hmac_keyfile` | `<state_dir>/irtt-hmac-keys.toml` | Where that keyfile goes. |
| `smokeng_ping_group_range` | `""` | Empty narrows `ping_group_range` to the service group's gid. Set a literal `"low high"` pair to widen it. |
| `smokeng_rmem_max` | `4194304` | Value for `net.core.rmem_max`, the ceiling for the 4 MiB receive buffer the prober asks for. `0` skips the task; it does not undo an earlier run. See [../../docs/operations.md](../../docs/operations.md). |
| `smokeng_prober_enabled` | `false` | Run the prober as its own process next to the master; see [below](#splitting-the-prober-off). |
| `smokeng_prober_agent_name` | `local-probe` | The name a target's `agents` must list to be measured there. |
| `smokeng_prober_master_url`, `smokeng_prober_key`, `smokeng_prober_db` | loopback URL on the listen port, files in `<state_dir>` | Where the prober reaches the master and keeps its key and database. |

### Version notes

- **`smokeng_oidc_everyone_is_admin`:** rendered as `--oidc-everyone-is-admin` for v0.22.0
  or later. An older smokeng has no such flag and already behaves that way when the group is
  empty.
- **`smokeng_alert_webhook`:** with v0.22.0 or later the URL is written to a service-owned
  0600 file and passed as `--alert-webhook-file`. An older pin has no such flag, so the role
  falls back to `--alert-webhook`, which is visible in the process list.

Put the OIDC client secret in a vault, not in `group_vars` in the clear.

## Upgrades

Change `smokeng_version` and run the play. The role replaces the binary and restarts the
service, which migrates the database on start. Migrations do not go back, and **the role
does not back up the database first.** Take the backup before the play, on the host:

```bash
sudo -u smokeng sh -c 'umask 077 && sqlite3 /var/lib/smokeng/smokeng.db ".backup /var/lib/smokeng/pre-upgrade.db"'
```

Without the `sqlite3` CLI, use Python's online backup:

```bash
sudo -u smokeng sh -c 'umask 077 && python3 -c "import sqlite3; s=sqlite3.connect(\"/var/lib/smokeng/smokeng.db\"); d=sqlite3.connect(\"/var/lib/smokeng/pre-upgrade.db\"); s.backup(d)"'
```

Do not `cp` the file while smokeng runs. The copy holds the session signing key, so keep it
private and move it off the host. More in [../../docs/operations.md](../../docs/operations.md#backups).

The role verifies the SHA-256 checksum only. For a check that ties the binary to the release
workflow, run this on a downloaded copy before you change the pin:

```bash
gh attestation verify smokeng-linux-amd64 \
  --repo Timdebruijn/smokeng \
  --signer-workflow Timdebruijn/smokeng/.github/workflows/release.yml
```

## Uninstalling

The role has no uninstall. To remove smokeng, stop and disable `smokeng` (and
`smokeng-prober`), then delete by hand what the role left behind: the units in
`/etc/systemd/system/`, the binary, the `smokeng` user and group, and
`/etc/sysctl.d/99-smokeng.conf`. That file holds the `ping_group_range` and `rmem_max`
settings, and the live values last until a reboot. `/var/lib/smokeng` holds the database,
the session key and the secret files; delete it only when you no longer want the data.

## The systemd units

Both units (`smokeng.service` and the prober's) carry the same hardening, listed with the
reasons in the templates and in [../../docs/operations.md](../../docs/operations.md#hardening-and-what-was-left-out).
If you replace unprivileged ICMP with the `CAP_NET_RAW` fallback, the unit's empty
`CapabilityBoundingSet=` has to become `CAP_NET_RAW` as well: otherwise the ambient
capability is dropped without an error and ICMP fails when the socket is opened.

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
