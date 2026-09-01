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
	// MetricShape fires on a change in the shape of the distribution rather than
	// a level: the Wasserstein distance from a baseline (recent history, or a
	// captured reference) crossing a bound. It catches a path that shifts or
	// whose tail grows while no single percentile trips. Its value is a distance
	// in ms (tunable mode) or a robust z-score of that distance (auto mode).
	MetricShape Metric = "shape"
	// MetricBimodality fires when the distribution splits into two clusters — the
	// signature of load-balancing across unequal paths, or a flapping failover.
	// Its value is Sarle's bimodality coefficient of the current interval, 0..1;
	// no baseline is involved, as bimodality is a property of the interval alone.
	MetricBimodality Metric = "bimodality"
)

// Mode is how a shape rule decides what counts as anomalous. Auto self-calibrates
// against the series' own recent variability and needs no threshold from the
// operator; tunable compares the raw measure against a threshold they set.
type Mode string

const (
	ModeAuto    Mode = "auto"
	ModeTunable Mode = "tunable"
)

// Baseline is what a shape rule compares the current interval against. Rolling
// is the target's own recent history; golden is a reference captured once, e.g.
// at commissioning, so a drift from the known-good state is what fires.
type Baseline string

const (
	BaselineRolling Baseline = "rolling"
	BaselineGolden  Baseline = "golden"
)

// IsShape reports whether a metric is one of the distribution-shape detectors,
// which the manager computes from history rather than from one interval.
func (m Metric) IsShape() bool { return m == MetricShape || m == MetricBimodality }

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
	// Mode and Baseline apply only to shape metrics; they are empty for the
	// scalar ones. Mode is auto or tunable; Baseline is rolling or golden.
	Mode     Mode
	Baseline Baseline
}

// Validate reports whether the rule is usable. Rules come from operators, so
// the message names what to fix.
func (r *Rule) Validate() error {
	switch r.Metric {
	case MetricLoss, MetricMedian, MetricP95, MetricSpread:
	case MetricShape, MetricBimodality:
		// A shape rule fires on rising anomaly, never on "less than": a
		// distribution that is more like its baseline is not a fault.
		if r.Op != OpGreater {
			return fmt.Errorf("alert: a %s rule compares with > (rising anomaly), not %q", r.Metric, r.Op)
		}
		switch r.Mode {
		case ModeAuto, ModeTunable:
		default:
			return fmt.Errorf("alert: %s mode must be auto or tunable, got %q", r.Metric, r.Mode)
		}
		if r.Metric == MetricShape {
			switch r.Baseline {
			case BaselineRolling, BaselineGolden:
			default:
				return fmt.Errorf("alert: shape baseline must be rolling or golden, got %q", r.Baseline)
			}
		}
	default:
		return fmt.Errorf("alert: unknown metric %q", r.Metric)
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
	// AckedSince records the firing episode an acknowledgement applies to: it
	// equals Since when the alert is acknowledged. Tying the ack to the episode
	// is what makes it mute only this one — when the alert resolves and later
	// re-fires with a new Since, the ack no longer matches and the alert
	// demands attention again. Zero means unacknowledged. AckedAt and AckedBy
	// are for display and audit.
	AckedSince int64
	AckedAt    int64
	AckedBy    string
}

// Acked reports whether this state's current firing episode is acknowledged.
func (st *State) Acked() bool {
	return st.Firing && st.AckedSince != 0 && st.AckedSince == st.Since
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
	value, ok := Value(m, r.Metric)
	return EvaluateValue(r, st, value, ok, trustworthy(m, r.Metric), m.TS, intervalS)
}

// EvaluateValue is Evaluate with the metric value supplied rather than read from
// the measurement. It exists for the shape metrics, whose value the manager
// computes from history — a baseline the single Input does not carry — while the
// hysteresis, gap handling and state transitions stay identical to every other
// rule, so shape rules inherit For/ClearFor, acknowledge and delivery unchanged.
func EvaluateValue(r *Rule, st *State, value float64, ok, trusted bool, ts int64, intervalS int) Transition {
	if !r.Enabled {
		return NoChange
	}
	// A gap, or an untrustworthy measurement, breaks continuity. Neither
	// counts towards firing or clearing.
	gap := st.LastTS != 0 && ts-st.LastTS > int64(2*intervalS)
	if gap || !ok || !trusted {
		st.Streak = 0
		st.LastTS = ts
		return NoChange
	}
	st.LastTS = ts
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
		st.Since = ts
		st.Streak = 0
		return Fired
	}
	if st.Firing && st.Streak >= r.ClearFor {
		st.Firing = false
		st.Since = 0
		st.Streak = 0
		// The acknowledgement belonged to the episode that just ended. Clearing
		// it means a fresh fire of the same rule alerts again rather than
		// inheriting an ack from a problem that is over.
		st.AckedSince, st.AckedAt, st.AckedBy = 0, 0, ""
		return Resolved
	}
	return NoChange
}

// Describe renders the rule's condition the way an operator wrote it.
func (r *Rule) Describe() string {
	switch r.Metric {
	case MetricShape:
		if r.Mode == ModeAuto {
			return fmt.Sprintf("shape shift (%s baseline) beyond z %g for %d intervals",
				r.Baseline, r.Threshold, r.For)
		}
		return fmt.Sprintf("shape shift (%s baseline) > %g ms for %d intervals",
			r.Baseline, r.Threshold, r.For)
	case MetricBimodality:
		return fmt.Sprintf("bimodality > %g for %d intervals", r.Threshold, r.For)
	}
	unit := "ms"
	if r.Metric == MetricLoss {
		unit = "%"
	}
	return fmt.Sprintf("%s %s %g%s for %d intervals", r.Metric, r.Op, r.Threshold, unit, r.For)
}

// Event is one transition: a rule started firing, or stopped.
//
// The rule's name and description are copies, not references. A rule that is
// renamed or deleted must not silently rewrite what the record says happened —
// history whose meaning changes afterwards is not history.
type Event struct {
	ID        int64
	TS        int64 // unix seconds
	RuleID    int64
	TargetID  int64
	AgentID   int64
	Firing    bool
	RuleName  string
	Describes string
	Value     float64
}
