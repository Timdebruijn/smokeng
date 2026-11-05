# Getting started

For someone installing smokeng on one Linux host for the first time. It answers: how do I
get a first measurement on screen?

## 1. Install

Download the build for your platform from the [releases
page](https://github.com/Timdebruijn/smokeng/releases), or build from source:

```bash
git clone https://github.com/Timdebruijn/smokeng.git && cd smokeng && make build
```

The result is a single static binary (`CGO_ENABLED=0`) at `bin/smokeng`. It embeds the web
UI, so there is nothing else to deploy and no library to install.

Verify a downloaded binary before you run it. `SHA256SUMS` comes from the same release as
the binary, so it only catches a broken download. The attestation ties the file to the
release workflow; it needs `gh` logged in (see [SECURITY.md](../SECURITY.md)):

```bash
gh attestation verify smokeng-linux-amd64 \
  --repo Timdebruijn/smokeng \
  --signer-workflow Timdebruijn/smokeng/.github/workflows/release.yml
```

Then create the service user, install the binary and make the state directory:

```bash
sudo useradd --system --user-group --home-dir /var/lib/smokeng --shell /usr/sbin/nologin smokeng
sudo install -m 0755 -o root -g root smokeng-linux-amd64 /usr/local/bin/smokeng
sudo install -d -m 0750 -o smokeng -g smokeng /var/lib/smokeng
```

## 2. Allow unprivileged ICMP (Linux)

smokeng needs neither root nor a setuid helper. It opens datagram ICMP sockets, and a
sysctl decides which groups may. Grant it to the service group only:

```bash
GID=$(getent group smokeng | cut -d: -f3)
echo "net.ipv4.ping_group_range = $GID $GID" | sudo tee /etc/sysctl.d/99-smokeng.conf
sudo sysctl --system
```

Check it:

```bash
sysctl net.ipv4.ping_group_range
```

It must print the smokeng gid twice, or a wider range that contains it. If it does not,
smokeng logs `cannot open v4 ICMP socket` at start, or, where a raw socket is allowed,
flags every measurement as taken over a raw socket. `"0 2147483647"` also works, but it
lets every user on the host open ICMP sockets.

Do not use `setcap cap_net_raw` instead. The hardened unit drops all capabilities, so the
file capability does nothing there. [Operations](operations.md#running-as-a-service)
explains what the raw-socket route needs.

On macOS, unprivileged datagram ICMP works out of the box; kernel timestamping does not,
so every measurement is flagged `userspace TX/RX`. macOS is fine for development, not for
production measurement.

## 3. Describe what to measure

Write a `targets.toml` that the service user can read, for example in `/var/lib/smokeng`:

```toml
[defaults]
interval_s = 60
pings_per_interval = 20

[targets."Internet"]
title = "Internet"

[targets."Internet/cloudflare-v4"]
host = "1.1.1.1"
address_family = "v4"

[targets."Internet/quad9-v4"]
host = "9.9.9.9"
address_family = "v4"
```

Load it into the database:

```bash
sudo -u smokeng smokeng config import --db /var/lib/smokeng/smokeng.db /var/lib/smokeng/targets.toml
```

The import is declarative and idempotent: run it as often as you like. Targets that are
in the database but absent from the file are *disabled*, not deleted, unless you pass
`--prune`. That flag exists only on the command line: importing the same file from the
web UI (**Targets → Import TOML**) goes through `PUT /api/v1/config`, which never
prunes — absence always disables there.

## 4. Run it

Install the systemd unit from [Operations](operations.md#running-as-a-service), then:

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now smokeng
```

To try it in the foreground first, run
`sudo -u smokeng smokeng serve --db /var/lib/smokeng/smokeng.db`. `--listen` defaults to
`127.0.0.1:8080`.

smokeng refuses to listen on a non-loopback address unless you either configure OIDC (see
[Authentication](authentication.md)) or pass `--i-know-this-is-unauthenticated`. That is
deliberate: an unauthenticated instance on `0.0.0.0` lets anyone edit your targets.

## 5. Check it works

```bash
systemctl status smokeng
curl -sS -m5 http://127.0.0.1:8080/healthz
```

`/healthz` prints `ok` once the process serves HTTP. It says nothing about whether probing
works. Open <http://127.0.0.1:8080>: the first measurement appears on the overview after one
interval (60 s with the file above), and the smoke becomes readable after a few. If the
rows stay empty, read `journalctl -u smokeng` and recheck step 2.

![The overview: cards for series, firing alerts, worst loss and flagged measurements above
a per-target list with sparklines, next to a panel of firing alerts and recent alert
activity](images/overview.png)

*The overview is the landing page: one row per series with its sparkline, median, p95 and
loss, and the alert state alongside.*

## 6. Next

- Tune intervals, ping counts and probe modes: [Configuration](configuration.md)
- Understand what you are looking at: [Reading the graphs](reading-graphs.md)
- Get told when it breaks: [Alerting](alerting.md)
- Run it as a system service: [Operations](operations.md)
