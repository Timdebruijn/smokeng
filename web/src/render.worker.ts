/**
 * Density renderer (DESIGN.md §8.2). Runs in a worker on an OffscreenCanvas
 * so the main thread never blocks. Per pixel column: pool the samples of
 * every interval in that column's time span, deposit them as impulses in
 * pixel space, run one 1-D Gaussian blur, map density to alpha, and write the
 * column into a single ImageData. The median line is computed from the pooled
 * samples — never a median of medians — and gaps stay gaps: no interpolation
 * across missing buckets.
 */

interface SeriesMsg {
  ts: Float64Array
  sent: Float64Array
  received: Float64Array
  offsets: Uint32Array
  values: Uint32Array
}

interface ViewMsg {
  w: number
  h: number
  t0: number
  t1: number
  dark: boolean
  log: boolean
}

// Density→alpha: count-normalized fixed curve alpha = 1 − exp(−K·fraction)
// (mode (b) of the design's trade-off #2), keeping columns comparable over
// time regardless of how many samples they pool.
const ALPHA_K = 14
const SIGMA_PX = 1.5
const KERNEL_R = 4
const RAIL_H = 10
const RAIL_GAP = 4
const AXIS_W = 56
const AXIS_H = 18

let canvas: OffscreenCanvas | null = null

self.onmessage = (ev: MessageEvent) => {
  const msg = ev.data
  if (msg.type === 'init') {
    canvas = msg.canvas as OffscreenCanvas
  } else if (msg.type === 'render' && canvas) {
    render(canvas, msg.series as SeriesMsg, msg.view as ViewMsg)
  }
}

function gaussianKernel(): Float32Array {
  const k = new Float32Array(2 * KERNEL_R + 1)
  let sum = 0
  for (let i = -KERNEL_R; i <= KERNEL_R; i++) {
    const v = Math.exp(-(i * i) / (2 * SIGMA_PX * SIGMA_PX))
    k[i + KERNEL_R] = v
    sum += v
  }
  for (let i = 0; i < k.length; i++) k[i] /= sum
  return k
}
const KERNEL = gaussianKernel()

// Sequential viridis stops for the loss rail (perceptually uniform).
const VIRIDIS: [number, number, number][] = [
  [68, 1, 84], [72, 40, 120], [62, 74, 137], [49, 104, 142], [38, 130, 142],
  [31, 158, 137], [53, 183, 121], [109, 205, 89], [180, 222, 44], [253, 231, 37],
]
function viridis(t: number): [number, number, number] {
  const x = Math.min(Math.max(t, 0), 1) * (VIRIDIS.length - 1)
  const i = Math.min(Math.floor(x), VIRIDIS.length - 2)
  const f = x - i
  const [r1, g1, b1] = VIRIDIS[i]
  const [r2, g2, b2] = VIRIDIS[i + 1]
  return [r1 + f * (r2 - r1), g1 + f * (g2 - g1), b1 + f * (b2 - b1)]
}

function quantile(sorted: Uint32Array, q: number): number {
  if (sorted.length === 0) return 0
  return sorted[Math.min(sorted.length - 1, Math.floor(q * sorted.length))]
}

function fmtUs(us: number): string {
  if (us < 1000) return `${Math.round(us)}µs`
  if (us < 1_000_000) return `${(us / 1000).toFixed(us < 10_000 ? 1 : 0)}ms`
  return `${(us / 1_000_000).toFixed(1)}s`
}

function render(cv: OffscreenCanvas, d: SeriesMsg, view: ViewMsg) {
  const { w, h, t0, t1, dark, log } = view
  cv.width = w
  cv.height = h
  const ctx = cv.getContext('2d')!
  ctx.clearRect(0, 0, w, h)

  const plotW = w - AXIS_W
  const plotH = h - RAIL_H - RAIL_GAP - AXIS_H
  const n = d.ts.length

  // y scale. Log (the default): the full range fits naturally, outliers and
  // all. Linear: clip the top 0.5% so one outlier doesn't crush the plot.
  const allSorted = sortedCopy(d.values)
  let ymax: number
  let ymin = 0
  if (log) {
    ymax = Math.max((allSorted[allSorted.length - 1] ?? 0) * 1.2, 1000)
    ymin = Math.max(quantile(allSorted, 0.005) * 0.7, 20)
  } else {
    ymax = Math.max(quantile(allSorted, 0.995) * 1.15, 1000)
  }
  const lmin = Math.log10(Math.max(ymin, 1))
  const lmax = Math.log10(ymax)
  // fraction of plot height for an RTT in µs (0 = bottom, 1 = top)
  const yFrac = (us: number): number => {
    if (log) return (Math.log10(Math.max(us, ymin)) - lmin) / (lmax - lmin)
    return Math.min(us, ymax) / ymax
  }
  // inverse, for axis tick labels
  const fracVal = (f: number): number => (log ? Math.pow(10, lmin + f * (lmax - lmin)) : f * ymax)

  const text = dark ? 'rgba(255,255,255,0.65)' : 'rgba(0,0,0,0.6)'
  const grid = dark ? 'rgba(255,255,255,0.09)' : 'rgba(0,0,0,0.08)'
  const smoke: [number, number, number] = dark ? [120, 180, 255] : [23, 80, 190]
  const medianColor = dark ? '#7dd3fc' : '#0369a1'

  // Assign measurement rows to pixel columns. A row's ts is its interval
  // START: the row covers [ts, ts + interval) and must land in every column
  // that span overlaps. The interval isn't on the wire, so infer it from the
  // median spacing between consecutive rows (robust against missed buckets).
  const dt = (t1 - t0) / plotW
  let span = 60
  if (n > 1) {
    const diffs: number[] = []
    for (let i = 1; i < n; i++) diffs.push(d.ts[i] - d.ts[i - 1])
    diffs.sort((a, b) => a - b)
    span = diffs[Math.floor(diffs.length / 2)]
  }
  const colRows: number[][] = Array.from({ length: plotW }, () => [])
  for (let i = 0; i < n; i++) {
    const x0 = Math.max(0, Math.floor((d.ts[i] - t0) / dt))
    const x1 = Math.min(plotW - 1, Math.ceil((d.ts[i] + span - t0) / dt) - 1)
    for (let x = x0; x <= x1; x++) colRows[x].push(i)
  }

  const img = ctx.createImageData(plotW, plotH)
  const dens = new Float32Array(plotH)
  const blurred = new Float32Array(plotH)
  const median = new Float64Array(plotW).fill(NaN)
  const loss = new Float64Array(plotW).fill(NaN)

  for (let x = 0; x < plotW; x++) {
    const rows = colRows[x]
    if (rows.length === 0) continue
    dens.fill(0)
    let total = 0
    let sumSent = 0
    let sumRecv = 0
    const pooled: number[] = []
    for (const i of rows) {
      sumSent += d.sent[i]
      sumRecv += d.received[i]
      for (let j = d.offsets[i]; j < d.offsets[i + 1]; j++) {
        const y = (plotH - 1) * (1 - Math.min(yFrac(d.values[j]), 1))
        const y0 = Math.floor(y)
        const f = y - y0
        if (y0 >= 0 && y0 < plotH) dens[y0] += 1 - f
        if (y0 + 1 >= 0 && y0 + 1 < plotH) dens[y0 + 1] += f
        total++
        pooled.push(d.values[j])
      }
    }
    if (sumSent > 0) loss[x] = 1 - sumRecv / sumSent
    if (total === 0) continue

    pooled.sort((a, b) => a - b)
    median[x] = pooled[Math.floor(pooled.length / 2)]

    // One separable 1-D blur per column: impulses-then-blur is identical to
    // per-sample kernels but O(samples + height·kernel).
    blurred.fill(0)
    for (let y = 0; y < plotH; y++) {
      const v = dens[y]
      if (v === 0) continue
      for (let k = -KERNEL_R; k <= KERNEL_R; k++) {
        const yy = y + k
        if (yy >= 0 && yy < plotH) blurred[yy] += v * KERNEL[k + KERNEL_R]
      }
    }
    for (let y = 0; y < plotH; y++) {
      const frac = blurred[y] / total
      if (frac <= 0) continue
      const a = 1 - Math.exp(-ALPHA_K * frac)
      const p = 4 * (y * plotW + x)
      img.data[p] = smoke[0]
      img.data[p + 1] = smoke[1]
      img.data[p + 2] = smoke[2]
      img.data[p + 3] = Math.min(255, Math.round(a * 255))
    }
  }
  ctx.putImageData(img, AXIS_W, 0)

  // y grid + labels
  ctx.font = '10px system-ui, sans-serif'
  ctx.fillStyle = text
  ctx.strokeStyle = grid
  ctx.textAlign = 'right'
  ctx.textBaseline = 'middle'
  for (let i = 0; i <= 4; i++) {
    const frac = i / 4
    const y = (plotH - 1) * (1 - frac)
    ctx.beginPath()
    ctx.moveTo(AXIS_W, y + 0.5)
    ctx.lineTo(w, y + 0.5)
    ctx.stroke()
    const label = log && i === 0 ? fmtUs(ymin) : fmtUs(fracVal(frac))
    ctx.fillText(label, AXIS_W - 6, Math.min(Math.max(y, 6), plotH - 6))
  }

  // time labels
  ctx.textAlign = 'center'
  ctx.textBaseline = 'top'
  for (let i = 0; i <= 4; i++) {
    const t = t0 + ((t1 - t0) * i) / 4
    const x = AXIS_W + (plotW * i) / 4
    const dte = new Date(t * 1000)
    const label = `${String(dte.getHours()).padStart(2, '0')}:${String(dte.getMinutes()).padStart(2, '0')}`
    ctx.fillText(label, Math.min(Math.max(x, AXIS_W + 16), w - 16), h - AXIS_H + 4)
  }

  // median line, breaking at gaps
  ctx.strokeStyle = medianColor
  ctx.lineWidth = 1.25
  ctx.beginPath()
  let pen = false
  for (let x = 0; x < plotW; x++) {
    if (Number.isNaN(median[x])) {
      pen = false
      continue
    }
    const y = (plotH - 1) * (1 - Math.min(yFrac(median[x]), 1))
    if (pen) ctx.lineTo(AXIS_W + x + 0.5, y)
    else ctx.moveTo(AXIS_W + x + 0.5, y)
    pen = true
  }
  ctx.stroke()

  // loss rail: 0% stays background so the rail is silent when all is well
  const railY = plotH + RAIL_GAP
  for (let x = 0; x < plotW; x++) {
    const l = loss[x]
    if (Number.isNaN(l) || l <= 0) continue
    const [r, g, b] = viridis(l)
    ctx.fillStyle = `rgb(${r | 0},${g | 0},${b | 0})`
    ctx.fillRect(AXIS_W + x, railY, 1, RAIL_H)
  }
  ctx.fillStyle = text
  ctx.textAlign = 'right'
  ctx.textBaseline = 'middle'
  ctx.fillText('loss', AXIS_W - 6, railY + RAIL_H / 2)

  postMessage({ type: 'rendered' })
}

// The per-row sample lists are sorted; a full ascending sort of the
// concatenation is still cheap at these sizes and only needed for ymax.
// TypedArray.sort() is numeric.
function sortedCopy(values: Uint32Array): Uint32Array {
  const copy = values.slice()
  copy.sort()
  return copy
}
