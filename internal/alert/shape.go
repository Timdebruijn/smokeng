package alert

import (
	"math"
	"sort"

	"github.com/timdebruijn/smokeng/internal/shape"
)

// Baselined is a captured reference distribution for a golden-baseline shape
// rule: the known-good shape an operator recorded, so a drift away from how a
// path was commissioned is what fires, rather than a drift from whatever it has
// been doing lately. What it was taken from is kept with it — an anonymous curve
// nobody can trace back is not evidence of anything.
type Baselined struct {
	RuleID     int64    `json:"rule_id"`
	TargetID   int64    `json:"target_id"`
	AgentID    int64    `json:"agent_id"`
	FromTS     int64    `json:"from_ts"`
	ToTS       int64    `json:"to_ts"`
	Intervals  int      `json:"intervals"`
	Samples    []uint32 `json:"-"`
	CapturedAt int64    `json:"captured_at"`
	CapturedBy string   `json:"captured_by"`
}

// How much history a shape rule keeps, and how much it needs before it will fire.
// These warm-up periods mean a shape rule is quiet for the first stretch after a
// (re)start rather than firing on a baseline it has not seen enough of — a cold
// start is not an anomaly.
const (
	shapeWindow      = 30 // rolling baseline: recent intervals pooled as "normal"
	shapeMinBaseline = 10 // rolling: intervals of history before the first verdict
	shapeDivHistory  = 60 // auto: recent divergences kept to calibrate against
	shapeMinDivs     = 20 // auto: divergences before a z-score means anything
)

// shapeState is a shape rule's per-series memory: the recent distributions it
// pools for a rolling baseline, and the recent divergences it calibrates a
// z-score against. It is not persisted — a restart re-warms it — and every
// method runs under the manager's lock.
type shapeState struct {
	samples [][]uint32 // recent per-interval samples, oldest first, capped
	divs    []float64  // recent Wasserstein divergences in ms, capped
}

// value computes the rule's shape measure for the current interval and updates
// the buffers. golden is the reference for a golden-baseline rule (nil when none
// is captured). ok is false while warming up, or when there is nothing to
// compare against — the caller treats that as a non-matching interval, so a cold
// start or a missing golden baseline never fires.
func (ss *shapeState) value(r *Rule, m *Input, golden []uint32) (float64, bool) {
	switch r.Metric {
	case MetricBimodality:
		// Baseline-free: bimodality is a property of the current interval.
		return shape.BimodalityCoefficient(m.Samples)
	case MetricShape:
		var baseline []uint32
		if r.Baseline == BaselineGolden {
			baseline = golden
		} else {
			baseline = ss.pooled()
		}
		// Record the current interval regardless of baseline mode, so a rolling
		// baseline builds and so switching a rule to rolling later has history.
		defer ss.push(m.Samples)

		if len(m.Samples) == 0 || len(baseline) == 0 {
			return 0, false
		}
		if r.Baseline == BaselineRolling && len(ss.samples) < shapeMinBaseline {
			return 0, false // warming up
		}
		divMS := shape.Wasserstein1(m.Samples, baseline) / 1000
		if r.Mode == ModeTunable {
			return divMS, true
		}
		z, ok := robustZ(divMS, ss.divs)
		ss.pushDiv(divMS)
		return z, ok
	}
	return 0, false
}

// pooled concatenates the buffered intervals into one baseline distribution.
// Wasserstein1 sorts internally, so order does not matter here.
func (ss *shapeState) pooled() []uint32 {
	n := 0
	for _, s := range ss.samples {
		n += len(s)
	}
	out := make([]uint32, 0, n)
	for _, s := range ss.samples {
		out = append(out, s...)
	}
	return out
}

func (ss *shapeState) push(samples []uint32) {
	cp := make([]uint32, len(samples))
	copy(cp, samples)
	ss.samples = append(ss.samples, cp)
	if len(ss.samples) > shapeWindow {
		ss.samples = ss.samples[len(ss.samples)-shapeWindow:]
	}
}

func (ss *shapeState) pushDiv(d float64) {
	ss.divs = append(ss.divs, d)
	if len(ss.divs) > shapeDivHistory {
		ss.divs = ss.divs[len(ss.divs)-shapeDivHistory:]
	}
}

// robustZ scores x against the history's own spread, using the median and the
// median absolute deviation rather than the mean and standard deviation, so a
// past spike does not inflate the scale and mask the next one. ok is false until
// there is enough history. A perfectly flat history has no scale to judge by;
// rather than treat any wobble as infinite, it declines to fire — a monitoring
// tool that cries wolf is ignored, and this errs towards precision.
func robustZ(x float64, hist []float64) (float64, bool) {
	if len(hist) < shapeMinDivs {
		return 0, false
	}
	med := median(hist)
	dev := make([]float64, len(hist))
	for i, v := range hist {
		dev[i] = math.Abs(v - med)
	}
	scale := 1.4826 * median(dev)
	if scale == 0 {
		scale = stddev(hist)
	}
	if scale == 0 {
		return 0, true
	}
	return (x - med) / scale, true
}

func median(x []float64) float64 {
	if len(x) == 0 {
		return 0
	}
	c := make([]float64, len(x))
	copy(c, x)
	sort.Float64s(c)
	n := len(c)
	if n%2 == 1 {
		return c[n/2]
	}
	return (c[n/2-1] + c[n/2]) / 2
}

func stddev(x []float64) float64 {
	if len(x) < 2 {
		return 0
	}
	var mean float64
	for _, v := range x {
		mean += v
	}
	mean /= float64(len(x))
	var s float64
	for _, v := range x {
		d := v - mean
		s += d * d
	}
	return math.Sqrt(s / float64(len(x)))
}
