# Proving a problem

For anyone who has to show an ISP, a customer or a colleague that a network fault is real.
It answers: what to measure, what the graphs prove, and what they cannot.

The README's 21:00 story is the reason this page exists: the ISP's test says 12 ms, and the
evening is gone from their numbers. Here is how to bring the evening back, in a form someone
else can check.

## Set up to prove something

Measure before you complain. A graph that starts after the fault shows nothing.

**Two vantage points at least.** One probe can only say "this is slow from here". A second
one, on another network or at the far end, separates a fault at your site from a fault on the
target. See [Remote agents](agents.md). Remember that agents do not run traceroute, so path
marks come from the master's own prober only.

**Separate your link from their network.** Add targets that cut the path in pieces:

```toml
[targets."Uplink/gateway"]
host = "192.168.1.1"          # your own router: the LAN side is clean or it is not

[targets."Uplink/isp-hop1"]
host = "203.0.113.1"          # first hop beyond your router, from the path on the graph

[targets."Uplink/far"]
host = "198.51.100.10"        # a stable target well beyond the ISP
```

Read the first ISP hop from the route shown when you hover a plot, or from
`GET /api/v1/paths?target_id=…`. Use three or four pieces, not twenty.

If the gateway is clean and the first ISP hop is bad, the fault is on the link or the
access network. If the gateway is already bad, the fault is yours.

**The right probe for the question.** ICMP answers "is the path lossy or slow". `irtt` answers
"which direction", and it carries the jitter plots. `tcp` and `http` answer "does the service
respond", with the caveats in [Probes](probes.md). Do not use an HTTP probe to prove a
network fault: a status of 400 or more counts as loss.

**Enough samples.** Keep the default 20 pings or raise it. With 20, one lost packet is 5 %;
see [what N means](reading-graphs.md#what-n-means). Leave the probes running for several days so the
fault shows up more than once.

## What to put in the ticket

Give the reader something they can open and something they can check.

1. **The time window**, with the time zone, in UTC if the ISP works in UTC.
2. **The targets**, by host and address family, and the vantage point each was measured from.
3. **A screenshot of the detail view** for the window. It has the pooled median, p95 and spread
   above the smoke plot, and the quality badges next to the name. There is no screenshot
   button; use your operating system's.
4. **The availability CSV.** The detail view's Availability section has a `Download CSV` for
   24 hours, 7 or 30 days. It holds one row per vantage point: availability, coverage, up,
   down and unknown seconds, and the number of outages.
5. **The outage list**, copied from the same section. It is not in the CSV.
6. **For a longer window or the raw intervals**, the API: `GET /api/v1/availability` and
   `GET /api/v1/measurements`, as described in
   [Reaching the data outside the UI](reading-graphs.md#reaching-the-data-outside-the-ui). Measurements come as Arrow,
   not CSV.

These are the only exports there are. There is no PDF report and no CSV of the smoke.

### State loss and coverage honestly

- Give loss as received out of sent, with the window: "31 of 600 probes lost between 20:00
  and 23:00 UTC", not "5 % loss".
- Give coverage next to availability. 100 % available over 60 % coverage is a smaller claim
  than it sounds.
- Name the down threshold you used (`100% loss` or `>50% loss`).
- Look at the badges first. `dropped replies` means some loss is the monitor's own; `not sent`
  means the probes never left; `cut short` intervals should be left out. If a badge covers
  your window, say so.
- Say what you did not measure. A fault seen from one place is a fault seen from one place.

## Four worked cases

In each one the rail is the `loss` strip under the plot.

### Evening congestion

**You see:** a thin band by day. Between about 19:00 and 23:00 the band rises and fans out
into a haze, the median climbs, and the loss rail shows colour. It repeats at the same hours
on other days. The gateway target stays flat.

**It means:** a queue fills somewhere beyond your router when many people are online.

**Confirm:** overlay the gateway, first ISP hop and far target in **compare**. If the first ISP
hop and the far target climb together and the gateway does not, the congestion is at or
before the first hop. Check three or more days: one evening proves nothing.

**Tell the ISP:** the window, the days it repeated, the pooled median and p95 for the evening
against the same hours of a quiet period, and the loss as received out of sent. Ask where
the queue is, not whether there is one.

**Limits:** it shows the delay and loss on the path from your probe. It does not show which
router, or whose network, holds the queue. The first ISP hop is where your path leaves your
side, not necessarily where the fault is.

### Bufferbloat under load

**You see:** with the link idle, a thin band. When something uploads or downloads, the band
rises by tens or hundreds of milliseconds for as long as the transfer runs, and falls again
when it stops. Loss is low or none.

**It means:** a buffer in front of the bottleneck fills. The packets are delayed, not dropped.

**Confirm:** an `irtt` target gives the direction. Open its detail view and read the two
jitter plots: `ipdv_send` is towards the target, `ipdv_receive` is on the way back. Saturate
the upload and watch which one widens. Then start the transfer yourself, note the time, and
compare the window to an idle one. smokeng does not generate load; use `iperf3` or a large
copy.

**Tell the ISP or the vendor:** idle against loaded, with the times of the transfers, and the
direction that widened. Ask about queue management on the bottleneck.

**Limits:** ipdv is the difference between consecutive packets, so it shows delay variation,
not the delay itself. The raw smoke gives the delay. A transfer that saturates your own LAN
causes the same picture, which is why the gateway target belongs in the set.

### A flapping path

**You see:** the smoke splits into two bands that appear and disappear. Amber dashed lines
sit on the time axis near the changes. If you have a `bimodality` alert rule on the target,
it fires; see [Alerting](alerting.md).

**It means:** traffic alternates between two routes of different length, or a load balancer
spreads it over two.

**Confirm:** hover each side of an amber mark. The route in force shows the hops. A mark means
the hop list differed from the last one stored, nothing more. If the bands switch without
a mark, the paths may differ beyond the hops traceroute sees, or the path changed between two
traceroutes (`trace_interval_s` defaults to 300 s).

**Tell the ISP:** the times of the marks, the two hop lists, and the two latency bands with
their medians.

**Limits:** marks come from the master's prober only. A flap seen from a remote agent has
bands but no marks. A hop that stopped answering traceroute can look like a change. Equal
cost load balancing is not a fault, only a shape; say that you are asking whether it is
expected.

### Loss on one hop

**You see:** the first ISP hop target shows loss on the rail. The far target shows none.

**It means:** probably nothing. Routers deprioritise ICMP addressed to themselves. Loss at a
router that does not continue to the targets behind it is usually that.

**Confirm:** loss that is real shows up on every target past that hop. If the far target is
clean at the same minutes, the loss at the hop is rate limiting, and the traffic is fine.
If the far target shows the same loss, it is real, and the hop is where to look.

**Tell the ISP:** only the case where the loss reaches the far target: the window, the loss
as received out of sent for both targets, and the vantage points that saw it.

**Limits:** the opposite also holds. Loss at the far target only, with a clean hop, can be the
target rate limiting you. A second vantage point settles it: loss from both is the target's
or the path's; loss from one is the path to that one.

## What the data cannot prove

- **A cause.** smokeng shows delay and loss on paths from your probes. Where in the ISP's
  network the fault sits is theirs to find.
- **That a fault is theirs.** A flat gateway is the best evidence the fault is not yours, and it
  is evidence, not proof.
- **Anything about traffic it did not send.** Probes are small and few. Your video call is
  neither.
- **Loss below the sample.** With 20 pings, a 1 % loss rate shows up as zero in most
  intervals. Pool a long window before you quote it.
