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

`spread` is the one SmokePing could never alert on, and it is the reason for keeping the
whole distribution. A link whose median is unchanged but whose spread has tripled is
degrading — a saturated uplink, a failing radio, a queue that has started to build — and
neither an average nor a p95 alone will tell you.

A sensible starting set is loss above 20 %, median above whatever your baseline plus a
margin is, and spread above roughly three times its usual value.

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
alerts are repeated once a minute because Alertmanager expires an alert it stops hearing
about.

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
