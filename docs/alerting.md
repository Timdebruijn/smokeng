# Alerting

Alert rules are evaluated per target, per agent, once per finalised measurement interval.
They are edge-triggered with hysteresis in both directions: a rule fires only after the
condition has held for a number of consecutive intervals, and clears only after it has
stopped holding for a number of consecutive intervals. A flapping link produces one alert,
not forty.

![The Alerts page: what is firing (a bimodality rule and a latency rule), the rule list,
the silences and maintenance windows, and the transition history](images/alerts.png)

*The Alerts page. Rules, the silences that mute them for a window, and the history of every
transition — recorded whether or not a webhook was reachable.*

## Defining a rule

In TOML, under `[default_alerts.<name>]` for the whole tree or
`[targets."<path>".alerts.<name>]` for one node:

```toml
[default_alerts.loss]
metric = "loss"
op = ">"
threshold = 20
for_intervals = 3
clear_intervals = 3

[default_alerts.slow]
metric = "median"
op = ">"
threshold = 150
```

Or in the web UI under **Alerts**, as an admin.

## The metrics

| Metric | Unit | What it catches |
| --- | --- | --- |
| `loss` | percent, 0–100 | Packets that never came back |
| `median` | milliseconds | The typical round-trip time got worse |
| `p95` | milliseconds | The tail got worse, even if the median did not |
| `spread` | milliseconds | p95 − p5: the distribution got *wider* |
| `shape` | ms, or a z-score | The distribution *moved* — a shift or a growing tail |
| `bimodality` | 0–1 | The distribution *split in two* — a load-balanced or flapping path |

`spread` is the one SmokePing could never alert on, and it is the reason for keeping the
whole distribution. A link whose median is unchanged but whose spread has tripled is
degrading — a saturated uplink, a failing radio, a queue that has started to build — and
neither an average nor a p95 alone will tell you.

### Starting points

These are starting points, not recommendations. Look at the graphs for a week, then move each
threshold to just beyond what the link does on a normal day.

| Target | Rule | Why |
| --- | --- | --- |
| Uplink to the internet | `loss > 5`, `p95 > 100` ms | Loss over a few percent is already visible to users; the tail shows a filling queue first |
| LAN gateway | `loss > 1`, `median > 5` ms | A wired hop should be near-perfect; anything else is a local fault |
| Resolver | `loss > 2`, `median > 100` ms | A cached answer is fast, so a high median points at the resolver or its upstream |

Keep `for_intervals` at the default or higher on a one-minute interval. One bad interval is
noise.

## Rule fields

| Field | Type | Default | Meaning |
| --- | --- | --- | --- |
| `metric` | string | required | One of the metrics above |
| `op` | string | required | `>` or `<`. `shape` and `bimodality` accept only `>` |
| `threshold` | float | required | The value to compare against. For `shape` and `bimodality` in `auto` mode it is the z cutoff, see below |
| `for_intervals` | int | `3` | Consecutive breaching intervals before firing |
| `clear_intervals` | int | `3` | Consecutive non-breaching intervals before clearing |
| `disabled` | bool | `false` | Keep the rule but stop evaluating it (`enabled` in the API) |
| `mode` | string | none | `auto` or `tunable`. Required for `shape` and `bimodality`, unused otherwise |
| `baseline` | string | none | `rolling` or `golden`. Required for `shape`, unused otherwise |

`mode` and `baseline` are explained [below](#alerting-on-the-shape-of-the-distribution). A TOML
rule that leaves out `for_intervals` or `clear_intervals` gets the default `3`, not `1`.

## Alerting on the shape of the distribution

Every metric above still reduces an interval to one number. `shape` and `bimodality` do
not: they compare the *distribution*, which is the thing smokeng keeps and nothing else
has. They are for the faults a threshold cannot express.

**`shape`** measures how far the current distribution has moved from a baseline, as the
Wasserstein (earth-mover) distance — the average distance the probability mass has to
travel to turn one into the other. It is sensitive to the tail, so a bulk that stays at
4 ms while a growing fraction reaches 40 ms registers as movement even though the median
never budges.

**`bimodality`** is Sarle's coefficient of the interval itself, from 0 to 1. Above about
0.6 the samples have split into two clusters, which is the signature of load-balancing
across two paths of unequal length, or of a failover that is flapping. The median sits
uselessly between the two, so no threshold on it would ever fire.

### Auto or tunable

Each shape rule picks how strict it is:

- **auto** self-calibrates. The distance is scored against the series' *own* recent
  variability (a robust z-score: median and MAD, so one past spike does not inflate the
  scale and mask the next). A jittery Wi-Fi link and a stable fibre have very different
  normal wobbles, and the same absolute distance means different things on each — auto
  handles that without a per-target threshold. `threshold` is the z cutoff. The UI form
  suggests 4 for `shape` and 0.6 for `bimodality`; the backend has no built-in default.
- **tunable** compares the raw measure to a threshold you set: milliseconds of movement
  for `shape`, or the coefficient itself for `bimodality`.

### Rolling or golden baseline

A `shape` rule compares against one of two things:

- **rolling** — the target's own recent history. This catches change relative to whatever
  the path has been doing lately, and needs no setup.
- **golden** — a reference you capture once, from a window you choose: *this is what good
  looks like*. A drift away from the commissioned state is what fires, however long ago
  that was. Capture it from the rule list (**Capture reference**), which records the
  distribution measured over that window, along with where it came from. **A golden rule
  with no captured reference cannot fire, and says so** rather than quietly never
  triggering.

  The button captures the last hour. Through the API a window may be chosen, but not
  wider than about 55 hours (200,000 seconds), and not holding more than two million
  samples: every sample in it is read into memory, sorted and stored as one blob. The
  first bound is on intervals at the densest a series can be; the second counts the
  samples themselves, since one interval can hold tens of thousands. A reference can only
  be captured from the node the rule is defined on or one beneath it.

### Seeing what changed

A z-score is a claim. When a `shape` rule fires, the Alerts page offers the evidence —
the reference distribution and the current one drawn together — so you can see what moved
rather than take the number on faith. It is the same commitment the graphs make.

Shape rules are ordinary rules in every other respect: they inherit down the tree, take
`for_intervals`/`clear_intervals` hysteresis, can be acknowledged and silenced, and deliver
over the same webhook. They stay quiet while warming up after a restart, and when a flat
history gives them no scale to judge by — a monitoring tool that cries wolf gets ignored, so these err
towards saying nothing.

## Hysteresis, and why gaps reset it

The consecutive-interval counter resets when smokeng cannot trust the input: a gap in the
data of more than two intervals, or an interval flagged as untrustworthy (a clock step, a
receive-queue overflow). Only two flags change what a rule reads: a socket overflow
is left out of loss, and a clock step is left out of the round-trip metrics. A probe that
failed to send (for example local file-descriptor exhaustion) still counts as sent and lost,
so it can fire a loss alert.

Alert state is persisted, so an alert that has been firing for an hour does not resolve
and re-fire every time the service restarts.

## Inheritance

Alert rules are inherited down the target tree, but they **replace** rather than
accumulate: if a node defines any rules of its own, those are the only rules that apply to
it, and its ancestors' rules are ignored.

This is a deliberate choice. Accumulation makes the effective rule set of a node something
you reconstruct by reading every ancestor, and makes "this one host is allowed to be
slower" impossible to express without disabling rules from a distance. With replacement,
what applies to a node is visible in one place.

The practical consequence: if you want a node to keep the inherited loss rule *and* add a
latency rule, restate both on that node.

## Delivery

Firing and resolved alerts are POSTed to a webhook in Alertmanager's v2 format:

```bash
smokeng serve --alert-webhook https://alertmanager.example.org/api/v2/alerts
```

The fields are listed in [api.md](api.md#webhook-payload). The payload is a JSON array of
alert objects, so anything that speaks Alertmanager — Alertmanager itself, Grafana OnCall,
ntfy shims, a small script — takes it unmodified.
Resolved alerts are sent as well, so receivers that track state stay in sync, and firing
alerts are repeated — once a minute by default, `--alert-repeat` sets the interval —
because Alertmanager expires an alert it stops hearing about.

**Where the URL lives.** A webhook URL is often the credential: a token in the path, a key
in the query, a user and password before the host. `--alert-webhook` puts it in the
process list, readable by every local user, so smokeng warns when it is used; put the URL
in a file (mode 0600) and pass `--alert-webhook-file PATH` instead. Neither the startup log
nor an error names more than the scheme and host.

**Delivery does not wait on the receiver.** Batches are posted from a queue of 64 on their
own goroutine, so a receiver that is slow or down does not hold up the loop that records
measurements. If the receiver stays unreachable until the queue is full, the oldest batch is
dropped, with a log line, to keep the newest: a lost transition is logged, and a
still-firing alert is repeated on the next `--alert-repeat`. On shutdown what is queued is
delivered, for up to five seconds; a resolved alert is not announced again, so it is not
dropped lightly. `delivering` in `GET /api/v1/alerts` says a webhook is configured, not that
every batch got through.

**Delivery failures are counted and logged.** `smokeng_webhook_deliveries_total{result}` on
`/metrics` counts each POST as `ok`, `error` or `dropped` (the queue was full). A failed POST is
not retried. A firing alert is posted again on the next `--alert-repeat`, but a missed resolve is
not. The log line starts `alert: deliver`. See [operations](operations.md) for a rule that fires
on failures.

**Rules are evaluated whether or not `--alert-webhook` is set.** Firing state and the
transition history are live either way; a missing webhook only means a transition is
never posted anywhere. `GET /api/v1/alerts` reports `enabled` (rules are being
evaluated) and `delivering` (transitions are being posted) as separate facts, so the UI
never has to guess which one is true.

Currently firing alerts are also visible in the UI under **Alerts**, and the count is
exported to Prometheus as `smokeng_alerts_firing`. If you would rather alert from
Prometheus, scrape [`/metrics`](operations.md) and skip the webhook entirely.

Webhook delivery is the only notifier implemented. Email and chat integrations are
deliberately not built: every organisation already has a router for those, and it is not
smokeng's job to become a second one.

### Alertmanager example

`severity` is always `warning` and cannot be set per rule, so route on `alertname` or `target`.
This sends alerts under `/Home` to a Slack channel and everything else to the default receiver.

```yaml
route:
  receiver: default
  group_by: [alertname, target]
  routes:
    - matchers: [target=~"/Home/.*"]
      receiver: slack-network
receivers:
  - name: default
  - name: slack-network
    slack_configs:
      - api_url_file: /etc/alertmanager/slack-url
        channel: "#network"
        send_resolved: true
        title: '{{ .CommonLabels.alertname }} on {{ .CommonLabels.target }}'
        text: '{{ range .Alerts }}{{ .Annotations.summary }} (from {{ .Labels.agent }}){{ "\n" }}{{ end }}'
```

## Acknowledging and silencing

Two different things quiet a firing alert without touching the rule, and the difference is
the point.

An **acknowledge** mutes one firing episode's demand for attention in the UI — "seen,
handling it" — while the alert keeps firing *and keeps being delivered*: downstream still
needs to know. It is tied to the episode, so if the alert resolves and later fires again,
the new episode is unacknowledged and shouts afresh.

A **silence** suppresses both delivery and attention for matching alerts over a time
window. This is the one that stops the webhook — because a maintenance window's whole
purpose is not to page during planned work. A silence never rewrites history: the alert
still fires, still advances its hysteresis, and the transition is still recorded to the
log. Only the notification and the UI's alarm are held back; when the window closes,
whatever is still firing is re-announced.

A silence is scoped like everything else on the tree. Leave it global, or narrow it to a
node — which covers that node and its whole subtree — and optionally to one agent or one
rule. It comes in two shapes: a duration from now (the quick "silence this for two hours"
on a firing alert), or an explicit `[from, until)` window booked ahead of time, which is a
maintenance window. Both are managed under **Alerts → Silences & maintenance windows**, or
over the API. Silences are not part of the TOML file and `smokeng config export` does not
write them: they live only in the database.

Who set a silence (`created_by`), captured a baseline (`captured_by`) or acknowledged an alert
(`acked_by`) is the e-mail address from their session (or the name or subject when the identity
provider sends none). It is shown only to callers who may change that node (for an alert, the
target that fired), and for a global silence only to administrators. Everyone else sees the same
entry with the field empty.

These calls need a session, or an instance without OIDC reached on `localhost`; see
[api.md](api.md#authentication). Without OIDC they work as written, because `curl` sends no
`Origin` header.

```bash
# Silence everything under /Production for two hours, from now.
curl -m8 -X POST http://localhost:8080/api/v1/silences \
  -d '{"target_id": 4, "duration_s": 7200, "reason": "DB cluster upgrade"}'

# Book a maintenance window ahead of time.
curl -m8 -X POST http://localhost:8080/api/v1/silences \
  -d '{"target_id": 4, "starts_at": 1793000000, "ends_at": 1793007200, "reason": "planned"}'
```

If you deliver to Alertmanager, its own silences work too and may fit a cross-tool workflow
better; smokeng's are for native use and for a maintenance window booked against a node of the
target tree.
