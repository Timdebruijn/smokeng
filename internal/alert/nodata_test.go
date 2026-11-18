package alert

import "testing"

// noData is an interval in which the prober could not ask the target anything
// (store: sent = 0, flagged not sent).
func noData(step int) *Input {
	return &Input{TS: int64(1_756_400_000 + step*60), LossTrusted: true, RTTTrusted: true}
}

// Protects the rule that an interval without data has no value for any metric,
// so that it can neither be read as 0% loss (a clean interval) nor as 100%.
func TestNoDataHasNoValueForAnyMetric(t *testing.T) {
	for _, metric := range []Metric{MetricLoss, MetricMedian, MetricP95, MetricSpread} {
		if v, ok := Value(noData(0), metric); ok {
			t.Errorf("%v: value %v for an interval that sent nothing", metric, v)
		}
	}
}

// Protects an alert from clearing on, or being raised by, a stretch of no data:
// a firing loss rule stays firing through it, because a host that lost its
// route has shown nothing about the loss, and a quiet one stays quiet. The
// latency rule gets the same treatment.
func TestNoDataCountsNeitherWay(t *testing.T) {
	for _, r := range []*Rule{
		lossRule(),
		// The default measurement has 1..10 ms samples, a median above 1 ms.
		{ID: 2, Name: "slow", Metric: MetricMedian, Op: OpGreater, Threshold: 1, For: 3, ClearFor: 2, Enabled: true},
	} {
		st := &State{}
		for i := range 3 {
			Evaluate(r, st, m(i, 10, 5, clean), 60) // 50% loss, and a slow median
		}
		if !st.Firing {
			t.Fatalf("%s: setup did not fire", r.Name)
		}
		for i := 3; i < 3+10; i++ {
			if tr := Evaluate(r, st, noData(i), 60); tr != NoChange {
				t.Fatalf("%s: transition %v on a no-data interval", r.Name, tr)
			}
		}
		if !st.Firing {
			t.Errorf("%s: cleared on intervals that sent nothing", r.Name)
		}
	}

	quiet, st := lossRule(), &State{}
	for i := range 10 {
		if tr := Evaluate(quiet, st, noData(i), 60); tr != NoChange {
			t.Fatalf("a quiet rule changed state on no data: %v", tr)
		}
	}
}

// Protects the rolling shape baseline: an empty interval pushed into it would
// displace a real one, so a prober without a route would slowly replace the
// baseline with nothing.
func TestNoDataDoesNotEnterTheShapeBaseline(t *testing.T) {
	r := &Rule{ID: 3, Name: "shape", Metric: MetricShape, Op: OpGreater, Threshold: 1,
		For: 3, ClearFor: 3, Enabled: true, Baseline: BaselineRolling, Mode: ModeTunable}
	app := &Applicable{IntervalS: 60}
	var s Series
	for i := range 5 {
		s.Step(r, m(i, 10, 10, clean), nil, app)
	}
	before := len(s.shape.samples)
	if before != 5 {
		t.Fatalf("baseline holds %d intervals, want 5", before)
	}
	for i := 5; i < 8; i++ {
		s.Step(r, noData(i), nil, app)
	}
	if got := len(s.shape.samples); got != before {
		t.Errorf("baseline went from %d to %d intervals across no data", before, got)
	}
}
