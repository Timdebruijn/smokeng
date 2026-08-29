# smokeng documentation

smokeng measures round-trip latency the way SmokePing taught everyone to look at it — a
band of latency, not a line — and keeps the full distribution of every measurement
interval, forever, at the resolution it was taken. Nothing is averaged away as data ages.

Start here:

| Guide | What it covers |
| --- | --- |
| [Getting started](getting-started.md) | Install, run, add your first target, open the UI |
| [Configuration](configuration.md) | The complete TOML reference, the target tree, inheritance |
| [Reading the graphs](reading-graphs.md) | What the smoke, the median line, the loss rail and the quality badges mean |
| [Alerting](alerting.md) | Alert rules, hysteresis, webhook delivery |
| [Remote agents](agents.md) | Measuring from more than one vantage point |
| [Authentication](authentication.md) | OIDC login and sessions |
| [Access control](access-control.md) | Roles, and scoping a user to one subtree |
| [Operations](operations.md) | Storage growth, backups, Prometheus metrics, running as a service |
| [Migrating from SmokePing](migrating-from-smokeping.md) | Importing an existing `Targets` file, and what differs |

`DESIGN.md` in the repository root explains *why* smokeng is built this way. It is the
reference for behaviour that looks surprising; this documentation is the reference for
operating it.
