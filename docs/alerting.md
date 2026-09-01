# Alerting

Alert rules are evaluated per target, per agent, once per finalised measurement interval.
They are edge-triggered with hysteresis in both directions: a rule fires only after the
condition has held for a number of consecutive intervals, and clears only after it has
stopped holding for a number of consecutive intervals. A flapping link produces one alert,
not forty.

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

| Field | Type | Default | Meaning |
| --- | --- | --- | --- |
| `metric` | string | required | `loss`, `median`, `p95` or `spread` |
| `op` | string | required | `>` or `<` |
| `threshold` | float | required | The value to compare against |
| `for_intervals` | int | `3` | Consecutive breaching intervals before firing |
| `clear_intervals` | int | `3` | Consecutive non-breaching intervals before clearing |
| `disabled` | bool | `false` | Keep the rule but stop evaluating it |

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

A sensible starting set is loss above 20 %, median above whatever your baseline plus a
margin is, and spread above roughly three times its usual value.

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
  handles that without a per-target threshold. It fires above z 4.
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

### Seeing what changed

A z-score is a claim. When a `shape` rule fires, the Alerts page offers the evidence —
the reference distribution and the current one drawn together — so you can see what moved
rather than take the number on faith. It is the same commitment the graphs make.

Shape rules are ordinary rules in every other respect: they inherit down the tree, take
`for`/`clear_for` hysteresis, can be acknowledged and silenced, and deliver over the same
webhook. They stay quiet while warming up after a restart, and when a flat history gives
them no scale to judge by — a monitoring tool that cries wolf gets ignored, so these err
towards saying nothing.

## Hysteresis, and why gaps reset it

The consecutive-interval counter resets when smokeng cannot trust the input: a gap in the
data of more than two intervals, or an interval flagged as untrustworthy (a clock step, a
receive-queue overflow). A measurement smokeng knows is wrong must not fire an alert, and
measurements flagged as smokeng's own fault are excluded from the metrics they would
distort.

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

The payload is a JSON array of alert objects, so anything that speaks Alertmanager —
Alertmanager itself, Grafana OnCall, ntfy shims, a small script — takes it unmodified.
Resolved alerts are sent as well, so receivers that track state stay in sync, and firing
alerts are repeated — once a minute by default, `--alert-repeat` sets the interval —
because Alertmanager expires an alert it stops hearing about.

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
over the API:

```bash
# Silence everything under /Production for two hours, from now.
curl -X POST http://localhost:8080/api/v1/silences \
  -d '{"target_id": 4, "duration_s": 7200, "reason": "DB cluster upgrade"}'

# Book a maintenance window ahead of time.
curl -X POST http://localhost:8080/api/v1/silences \
  -d '{"target_id": 4, "starts_at": 1793000000, "ends_at": 1793007200, "reason": "planned"}'
```

If you deliver to Alertmanager, its own silences work too and may fit a cross-tool workflow
better; smokeng's are for native use and for expressing a maintenance window declaratively
against the target tree.
