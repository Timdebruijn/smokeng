package alert

import "github.com/timdebruijn/smokeng/internal/tree"

// step advances one rule's state for one series by one measurement and reports
// what changed. It is the whole of evaluation — a shape metric's value read
// against history, then hysteresis — with no lock, no clock and no I/O, so the
// manager and a replay over stored history run the same code rather than two
// implementations that agree until they do not.
//
// ss is the series' shape memory; only the shape metrics read it, and it may be
// nil for the others. golden is the rule's captured reference, nil when there
// is none. app is the target's resolved context: it carries the interval and
// the positional-loss policy, so that neither caller can forget either.
func step(r *Rule, st *State, ss *shapeState, m *Input, golden []uint32, app *Applicable) Transition {
	intervalS := app.IntervalS
	if r.Metric.IsShape() {
		// An interval that sent nothing has no shape, and pushing it would put an
		// empty distribution into the rolling baseline in place of a real one.
		// Like any interval without a value it breaks continuity and counts
		// neither way.
		if m.Sent == 0 {
			return EvaluateValue(r, st, 0, false, m.RTTTrusted, m.TS, intervalS)
		}
		// Shape metrics are computed from history, not from one interval: the
		// baseline and divergence buffers live in ss, and the value feeds the
		// same hysteresis every other rule uses.
		value, ok := ss.value(r, m, golden)
		return EvaluateValue(r, st, value, ok, m.RTTTrusted, m.TS, intervalS)
	}
	if r.Metric == MetricLoss && app.ExcludePositionalLoss && m.PositionalLoss {
		// Loss that the target's own rate limiter produces is not loss on the
		// network, and a rule that pages on it pages about the target's
		// configuration. So the rule reads the interval as having lost nothing.
		// Skipping it instead, as an interval with dropped replies is skipped,
		// leaves an alert that fired before the detector had seen enough firing
		// for as long as the limiter keeps hitting: nothing would ever count
		// towards clearing it. The round-trip times of such an interval are
		// real, so every other metric reads it as usual. The state still carries
		// the loss that was measured, so a notification says what happened and
		// not what the rule counted.
		measured, ok := Value(m, MetricLoss)
		return evaluate(r, st, 0, measured, ok, m.LossTrusted, m.TS, intervalS)
	}
	return Evaluate(r, st, m, intervalS)
}

// Series is what one rule remembers about one series: its hysteresis state and,
// for a shape rule, the history it compares against. The manager keeps the two
// apart because it persists one and not the other; this is the pair for a caller
// that keeps neither, such as a replay.
type Series struct {
	State State
	shape shapeState
}

// Step advances the series by one measurement, exactly as the manager does for
// a live one. Measurements must be given oldest first.
func (s *Series) Step(r *Rule, m *Input, golden []uint32, app *Applicable) Transition {
	return step(r, &s.State, &s.shape, m, golden, app)
}

// WarmupIntervals is how many intervals before a moment a rule's verdict at
// that moment can depend on, apart from its own hysteresis: the rolling
// baseline it pools, and the divergences an auto rule calibrates against, each
// of which was itself measured against the baseline before it. Zero for
// everything that reads one interval at a time.
func (r *Rule) WarmupIntervals() int {
	if r.Metric != MetricShape {
		return 0
	}
	n := 0
	if r.Baseline == BaselineRolling {
		n += shapeWindow
	}
	if r.Mode == ModeAuto {
		n += shapeDivHistory
	}
	return n
}

// Applicable is a leaf target's resolved alerting context: the rules that reach
// it and what a notification needs to name it.
type Applicable struct {
	Rules     []*Rule
	IntervalS int
	Path      string
	Host      string
	// Family is the address family the target is measured over.
	Family tree.Family
	// ExcludePositionalLoss is the target's positional_loss setting: true when a
	// rule on loss reads an interval flagged as losing at fixed send positions
	// as one without loss.
	ExcludePositionalLoss bool
}

// Resolve works out which rules reach each enabled leaf. Rules inherit like
// every other setting and replace rather than accumulate: the nearest ancestor
// that defines any rules defines the whole set for its subtree, so a child's
// rules are a complete override, and there is no puzzle about how to remove an
// inherited one. The rules in the result point into the slice given.
func Resolve(targets []tree.Target, rules []Rule) (map[int64]Applicable, error) {
	tr, err := tree.New(targets)
	if err != nil {
		return nil, err
	}
	byNode := map[int64][]*Rule{}
	for i := range rules {
		r := &rules[i]
		byNode[r.TargetID] = append(byNode[r.TargetID], r)
	}
	resolved := map[int64]Applicable{}
	for i := range targets {
		n := &targets[i]
		if n.Host == nil || !n.Enabled {
			continue
		}
		res, err := tr.Resolve(n.ID)
		if err != nil {
			return nil, err
		}
		path, err := tr.Path(n.ID)
		if err != nil {
			return nil, err
		}
		app := Applicable{
			IntervalS: res.IntervalS.Effective, Path: path, Host: *n.Host, Family: n.Family(),
			ExcludePositionalLoss: res.PositionalLoss.Effective == tree.PositionalExclude,
		}
		if node, ok := NearestRuleNode(tr, func(id int64) bool { return len(byNode[id]) > 0 }, n.ID); ok {
			app.Rules = byNode[node]
		}
		resolved[n.ID] = app
	}
	return resolved, nil
}

// Ancestors maps each target id to the set of its ancestor ids, itself
// included, so a silence scoped to a group can be matched against any leaf
// beneath it by a map lookup rather than a walk.
func Ancestors(targets []tree.Target) map[int64]map[int64]bool {
	byNodeID := map[int64]*tree.Target{}
	for i := range targets {
		byNodeID[targets[i].ID] = &targets[i]
	}
	ancestors := map[int64]map[int64]bool{}
	for i := range targets {
		set := map[int64]bool{}
		for cur := &targets[i]; cur != nil; {
			set[cur.ID] = true
			if cur.ParentID == nil {
				break
			}
			cur = byNodeID[*cur.ParentID]
		}
		ancestors[targets[i].ID] = set
	}
	return ancestors
}
