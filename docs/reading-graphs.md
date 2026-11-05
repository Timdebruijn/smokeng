# Reading the graphs

A smokeng plot shows one target's latency over time. Every measurement interval
contributes its **entire distribution** — all twenty (or sixty, or two hundred) round-trip
times, not a summary of them.

![The graphs view: several targets stacked on a shared time axis, each showing its RTT
distribution as density smoke with a loss rail beneath, and a sidebar listing the target
tree](images/graphs.png)

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

The smoke never gets coarser with age. A minute recorded last March is still a minute, with
the same distribution the prober recorded then. There are no consolidation windows and no
averaged-away RRAs. Retention, if you set it, deletes old intervals whole; see
[Operations](operations.md).

A window longer than the plot is wide — a week, a month — holds more intervals than there are
pixel columns, so each column pools every interval that starts in it. The server does that
pooling when it reads the window, from the stored intervals; nothing coarser is kept. See
[Long windows](#long-windows).

### What N means

Each interval holds `pings_per_interval` samples, 20 by default. Read the picture with that
number in mind:

- With 20 samples, one lost packet is 5 % loss. Two is 10 %. A single stray ping is not a
  trend.
- The p5 and p95 figures are order statistics of the pooled samples (the value at position
  `floor(p × N)` in the sorted list, no interpolation). With 20 samples, p95 is the 20th
  value of 20, so it is the maximum. It becomes a stable number only over many intervals,
  which is why the detail view pools the whole window.
- A short window pools few samples. 15 minutes at the default 60 s step is 300 samples; a
  1 % loss claim needs more than that.

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

### What the rail counts

Loss is `1 − received / sent` per interval. Probes that never went out because of a local
failure are still drawn as loss, with a `not sent` badge naming the reason (see the badges
below), so an unreachable target is not an empty graph. Loss under 0.1 % is shown as
`<0.1%`, never as `0.0%`. What counts as lost for each probe type is in [Probes](probes.md).

## Path change marks

If `trace_interval_s` is non-zero, smokeng periodically runs a TTL-limited traceroute and
stores the result **only when the hop list differs from the last one stored**. Each change is
drawn as a dashed amber line on the time axis, so "the route changed at 14:02" sits directly
beside "the smoke widened at 14:03" without switching views. Hovering names the route in
force at that instant.

A mark says the hop list differed. It does not say the new path is worse, and a hop that
stopped answering traceroute can look like a change. Only the master's own prober records
paths. A target seen from a remote agent has no marks, because remote agents do not run
traceroute.

Hops are found with TTL-limited probes, reading each router's address from the ICMP
time-exceeded reply on the socket error queue. That needs Linux; on other platforms no
route is recorded, rather than a wrong one.

## Interaction

- **Hover** — a crosshair follows the cursor across every plot at once, with a readout of
  the interval under it: time, median, replies received out of sent, and any ICMP error. On a
  long window it is the pixel column under it instead; see [Long windows](#long-windows).
- **Drag** — on the graphs view, select a time span to zoom every plot to it (at least five
  seconds). `reset zoom` returns to the trailing window. The detail view does not take a
  drag; use its range buttons.
- **Range buttons** — 15m, 1h, 6h, 24h, 7d, 30d; `log` toggles between logarithmic and
  linear latency, and `live` follows the present. A trailing window refreshes every ten
  seconds, or about once per pixel column when that is longer: a month every 43 minutes.
- **custom** — type a start and end. Any window, as far back as the data goes.
- **zoom out** — twice as much time around the same centre. A window that would reach past
  now becomes a live, trailing one.

Logarithmic is the default because latency is multiplicative: the difference between 1 ms
and 4 ms matters as much as the one between 100 ms and 400 ms, and a linear axis hides the
first. In linear mode the top 0.5 % is clipped so a single outlier cannot flatten
everything else.

### Long windows

When a window holds more intervals than the plot has pixel columns, each column pools the
intervals that start in it:

- the smoke is drawn from the pooled samples (up to 256 of them per column, as evenly spaced
  ranks past that), and the median line is the median of all of them, not a median of
  per-interval medians;
- the loss rail is lost probes over sent probes for the whole column;
- the hover readout gives the column's span, its median, replies out of sent, and how many
  intervals it holds, with coverage against the target's current interval. A column whose
  intervals are missing reads as low coverage, and one with none is a gap;
- quality flags are listed without counts, and ICMP errors are named only as "ICMP errors":
  a column knows which flags its intervals carried, not how many of each, nor which error.
  Zoom in to see the intervals themselves.

The detail view's median, p95, spread and loss are taken over the whole window the same way,
pooled into one column.

One read walks at most 10 million intervals, about thirty days of a one-second target. A
longer window of such a target is refused with a message that says so; ask for less.

### Reaching the data outside the UI

- **Availability** covers 7 and 30 days in the detail view, and `GET /api/v1/availability`
  takes any `from` and `to` (Unix seconds). This includes the outage list.
- **Raw intervals** come from `GET /api/v1/measurements?target_id=…&agent_id=…&from=…&to=…`
  as an Arrow IPC stream (`application/vnd.apache.arrow.stream`), not CSV. One request is
  refused when the window could hold more than 200 000 intervals, worked out at one interval
  per second, so ask for a day or so at a time. The sample lists are in microseconds. With
  `columns=N` the same request returns the window pooled as the graphs see it; see
  [API](api.md#pooled-measurements).
- **Path changes** for a window come from `GET /api/v1/paths` with the same query.

Whatever protects the UI protects the API, and there are no API tokens. See
[Authentication](authentication.md).

## The detail view

Clicking a target opens it at length: the pooled median, p95 and spread (p95 − p5) over the
window on screen, computed from the same samples the plot draws, above a full-height smoke
plot and the target's effective settings, alert rules and vantage points.

![A target's detail page: median, p95 and spread figures above a large density-smoke plot of
a target whose latency keeps splitting into two bands](images/detail.png)

*The detail view. p95 and the spread are the two numbers a median cannot give you, and the
reason the whole distribution is kept. Availability and coverage sit below, as two figures
rather than one.*

## Availability

The detail page also reports **availability** over a period — 24 hours, 7 days or 30 days —
per vantage point, which is what an SLA review or an incident write-up asks for. It reports
two numbers, and keeps them apart on purpose:

- **Availability** is uptime over the intervals there *is* data for. An interval is up when
  at least one probe came back, and down when every probe was lost. A **down threshold**
  chooses how strict that is: `100% loss` counts only a total blackout as down; `>50% loss`
  counts heavy loss as down too, for an SLA defined that way.
- **Coverage** is how much of the period was actually measured. A gap — smokeng not
  running, an agent offline — is **unknown** time: never counted as up, and never as down.
  Availability of 100% over 60% coverage is a different claim from 100% over 99%, and the
  two numbers side by side refuse to let the first pass for the second.

Beneath them is the list of **outages**: each contiguous run of down intervals, with its
start, end and duration — the evidence, not just the headline. **Download CSV** exports the
per-vantage-point summary for a report.

The CSV has one row per vantage point: target, agent, availability, coverage, and up, down
and unknown seconds, plus the number of outages. It does not list the outages; those are on
the page and in the API response.

The same report is at `GET /api/v1/availability?target_id=…`, with `from`, `to` and
`down_threshold` query parameters. To use it as evidence, see
[Proving a problem](proving-a-problem.md).

## Jitter plots for irtt

A target with `probe_type = "irtt"` gets extra plots under its round-trip smoke in the detail
view, on the same time cursor: jitter towards the target (`ipdv_send`), jitter on the way
back (`ipdv_receive`) and the time the far end held each packet (`server_processing`). Which
ones draw is set by `graph_series`; see [Configuration](configuration.md).

The jitter plots are the difference between consecutive packets, so the sign is kept: a
packet that caught up with its predecessor is below zero. The two ends' clocks need not agree.
A series the far end returned no timestamps for reads as "not measured", not as zero. ICMP,
DNS, HTTP and TCP targets have no such plots.

## Compare

The **compare** button overlays the pooled medians of the shown targets on one axis, which
the stacked plots cannot do — they answer "what is this target doing", not "which of these
is worse". Only the median is drawn; the distribution stays in the plots below.

![The compare overlay: two targets' pooled median lines, each from two vantage points, on one
shared axis in distinct colours, with a legend naming them](images/compare.png)

*Compare. Vantage points are never averaged — a target measured from two agents is two
lines, exactly as it is two plots.*

## Quality badges

smokeng records *how well it was able to measure* alongside *what it measured*, and shows
it beside the target name rather than leaving you to infer it. A badge with a count like
`(3/166)` affected only some intervals in view.

| Badge | Meaning | What to do |
| --- | --- | --- |
| `userspace TX` / `userspace RX` | Timestamps were taken in userspace, not by the kernel. Scheduler jitter is included in the measurement, so the smoke is wider than the network really is. On Linux, `tcp`, `http(s)` and `irtt` targets always carry both badges, because those probes are timed in userspace. On `icmp` and `dns` targets on Linux it means kernel timestamping was unavailable. Expected for everything on macOS. | Expect it on tcp, http(s) and irtt. On icmp or dns, check the host for scheduler load before blaming the path. |
| `raw socket` | Unprivileged datagram ICMP was unavailable, so smokeng fell back to a raw socket. Accuracy is unaffected; it means the sysctl is not set. See [Getting started](getting-started.md). | Set the sysctl; see Getting started. |
| `dropped replies` | The kernel receive queue overflowed and discarded replies. **Some of the loss shown is smokeng's, not the network's.** | Do not quote the loss as network loss. Raise the receive buffer or cut the ping rate, then measure again. |
| `clock step` | The wall clock jumped during these intervals. The affected round-trip times cannot be trusted. | Ignore those intervals' latency. Check NTP on the prober. |
| `cut short` | The interval ended early, after a restart or a settings change. Its loss figure is not comparable with a whole interval. | Leave it out of any loss figure you quote. |
| `not sent`, with a reason | The probes never went out: no route, a refused write, an irtt session that never opened. They still draw as loss, so an unreachable target is not an empty graph. The reason says whether the cause was local or the far end. | Read the reason first. A local cause is yours to fix; a refusal from the far end is evidence. |
| ICMP error | The target or a router answered with an ICMP error rather than an echo reply. The specific error (host unreachable, admin prohibited, TTL exceeded) appears in the hover readout. | Quote the exact error. It names who refused the traffic. |

The point of these badges is that a widened band always has an attributable cause. If
smokeng degraded its own measurement, it says so, on the graph, next to the data it
affected — rather than letting you spend an afternoon debugging a router that was fine.
