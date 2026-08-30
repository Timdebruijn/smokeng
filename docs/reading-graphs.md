# Reading the graphs

A smokeng plot shows one target's latency over time. Every measurement interval
contributes its **entire distribution** — all twenty (or sixty, or two hundred) round-trip
times, not a summary of them.

![The graphs view: several targets stacked on a shared time axis, each showing its RTT
distribution as density smoke with a loss rail beneath, and a sidebar listing the target
tree](images/smokeng.png)

*The graphs view. Each target's smoke is its full RTT distribution over time; the sidebar
is the target tree, and the toolbar sets the time range, log scale, live follow, and the
compare overlay.*

## The smoke

Each pixel column pools every measurement whose interval overlaps that column, deposits
each individual round-trip time at its own height, and blurs the result vertically. Dense
regions are opaque; sparse ones are faint.

This is what the band actually means:

- **A thin, dark band** — every ping came back at nearly the same latency. The path is
  stable.
- **A tall, faint haze** — the round-trip times are scattered. Jitter, a congested queue,
  a wireless link, a CPU-starved middlebox.
- **A dark band with a faint tail above it** — most pings are fine and a few are slow.
  This is the shape that means "intermittent", and it is the shape an average erases.
- **Two separate bands** — the classic signature of load balancing across two paths of
  different length, or of a failover that is flapping.

The opacity is normalised per column, so a column pooling 200 samples is comparable to one
pooling 20. Density is mapped to alpha as `1 − exp(−k·fraction)`, which keeps a single
outlier visible without letting the bulk saturate to a solid block.

Crucially, the smoke never gets coarser with age. Zoom into last March at one-minute
resolution and you see the same distribution the prober recorded then. There are no
consolidation windows, no averaged-away RRAs, no "the detail expired".

## The median line

The solid line is the median of the samples pooled into that column — a true median of the
underlying round-trip times, never a median of per-interval medians. It **breaks at gaps**
rather than interpolating across them: a discontinuity means smokeng has no data there,
and it will not draw a line implying otherwise.

## The loss rail

The thin strip beneath the plot, labelled `loss`, colours packet loss per column on a
viridis scale from dark purple to yellow. Zero loss stays background-coloured, so a
healthy target has a silent rail and any mark on it is worth looking at.

Loss and latency are shown together on purpose: "slow" and "lossy" are different faults
with different causes, and the pair distinguishes them at a glance.

## Path change marks

If `trace_interval_s` is non-zero, smokeng periodically runs a TTL-limited traceroute and
stores the result **only when the path changes**. Each change is drawn as a dashed amber
line on the time axis, so "the route changed at 14:02" sits directly beside "the smoke
widened at 14:03" without switching views. Hovering names the route in force at that
instant.

Hops are found with TTL-limited probes, reading each router's address from the ICMP
time-exceeded reply on the socket error queue. That needs Linux; on other platforms no
route is recorded, rather than a wrong one.

## Interaction

- **Hover** — a crosshair follows the cursor across every plot at once, with a readout of
  the interval under it: time, median, replies received out of sent, and any ICMP error.
- **Drag** — select a time span to zoom every plot to it.
- **Range buttons** — 15m, 1h, 6h, 24h; `log` toggles between logarithmic and linear
  latency, and `live` follows the present.

Logarithmic is the default because latency is multiplicative: the difference between 1 ms
and 4 ms matters as much as the one between 100 ms and 400 ms, and a linear axis hides the
first. In linear mode the top 0.5 % is clipped so a single outlier cannot flatten
everything else.

## The detail view

Clicking a target opens it at length: the pooled median, p95 and spread (p95 − p5) over the
window on screen, computed from the same samples the plot draws, above a full-height smoke
plot and the target's effective settings, alert rules and vantage points.

![A target's detail page: median, p95 and spread figures above a large density-smoke plot,
with the target's settings and the agents measuring it alongside](images/detail.png)

*The detail view. p95 and the spread are the two numbers a median cannot give you, and the
reason the whole distribution is kept.*

## Compare

The **compare** button overlays the pooled medians of the shown targets on one axis, which
the stacked plots cannot do — they answer "what is this target doing", not "which of these
is worse". Only the median is drawn; the distribution stays in the plots below.

![The compare overlay: several targets' pooled median lines on one shared axis, each a
distinct colour, with a legend naming them](images/compare.png)

*Compare. Vantage points are never averaged — a target measured from two agents is two
lines, exactly as it is two plots.*

## Quality badges

smokeng records *how well it was able to measure* alongside *what it measured*, and shows
it beside the target name rather than leaving you to infer it. A badge with a count like
`(3/166)` affected only some intervals in view.

| Badge | Meaning |
| --- | --- |
| `userspace TX` / `userspace RX` | Timestamps were taken in userspace, not by the kernel. Scheduler jitter is included in the measurement, so the smoke is wider than the network really is. Expected on macOS; on Linux it means kernel timestamping was unavailable. |
| `raw socket` | Unprivileged datagram ICMP was unavailable, so smokeng fell back to a raw socket. Accuracy is unaffected; it means the sysctl is not set. See [Getting started](getting-started.md). |
| `dropped replies` | The kernel receive queue overflowed and discarded replies. **Some of the loss shown is smokeng's, not the network's.** |
| `clock step` | The wall clock jumped during these intervals. The affected round-trip times cannot be trusted. |
| `send refused` | The local stack would not transmit — no route, or a local firewall rule. These count as attempted and lost, so an unreachable target draws as loss instead of as an empty graph. |
| ICMP error | The target or a router answered with an ICMP error rather than an echo reply. The specific error (host unreachable, admin prohibited, TTL exceeded) appears in the hover readout. |

The point of these badges is that a widened band always has an attributable cause. If
smokeng degraded its own measurement, it says so, on the graph, next to the data it
affected — rather than letting you spend an afternoon debugging a router that was fine.
