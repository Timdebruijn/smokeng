// Package alert evaluates alert rules against measurements and reports state
// changes (DESIGN.md §4.3, roadmap v0.3).
//
// Rules are edge-triggered with hysteresis in both directions: a condition
// must hold for a number of consecutive intervals before it fires, and fail
// for a number of consecutive intervals before it clears. A naive
// instantaneous threshold would flap on every blip, which is worse than no
// alerting — SmokePing's matchers are the bar to clear, not a floor to sink
// below.
//
// Because smokeng keeps the whole distribution rather than one number per
// check, rules can also be written against p95 and spread, which a tool
// storing a single RTT per interval cannot express at all.
package alert

import (
	"fmt"
	"math"
)

// Input is the part of a measurement that alerting reads. Keeping it narrow
// is what lets the persistence layer depend on this package rather than the
// other way round, and it forces the caller to state explicitly whether the
// underlying measurement can be trusted for each kind of question.
type Input struct {
	TargetID, AgentID int64
	TS                int64
	Sent, Received    int
	Samples           []uint32 // RTTs in µs, sorted ascending
	// LossTrusted is false when the loss figure includes replies smokeng
	// itself dropped; RTTTrusted is false when a clock step made the RTTs
	// unreliable. Alerting on either would page someone about smokeng rather
	// than about the network.
	LossTrusted, RTTTrusted bool
}

// Metric is the quantity a rule tests.
type Metric string

const (
	// MetricLoss is the packet loss percentage, 0 to 100.
	MetricLoss Metric = "loss"
	// MetricMedian is the median RTT in milliseconds.
	MetricMedian Metric = "median"
	// MetricP95 is the 95th percentile RTT in milliseconds.
	MetricP95 Metric = "p95"
	// MetricSpread is p95 minus p5 in milliseconds: the width of the
	// distribution, which is what the smoke draws and what a single-value
	// tool cannot measure.
	MetricSpread Metric = "spread"
)

// Op is the comparison a rule applies.
type Op string

const (
	OpGreater Op = ">"
	OpLess    Op = "<"
)

// Rule is one alert condition, defined on a tree node and inherited by its
// descendants. Inheritance replaces rather than accumulates, consistently
// with every other inheritable setting: a node that defines rules defines the
// whole set for its subtree.
type Rule struct {
	ID       int64
	TargetID int64 // the node the rule is defined on
	Name     string
	Metric   Metric
	Op       Op
	// Threshold is a percentage for loss, milliseconds for latency metrics.
	Threshold float64
	// For is how many consecutive matching intervals must pass before the
	// rule fires; ClearFor how many non-matching before it clears again.
	For      int
	ClearFor int
	Enabled  bool
}

// Validate reports whether the rule is usable. Rules come from operators, so
// the message names what to fix.
func (r *Rule) Validate() error {
	switch r.Metric {
	case MetricLoss, MetricMedian, MetricP95, MetricSpread:
	default:
		return fmt.Errorf("alert: unknown metric %q (want loss, median, p95 or spread)", r.Metric)
	}
	if r.Op != OpGreater && r.Op != OpLess {
		return fmt.Errorf("alert: unknown comparison %q (want > or <)", r.Op)
	}
	if r.Name == "" {
		return fmt.Errorf("alert: name is required")
	}
	if math.IsNaN(r.Threshold) || math.IsInf(r.Threshold, 0) {
		return fmt.Errorf("alert: threshold must be a finite number")
	}
	if r.Metric == MetricLoss && (r.Threshold < 0 || r.Threshold > 100) {
		return fmt.Errorf("alert: loss threshold must be a percentage between 0 and 100")
	}
	if r.For < 1 {
		return fmt.Errorf("alert: `for` must be at least 1 interval")
	}
	if r.ClearFor < 1 {
		return fmt.Errorf("alert: `clear_for` must be at least 1 interval")
	}
	return nil
}

// State is a rule's standing for one series. It is persisted so that
// hysteresis survives a restart: an alert that has been firing for an hour
// must not resolve and re-fire because smokeng was restarted.
type State struct {
	RuleID   int64
	TargetID int64
	AgentID  int64
	Firing   bool
	// Since is the interval at which the current firing period began.
	Since int64
	// Streak counts consecutive intervals pushing towards the opposite
	// state: matching while clear, non-matching while firing.
	Streak int
	LastTS int64
	// Value is the metric value at the most recent evaluation, carried into
	// the notification so it says what actually happened.
	Value float64
}

// Transition is what an evaluation did to a rule's state.
type Transition int

const (
	NoChange Transition = iota
	Fired
	Resolved
)

// Value extracts the metric from a measurement. ok is false when the
// measurement cannot support it — an interval with no replies has no median.
func Value(m *Input, metric Metric) (v float64, ok bool) {
	switch metric {
	case MetricLoss:
		if m.Sent == 0 {
			return 0, false
		}
		return 100 * float64(m.Sent-m.Received) / float64(m.Sent), true
	case MetricMedian:
		return percentileMS(m.Samples, 0.5)
	case MetricP95:
		return percentileMS(m.Samples, 0.95)
	case MetricSpread:
		hi, ok1 := percentileMS(m.Samples, 0.95)
		lo, ok2 := percentileMS(m.Samples, 0.05)
		if !ok1 || !ok2 {
			return 0, false
		}
		return hi - lo, true
	}
	return 0, false
}

// percentileMS reads a percentile off the sorted sample list, in ms.
func percentileMS(sorted []uint32, q float64) (float64, bool) {
	if len(sorted) == 0 {
		return 0, false
	}
	i := int(q * float64(len(sorted)))
	if i >= len(sorted) {
		i = len(sorted) - 1
	}
	return float64(sorted[i]) / 1000, true
}

// trustworthy reports whether a measurement can carry this metric honestly.
func trustworthy(m *Input, metric Metric) bool {
	if metric == MetricLoss {
		return m.LossTrusted
	}
	return m.RTTTrusted
}

// Evaluate advances a rule's state by one measurement and reports whether it
// changed. intervalS is the target's interval, used to notice that the series
// has a gap: consecutive-interval counting means nothing across missing data,
// so a gap resets the streak rather than silently bridging it.
func Evaluate(r *Rule, st *State, m *Input, intervalS int) Transition {
	if !r.Enabled {
		return NoChange
	}
	// A gap, or an untrustworthy measurement, breaks continuity. Neither
	// counts towards firing or clearing.
	gap := st.LastTS != 0 && m.TS-st.LastTS > int64(2*intervalS)
	value, ok := Value(m, r.Metric)
	if gap || !ok || !trustworthy(m, r.Metric) {
		st.Streak = 0
		st.LastTS = m.TS
		return NoChange
	}
	st.LastTS = m.TS
	st.Value = value

	matches := value > r.Threshold
	if r.Op == OpLess {
		matches = value < r.Threshold
	}

	// The streak always counts towards changing state, so it is reset by
	// anything that argues for staying put.
	if matches == st.Firing {
		st.Streak = 0
		return NoChange
	}
	st.Streak++

	if !st.Firing && st.Streak >= r.For {
		st.Firing = true
		st.Since = m.TS
		st.Streak = 0
		return Fired
	}
	if st.Firing && st.Streak >= r.ClearFor {
		st.Firing = false
		st.Since = 0
		st.Streak = 0
		return Resolved
	}
	return NoChange
}

// Describe renders the rule's condition the way an operator wrote it.
func (r *Rule) Describe() string {
	unit := "ms"
	if r.Metric == MetricLoss {
		unit = "%"
	}
	return fmt.Sprintf("%s %s %g%s for %d intervals", r.Metric, r.Op, r.Threshold, unit, r.For)
}
