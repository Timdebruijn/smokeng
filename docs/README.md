# smokeng documentation

smokeng measures latency and loss the way SmokePing taught everyone to look at it — a band,
not a line — and keeps every sample of every interval, at the resolution it was taken.

## Where to start

| You want to | Read |
| --- | --- |
| Get a first measurement on screen | [Getting started](getting-started.md) |
| Find a network problem and prove it to someone | [Proving a problem](proving-a-problem.md), then [Reading the graphs](reading-graphs.md) |
| Fix something that is not working | [Troubleshooting](troubleshooting.md) |
| Run it in production | [Operations](operations.md), [Authentication](authentication.md), the [Ansible role](../deploy/ansible/README.md) |
| Automate it or wire it into a platform | [Configuration](configuration.md) (config as code), [CLI](cli.md), [API](api.md) |
| Move from SmokePing | [Migrating from SmokePing](migrating-from-smokeping.md) |

## All pages

**Measuring**
- [Configuration](configuration.md): the TOML reference, the target tree, inheritance, limits
- [Probes](probes.md): what each probe type measures, how loss is counted, and where it misleads
- [Remote agents](agents.md): more vantage points, enrolment, running an agent as a service

**Reading**
- [Reading the graphs](reading-graphs.md): smoke, median, loss rail, path marks, quality badges
- [Proving a problem](proving-a-problem.md): what to put in a ticket, four worked cases
- [Alerting](alerting.md): rules, shape and bimodality, hysteresis, the webhook

**Running**
- [Operations](operations.md): service, proxy, metrics, logs, backup, upgrades, tuning
- [Authentication](authentication.md): OIDC per identity provider, sessions
- [Access control](access-control.md): roles, and scoping a group to one subtree
- [Troubleshooting](troubleshooting.md): by symptom

**Reference**
- [CLI](cli.md): every command and flag
- [API](api.md): routes, the auth model, the webhook payload

[DESIGN.md](../DESIGN.md) explains *why* smokeng is built this way. It is the reference for
behaviour that looks surprising; these pages are the reference for running it.

## Glossary

| Term | Meaning |
| --- | --- |
| Interval | One measurement period for one target and vantage point, `interval_s` long. It holds every RTT of that period. |
| Smoke | The density plot: per column, how the samples spread. A tight band is a steady path; a haze is jitter; two bands are two paths. |
| Pooled median | The median of all samples in a column or window, not a median of per-interval medians. |
| p5 / p95, spread | Percentiles of the samples; spread is p95 − p5. With 20 samples these are order statistics of 20 values, not smooth estimates. |
| Loss | Probes sent and not received, out of probes sent. A probe that could not be sent counts as lost and is flagged `not sent`. |
| Vantage point | Where a measurement is taken from: the master's own prober (`local`) or a remote agent. Each is its own series. |
| Series | One target as seen from one vantage point. |
| Coverage | The share of a window for which there is data. Availability is always given next to it. |
| Quality badge | A flag on an interval saying the measurement itself is suspect: userspace timestamps, dropped replies, a clock step and more. |
| Path change mark | A mark on the time axis where the traceroute hop list differed from the last one stored. Master only. |
| Shape rule | An alert on how far the distribution drifted from its baseline (Wasserstein distance). |
| Bimodality rule | An alert when one band of latency becomes two. |
| Baseline | What a shape or bimodality rule compares against: `rolling` (the recent past) or `golden` (a window you captured). |
| Hysteresis | `for_intervals` matching intervals to fire, `clear_intervals` to clear, so a rule does not flap. |
| Grant | A role (viewer or editor) for a group on one subtree. |
