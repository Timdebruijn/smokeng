# Documentation screenshots

The docs reference these PNGs directly; the captions and alt text live with each
reference. All four are captured at 1280×942 logical, 2× device scale → **2560×1884**,
theme-light, from a live instance.

| File | View | Referenced from |
| --- | --- | --- |
| `smokeng.png` | **Graphs** (hero) — the stacked density smoke led by a service probe whose distribution shows real spread and a faint tail, with loss rails, quality badges and the target-tree sidebar. | `README.md`, `reading-graphs.md` |
| `overview.png` | **Overview** — the KPI cards, the per-series list with sparklines, and the firing/recent-alert panel. | `getting-started.md` |
| `compare.png` | **Compare** — the compare overlay (button in the Graphs toolbar): every vantage point's pooled median on one axis with the legend. | `reading-graphs.md` |
| `detail.png` | **Detail** — a target's page: median / p95 / spread above the full-height smoke plot, with the effective settings alongside. | `reading-graphs.md` |

## Recapturing

These were driven with headless Chrome over the DevTools Protocol — no extra
dependencies beyond Chrome and a recent Node (global `WebSocket`). Point it at a running
instance, set the range to **15m** so a recent loss band does not dominate the frame,
and prefer views with no empty "from &lt;agent&gt;" panels. The device metrics
(`1280×942`, `deviceScaleFactor: 2`) reproduce the resolution above exactly.
