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
| `@include` | Not followed. Import each included file separately. |
| `host = DYNAMIC` | Not imported. smokeng re-resolves hostnames on their DNS TTL instead, so a dynamic address needs no special mode. |
| `host = a b c` (overlay graphs) | Not imported. Multi-host overlays are a rendering feature; add the hosts as separate targets. |
| `parents = …` | Not imported. smokeng has one target tree, not alternative hierarchies. |
| `alerts = …` | Not imported. Alert rules are expressed differently — see [Alerting](alerting.md). |
| `slaves = …` | Imported as an `agents` list, but the agents must be enrolled separately first — with a one-time enrolment token or with `smokeng agent add` — or the import refuses the unknown names. See [Remote agents](agents.md). |
| `nomasterpoll` with no slaves | Warned about — the target would never be measured. |

SmokePing's `probe = …` lines are not translated, and every imported target arrives as
`icmp`. smokeng has its own probe types — `dns`, `tcp`, `http`, `https` and `irtt` as well
as `icmp` — but the names and per-probe settings do not map one to one onto SmokePing's,
and a wrong guess would measure something other than what you asked for while looking
correct. Set `probe_type` on the imported targets yourself; see
[Configuration](configuration.md#probe-types).

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
