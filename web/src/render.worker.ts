/**
 * Density renderer (DESIGN.md §8.2). Runs in a worker on an OffscreenCanvas
 * so the main thread never blocks. Per pixel column: pool the samples of
 * every interval overlapping that column's time span, deposit them as
 * impulses in pixel space, run one 1-D Gaussian blur, map density to alpha,
 * and write the column into a single ImageData. The median line comes from
 * the pooled samples — never a median of medians — and gaps stay gaps: no
 * interpolation across missing buckets.
 *
 * All geometry arrives in CSS pixels and is scaled by devicePixelRatio here,
 * so text, line weights and the blur keep their intended size on any display.
 */
import { AXIS_W, AXIS_H, RAIL_H, RAIL_GAP, fmtSigned, fmtUs, inferSpan } from './layout'

interface SeriesMsg {
  ts: Float64Array
  sent: Float64Array
  received: Float64Array
  offsets: Uint32Array
  values: Uint32Array | Int32Array
}

interface ViewMsg {
  cssW: number
  cssH: number
  dpr: number
  t0: number
  t1: number
  dark: boolean
  log: boolean
  /**
   * A signed series (inter-packet delay variation) rather than a duration.
   * The scale becomes linear and symmetric about zero, with the zero line
   * drawn: on a jitter plot the sign is the point — a packet that caught up
   * with its predecessor and one that fell behind are different events, and a
   * scale that started at zero would put them on top of each other.
   */
  signed?: boolean
  /**
   * Draw the loss rail. Off for the extra series, where it would repeat the
   * round trip's loss under a graph that did not measure it.
   */
  rail?: boolean
}

// Density→alpha: count-normalized fixed curve alpha = 1 − exp(−K·fraction)
// (mode (b) of the design's trade-off #2), so columns stay comparable over
// time no matter how many samples they pool.
const ALPHA_K = 14
const SIGMA_CSS = 1.1

let canvas: OffscreenCanvas | null = null

self.onmessage = (ev: MessageEvent) => {
  const msg = ev.data
  if (msg.type === 'init') {
    canvas = msg.canvas as OffscreenCanvas
  } else if (msg.type === 'render' && canvas) {
    render(canvas, msg.series as SeriesMsg, msg.view as ViewMsg)
  } else if (msg.type === 'clear' && canvas) {
    // A failed fetch on the main thread has nothing to render, and leaving
    // the previous render up would show old data under whatever axis labels
    // the main thread has since moved on to. Blank the canvas rather than
    // let a stale frame keep looking current.
    canvas.getContext('2d')!.clearRect(0, 0, canvas.width, canvas.height)
  }
}

let kernelCache: { sigma: number; radius: number; k: Float32Array } | null = null
function gaussianKernel(sigma: number) {
  if (kernelCache && kernelCache.sigma === sigma) return kernelCache
  const radius = Math.max(2, Math.ceil(sigma * 3))
  const k = new Float32Array(2 * radius + 1)
  let sum = 0
  for (let i = -radius; i <= radius; i++) {
    const v = Math.exp(-(i * i) / (2 * sigma * sigma))
    k[i + radius] = v
    sum += v
  }
  for (let i = 0; i < k.length; i++) k[i] /= sum
  kernelCache = { sigma, radius, k }
  return kernelCache
}

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

function quantile(sorted: Uint32Array | Int32Array, q: number): number {
  if (sorted.length === 0) return 0
  return sorted[Math.min(sorted.length - 1, Math.floor(q * sorted.length))]
}

function render(cv: OffscreenCanvas, d: SeriesMsg, view: ViewMsg) {
  const { cssW, cssH, dpr, t0, t1, dark, log } = view
  const signed = view.signed === true
  const showRail = view.rail !== false
  const w = Math.round(cssW * dpr)
  const h = Math.round(cssH * dpr)
  cv.width = w
  cv.height = h
  const ctx = cv.getContext('2d')!
  ctx.clearRect(0, 0, w, h)

  const axisW = Math.round(AXIS_W * dpr)
  const railH = showRail ? Math.round(RAIL_H * dpr) : 0
  const railGap = showRail ? Math.round(RAIL_GAP * dpr) : 0
  const axisH = Math.round(AXIS_H * dpr)
  const plotW = w - axisW
  const plotH = h - railH - railGap - axisH
  const n = d.ts.length
  if (plotW <= 0 || plotH <= 0) return

  // y scale. Log (the default): the whole range fits, outliers and all.
  // Linear: clip the top 0.5% so a single outlier can't crush the plot.
  const allSorted = sortedCopy(d.values)
  let ymax: number
  let ymin = 0
  if (signed) {
    // Symmetric about zero, sized from both tails so the two directions are
    // drawn to the same scale: a plot whose zero drifted off centre would make
    // a link that only ever bursts late look the same as one that jitters
    // evenly.
    const lo = Math.abs(quantile(allSorted, 0.005))
    const hi = Math.abs(quantile(allSorted, 0.995))
    ymax = Math.max(lo, hi) * 1.15
    if (!(ymax > 0)) ymax = 100 // a perfectly flat window still needs an axis
    ymin = -ymax
  } else if (log) {
    ymax = Math.max((allSorted[allSorted.length - 1] ?? 0) * 1.2, 1000)
    ymin = Math.max(quantile(allSorted, 0.005) * 0.7, 20)
  } else {
    ymax = Math.max(quantile(allSorted, 0.995) * 1.15, 1000)
  }
  const lmin = Math.log10(Math.max(ymin, 1))
  const lmax = Math.log10(ymax)
  const yFrac = (us: number): number =>
    signed
      ? (us - ymin) / (ymax - ymin)
      : log
        ? (Math.log10(Math.max(us, ymin)) - lmin) / (lmax - lmin)
        : Math.min(us, ymax) / ymax
  const fracVal = (f: number): number =>
    signed ? ymin + f * (ymax - ymin) : log ? Math.pow(10, lmin + f * (lmax - lmin)) : f * ymax
  // Samples outside the drawn range are left out of the density rather than
  // pinned to the edge, for the reason spelt out at the clip below. On a
  // signed scale that applies at both ends.
  const clipped = (v: number): boolean => (signed ? v < ymin || v > ymax : !log && v > ymax)

  const text = dark ? 'rgba(255,255,255,0.62)' : 'rgba(0,0,0,0.58)'
  const grid = dark ? 'rgba(255,255,255,0.09)' : 'rgba(0,0,0,0.08)'
  const smoke: [number, number, number] = dark ? [125, 185, 255] : [23, 80, 190]
  const medianColor = dark ? '#7dd3fc' : '#0369a1'

  // A row's ts is its interval START: it covers [ts, ts+span) and belongs in
  // every column that span overlaps.
  const dt = (t1 - t0) / plotW
  const span = inferSpan(d.ts, n)
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
  const { radius, k: KERNEL } = gaussianKernel(SIGMA_CSS * dpr)

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
        const v = d.values[j]
        // On the linear scale ymax is a clip, not the true max — it exists so
        // one outlier can't crush the rest of the plot. `Math.min(us, ymax)`
        // used to pin every sample above it to the same row at the axis edge,
        // which after the blur reads as a flat, solid density there: an
        // actual excursion becomes visually identical to a real plateau at
        // that latency. Leave clipped samples out of the density entirely —
        // still counted in the pooled median below, since that's a real
        // statistic and can legitimately sit above the drawn axis. The log
        // scale never clips (ymax there is sized from the data itself), so
        // this only applies on linear.
        if (clipped(v)) {
          pooled.push(v)
          continue
        }
        const y = (plotH - 1) * (1 - Math.min(yFrac(v), 1))
        const y0 = Math.floor(y)
        const f = y - y0
        if (y0 >= 0 && y0 < plotH) dens[y0] += 1 - f
        if (y0 + 1 >= 0 && y0 + 1 < plotH) dens[y0 + 1] += f
        total++
        pooled.push(v)
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
      for (let kk = -radius; kk <= radius; kk++) {
        const yy = y + kk
        if (yy >= 0 && yy < plotH) blurred[yy] += v * KERNEL[kk + radius]
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
  ctx.putImageData(img, axisW, 0)

  // y grid + labels
  ctx.font = `${Math.round(10 * dpr)}px system-ui, sans-serif`
  ctx.fillStyle = text
  ctx.strokeStyle = grid
  ctx.lineWidth = Math.max(1, Math.round(dpr / 2))
  ctx.textAlign = 'right'
  ctx.textBaseline = 'middle'
  for (let i = 0; i <= 4; i++) {
    const frac = i / 4
    const y = (plotH - 1) * (1 - frac)
    ctx.beginPath()
    ctx.moveTo(axisW, y + 0.5)
    ctx.lineTo(w, y + 0.5)
    ctx.stroke()
    const v = fracVal(frac)
    const label = log && !signed && i === 0 ? fmtUs(ymin) : signed ? fmtSigned(v) : fmtUs(v)
    ctx.fillText(label, axisW - 6 * dpr, Math.min(Math.max(y, 7 * dpr), plotH - 7 * dpr))
  }

  // time labels
  ctx.textAlign = 'center'
  ctx.textBaseline = 'top'
  for (let i = 0; i <= 4; i++) {
    const t = t0 + ((t1 - t0) * i) / 4
    const x = axisW + (plotW * i) / 4
    const dte = new Date(t * 1000)
    const label = `${String(dte.getHours()).padStart(2, '0')}:${String(dte.getMinutes()).padStart(2, '0')}`
    ctx.fillText(label, Math.min(Math.max(x, axisW + 18 * dpr), w - 18 * dpr), h - axisH + 4 * dpr)
  }

  // median line, breaking at gaps — and, on the linear scale, wherever the
  // median itself sits above the clip: `Math.min(yFrac(v), 1)` used to pin
  // that segment to the axis edge too, drawing a real excursion as a flat
  // run along the top exactly like the density above did. Treat it as a gap
  // instead of a value at ymax.
  ctx.strokeStyle = medianColor
  ctx.lineWidth = 1.25 * dpr
  ctx.beginPath()
  let pen = false
  for (let x = 0; x < plotW; x++) {
    if (Number.isNaN(median[x]) || clipped(median[x])) {
      pen = false
      continue
    }
    const y = (plotH - 1) * (1 - Math.min(yFrac(median[x]), 1))
    if (pen) ctx.lineTo(axisW + x + 0.5, y)
    else ctx.moveTo(axisW + x + 0.5, y)
    pen = true
  }
  ctx.stroke()

  // loss rail: 0% stays background, so the rail is silent when all is well
  const railY = plotH + railGap
  for (let x = 0; x < plotW; x++) {
    const l = loss[x]
    if (Number.isNaN(l) || l <= 0) continue
    const [r, g, b] = viridis(l)
    ctx.fillStyle = `rgb(${r | 0},${g | 0},${b | 0})`
    ctx.fillRect(axisW + x, railY, 1, railH)
  }
  ctx.fillStyle = text
  ctx.textAlign = 'right'
  ctx.textBaseline = 'middle'
  ctx.fillText('loss', axisW - 6 * dpr, railY + railH / 2)

  postMessage({ type: 'rendered' })
}

// Per-row sample lists are sorted; a full ascending sort of the concatenation
// is still cheap at these sizes and is only needed for the y scale.
// TypedArray.sort() is numeric.
// TypedArray.prototype.sort is numeric, not lexicographic like Array's, so
// this orders negative values correctly as well.
function sortedCopy<T extends Uint32Array | Int32Array>(values: T): T {
  const copy = values.slice() as T
  copy.sort()
  return copy
}
