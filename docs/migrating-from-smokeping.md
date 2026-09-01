# Migrating from SmokePing

smokeng reads a SmokePing `Targets` file directly and turns it into a target tree.

```bash
smokeng config import-smokeping --dry-run /etc/smokeping/config.d/Targets
```

`--dry-run` prints the translated TOML and the warnings, and writes nothing. Read both
before you commit to the import. When it looks right:

```bash
smokeng config import-smokeping --db smokeng.db /etc/smokeping/config.d/Targets
```

Add `--also-ipv6` to create a v6 target alongside every v4 one, which is usually what you
want and what SmokePing made awkward.

## What comes across

The hierarchy, as a hierarchy — `+`, `++`, `+++` become nested groups, not a flat list.
Per-node `title`, `host` and `menu` come across, and `pings` and `step` become
`pings_per_interval` and `interval_s` at the node that set them, so your inheritance
survives the move.

Anything the importer cannot faithfully translate is reported as a warning rather than
dropped in silence. You will typically see some of these:

| SmokePing construct | What happens |
| --- | --- |
| `@include` | **Followed**, relative to the including file, so pointing the importer at your main config pulls the whole install in one command. An include cycle is broken with a warning rather than looping. |
| `probe = …` | **Mapped to a smokeng probe type** — see below. |
| `host = DYNAMIC` | Not imported. smokeng re-resolves hostnames on their DNS TTL instead, so a dynamic address needs no special mode. |
| `host = a b c` (overlay graphs) | Not imported. Multi-host overlays are a rendering feature; add the hosts as separate targets. |
| `parents = …` | Not imported. smokeng has one target tree, not alternative hierarchies. |
| `alerts = …` | Not imported. Alert rules are expressed differently — see [Alerting](alerting.md). |
| `slaves = …` | Imported as an `agents` list, but the agents must be enrolled separately first — with a one-time enrolment token or with `smokeng agent add` — or the import refuses the unknown names. See [Remote agents](agents.md). |
| `nomasterpoll` with no slaves | Warned about — the target would never be measured. |

## Probe types come across

The importer reads the `*** Probes ***` section and maps each target's probe module to a
smokeng probe type, following the subclass chain so a renamed probe still resolves:

| SmokePing module | smokeng `probe_type` | Parameters carried |
| --- | --- | --- |
| `FPing`, `FPing6` | `icmp` | — (left implicit; icmp is the default) |
| `EchoPingDNS`, `AnotherDNS`, `DNS` | `dns` | `lookup` → `dns_query`, `recordtype` → `dns_rr_type` |
| `EchoPingHttp`, `Curl` | `http` | `port` → `probe_port` |
| `EchoPingHttps` | `https` | `port` → `probe_port` |
| `TCPPing`, `EchoPingTcp` | `tcp` | `port` → `probe_port` |
| `IRTT` | `irtt` | `port` → `probe_port` |

The module name is matched loosely, so a probe you renamed — `FPingContinuous`, say — still
resolves to the type it actually measures with.

Three things need your eye afterwards. A `tcp` target with no port in the SmokePing config
is imported without one and warned about — smokeng will not measure a `tcp` target until you
set `probe_port`. An `http`/`https` probe's path is left at `/`: SmokePing hides it in
`urlformat`/`url` in ways too varied to reconstruct safely, so set `http_path` by hand where
a probe requested a specific path. And an `IRTT` target comes across as `irtt` — the same
tool, so the measurement is the same — but the graphs are arranged differently, and that
needs a paragraph of its own.

A probe module the importer does not recognise is imported as `icmp` with a warning naming
it, so nothing is measured as something it is not without saying so.

## IRTT: three SmokePing targets become one

SmokePing's IRTT probe graphs one figure per target, chosen with `metric`. A typical install
therefore has the same host three times — the round trip, `send_ipdv` and `receive_ipdv` —
and each of those is a separate graph.

They were never three measurements. All three come out of one irtt session, and importing
them as three targets made smokeng open three sessions to the same server, which collided:
one won each interval and the other two recorded as send failures. So the importer keeps the
plain target and skips the derived ones, naming each in a warning.

Nothing is lost by that. smokeng runs the one session and keeps all three distributions from
it, so the two jitter graphs come back — under the round trip, on the same time cursor,
rather than as separate targets in the tree:

```toml
[targets.'wag/Irtt']
host = "irtt.example.org"
probe_type = "irtt"
graph_series = "ipdv_send ipdv_receive"   # or "all", the default, or "" for none
```

`graph_series` is inherited like any other setting and chooses what is *drawn*. Everything
measured is stored regardless, so turning a graph on later shows the history it already has
rather than starting it from that moment.

Two differences from the SmokePing graphs are worth knowing. smokeng keeps the whole
distribution rather than a median with percentile bands, so a jitter plot is a cloud of
individual values around a zero line, and its sign is preserved: a packet that caught up
with the one before it is drawn below zero, not folded onto the same side as one that fell
behind. And smokeng does not graph absolute one-way delay, which irtt also reports. That
figure is a subtraction between two machines' wall clocks and is wrong by however far apart
they have drifted — it measures NTP as much as the network. Inter-packet delay variation is
a difference between consecutive packets on the monotonic clock, so the clock offset cancels
and the number means what it says without the two ends being synchronised.

A series needs the far end to return timestamps. An `irtt server` that does not is not an
error and does not read as zero jitter: the graph says the series was not measured.

## What is different once you are across

**History does not migrate.** RRD files hold consolidated averages, and un-consolidating
them is not possible — the data that would make a distribution was thrown away when it was
written. Keep the SmokePing instance running read-only for as long as you need its
history, and start smokeng alongside it.

**The graphs mean something else.** SmokePing draws percentile bands around a median.
smokeng draws every individual round-trip time. A band that looks noisier than the
SmokePing equivalent is usually not noise: it is the detail the percentile bands were
smoothing. See [Reading the graphs](reading-graphs.md).

**Nothing gets coarser with age.** There are no RRAs and no consolidation windows. Last
year's minute is still a minute, so a post-incident review a year later shows the same
distribution the prober recorded.

**IPv4 and IPv6 are separate targets.** They take different paths and fail independently,
so smokeng never merges them into one graph.

**Loss and latency share the plot.** SmokePing encodes loss in the colour of the median
line; smokeng gives it a dedicated rail beneath the graph, so "slow" and "lossy" stay
distinguishable.

**No fping, no setuid helper.** smokeng probes from its own process using unprivileged
datagram ICMP sockets, and takes kernel timestamps where the kernel offers them. That
removes the fork-per-interval cost, and it removes the ambiguity of a timestamp taken by a
different process than the one that sent the packet. Where it has to fall back — userspace
timestamps, a raw socket — it says so on the graph.
