# Getting started

## 1. Install

Download a build for your platform from the [releases
page](https://github.com/timdebruijn/smokeng/releases), or build from source:

```bash
git clone https://github.com/timdebruijn/smokeng.git && cd smokeng && make build
```

The result is a single static binary at `bin/smokeng`. It embeds the web UI, so there is
nothing else to deploy and no runtime dependency beyond libc.

## 2. Allow unprivileged ICMP (Linux)

smokeng does not need root and does not need a setuid helper. It uses Linux's datagram
ICMP sockets, which are gated behind a sysctl that says which groups may open them:

```bash
sudo sysctl -w net.ipv4.ping_group_range="0 2147483647"
```

Make it survive a reboot:

```bash
echo 'net.ipv4.ping_group_range = 0 2147483647' | sudo tee /etc/sysctl.d/99-smokeng.conf
```

To restrict it to one group instead, put that group's gid at both ends of the range.

If the sysctl is not set, smokeng falls back to a raw socket, which does need
`CAP_NET_RAW`. Grant it with `sudo setcap cap_net_raw+ep /usr/local/bin/smokeng` if you
prefer that route. Measurements taken over a raw socket are flagged as such in the UI.

On macOS, unprivileged datagram ICMP works out of the box; kernel timestamping does not,
so every measurement is flagged `userspace TX/RX`. macOS is fine for development, not for
production measurement.

## 3. Describe what to measure

Write a `targets.toml`:

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
smokeng config import --db smokeng.db targets.toml
```

The import is declarative and idempotent: run it as often as you like. Targets that are
in the database but absent from the file are *disabled*, not deleted, unless you pass
`--prune`. That flag exists only on the command line: importing the same file from the
web UI (**Targets → Import TOML**) goes through `PUT /api/v1/config`, which never
prunes — absence always disables there.

## 4. Run it

```bash
smokeng serve --db smokeng.db --listen 127.0.0.1:8080
```

Open <http://127.0.0.1:8080>. The first measurements appear after one interval; the smoke
becomes readable after a few.

![The overview: cards for series, firing alerts, worst loss and flagged measurements above
a per-target list with sparklines, next to a panel of firing alerts and recent alert
activity](images/overview.png)

*The overview is the landing page: one row per series with its sparkline, median, p95 and
loss, and the alert state alongside.*

smokeng refuses to listen on a non-loopback address unless you either configure OIDC (see
[Authentication](authentication.md)) or pass `--i-know-this-is-unauthenticated`. That is
deliberate: an unauthenticated instance on `0.0.0.0` lets anyone edit your targets.

## 5. Next

- Tune intervals, ping counts and probe modes: [Configuration](configuration.md)
- Understand what you are looking at: [Reading the graphs](reading-graphs.md)
- Get told when it breaks: [Alerting](alerting.md)
- Run it as a system service: [Operations](operations.md)
