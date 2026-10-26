# Documentation screenshots

The README and the docs reference these files directly; captions and alt text live with each
reference.

| File | View | Referenced from |
| --- | --- | --- |
| `tour.gif` | A short tour: overview, graphs, a crosshair sweep, a brush-zoom, a detail page, alerts. 960 px wide, 17 frames. | `README.md` |
| `graphs.png` | **Graphs** — an uplink with evening congestion and loss, a wifi access point that splits into two bands, an irtt target with its userspace badges. | `README.md`, `reading-graphs.md` |
| `overview.png` | **Overview** — KPI cards, the per-series list with sparklines, firing alerts and measurement quality. | `README.md`, `getting-started.md` |
| `compare.png` | **Compare** — two public resolvers, each from the master and from a branch-office agent. | `README.md`, `reading-graphs.md` |
| `detail.png` | **Detail** — median, p95, spread and loss above the full-height smoke of the bimodal target. | `README.md`, `reading-graphs.md` |
| `alerts.png` | **Alerts** — a firing bimodality rule and a latency rule, the rule list, silences, history. | `README.md`, `alerting.md` |
| `targets.png` | **Targets** — the tree admin with values set on the node next to inherited ones. | `README.md` |

Stills are 1280 logical pixels wide at 2× device scale, light theme; the GIF is captured at 1×.

## How they were made

From a demo instance, not a production one: a throwaway VM running the released binary against
public resolvers plus three network namespaces whose reply path runs through `netem` (the
delay has to sit on the reply side: kernel transmit stamps are taken after the qdisc, so a
delay on the sending host is correctly not measured). A small loop changes the conditions on a
schedule — evening congestion with loss on one uplink, and a random half of the replies on a
wifi target sent through a slower band — so the alerts in the shots fired for real. A second
namespace runs an agent behind 25 ms of extra WAN delay for the compare view.

Pages were driven with Playwright (`playwright-core` against the system Chrome) on the 15m
range, and the GIF frames were joined with Pillow using one palette built from every frame.
A young demo shows low availability *coverage* over 7 days, which is true and would read as
broken in a screenshot, so the detail shot stops above the availability panel.
