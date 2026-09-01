# Documentation screenshots

The docs reference these PNGs directly; the captions and alt text live with each
reference. All are captured from a live instance at 1280 logical pixels wide, 2× device
scale, theme-light.

| File | View | Referenced from |
| --- | --- | --- |
| `smokeng.png` | **Graphs** (hero) — the stacked density smoke led by a service probe whose distribution shows real spread and a faint tail, with loss rails, quality badges and the target-tree sidebar. | `README.md`, `reading-graphs.md` |
| `overview.png` | **Overview** — the KPI cards, the per-series list with sparklines, and the firing/recent-alert panel. | `getting-started.md` |
| `compare.png` | **Compare** — the compare overlay: every vantage point's pooled median on one axis with the legend. | `reading-graphs.md` |
| `detail.png` | **Detail** — median / p95 / spread above the full-height smoke plot, the availability panel, and the effective settings and alert rules alongside. | `reading-graphs.md` |
| `alerts.png` | **Alerts** — firing alerts, the rule list (including shape and bimodality rules and a golden reference), silences and maintenance windows, and the transition history. | `alerting.md` |

The first three are 2560×1884 (942 logical high); `detail.png` and `alerts.png` are
2560×3000, since both pages are longer than a viewport.

## Recapturing

Driven with headless Chrome over the DevTools Protocol — no dependencies beyond Chrome
and a recent Node (global `WebSocket`). Point it at a running instance and set device
metrics to `1280×<height>`, `deviceScaleFactor: 2`.

Two things matter for a shot that is honest as well as tidy:

- Set the graph range to **15m** and prefer views with no empty "from &lt;agent&gt;"
  panels, so nothing reads as broken that is merely unassigned.
- The availability panel defaults to **7d**. On a target only a day or two old that
  shows low coverage — true, but an artefact of the instance rather than of the report.
  **24h** shows the panel doing its job.
