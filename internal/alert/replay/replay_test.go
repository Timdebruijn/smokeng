package replay_test

import (
	"context"
	"reflect"
	"slices"
	"testing"

	"github.com/timdebruijn/smokeng/internal/alert"
	"github.com/timdebruijn/smokeng/internal/alert/replay"
	"github.com/timdebruijn/smokeng/internal/store"
	"github.com/timdebruijn/smokeng/internal/tree"
)

// everyKind is one rule of each kind the engine has, on the Production group.
func (e *env) everyKind() []alert.Rule {
	gold := e.rule(e.prod, "smoke-gold", alert.MetricShape, alert.OpGreater, 8, 3, 3)
	gold.Baseline = alert.BaselineGolden
	auto := e.rule(e.prod, "smoke-auto", alert.MetricShape, alert.OpGreater, 8, 3, 3)
	auto.Mode = alert.ModeAuto
	return []alert.Rule{
		e.save(e.rule(e.prod, "loss", alert.MetricLoss, alert.OpGreater, 20, 2, 2)),
		e.save(e.rule(e.prod, "p95", alert.MetricP95, alert.OpGreater, 35, 3, 3)),
		e.save(e.rule(e.prod, "bimodal", alert.MetricBimodality, alert.OpGreater, 0.6, 3, 3)),
		e.save(auto),
		e.save(e.rule(e.prod, "smoke-roll", alert.MetricShape, alert.OpGreater, 8, 3, 3)),
		e.save(gold),
	}
}

// scenario is 600 minutes holding a loss episode, a latency shift, a bimodal
// stretch, a gap, and intervals flagged for a socket overflow and a clock step.
// shift moves the three episodes, so two agents do not agree.
func scenario(seed uint64, shift int) map[int64]row {
	return gen(seed, 600, func(i int) spec {
		j := i - shift
		s := base(20)
		switch {
		case j >= 200 && j < 210:
			s.lossPct = 60
		case j >= 300 && j < 341:
			s.mean = 40
		case j >= 401 && j < 431:
			s.mean, s.mean2 = 10, 60
		}
		switch {
		case i >= 431 && i < 451:
			s.skip = true
		case i >= 460 && i < 464:
			s.lossPct, s.flags = 75, store.FlagSocketOverflow
		case i >= 470 && i < 474:
			s.mean, s.flags = 200, store.FlagClockStep
		}
		return s
	})
}

func (e *env) fill() {
	e.put(e.web, 0, scenario(1, 0))
	e.put(e.web, e.ams, scenario(2, 37))
	e.put(e.db, 0, scenario(3, 11))
}

// Protects the premise of the whole feature: a replay is the live engine run
// over stored rows, so it must report exactly the transitions the manager
// produced when it saw the same rows as they arrived — across loss, latency,
// bimodality and every shape mode, over gaps and flagged intervals, for series
// from two agents.
func TestReplayMatchesLive(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	rules := e.everyKind()
	e.fill()

	rec := &recorder{}
	mgr := alert.NewManager(e.st, rec)
	if err := mgr.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	// The golden reference: the first stretch of web/local, as an operator
	// captures it at commissioning.
	var ref []uint32
	var goldenID int64
	for _, r := range rules {
		if r.Name == "smoke-gold" {
			goldenID = r.ID
		}
	}
	early, err := e.st.QueryRange(ctx, e.web, 0, tree.FamilyV4, at(0), at(100))
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range early {
		ref = append(ref, m.Samples...)
	}
	slices.Sort(ref)
	if err := mgr.CaptureBaseline(ctx, alert.Baselined{RuleID: goldenID, TargetID: e.web,
		FromTS: at(0), ToTS: at(100), Intervals: 100, Samples: ref}); err != nil {
		t.Fatal(err)
	}

	type key struct{ target, agent int64 }
	var all []store.Measurement
	for _, k := range []key{{e.web, 0}, {e.web, e.ams}, {e.db, 0}} {
		ms, err := e.st.QueryRange(ctx, k.target, k.agent, tree.FamilyV4, 0, at(1000))
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, ms...)
	}
	slices.SortStableFunc(all, func(a, b store.Measurement) int { return int(a.TS - b.TS) })
	for _, m := range all {
		mgr.Observe(ctx, []alert.Input{m.AlertInput()})
	}
	live := rec.got
	sortTrans(live)

	var got []trans
	for _, r := range rules {
		got = append(got, transitions(run(t, e.st, replay.Options{RuleName: r.Name, From: at(0), To: at(600)}))...)
	}
	sortTrans(got)

	perRule := map[string]int{}
	for _, x := range live {
		if x.Fired {
			perRule[x.Rule]++
		}
	}
	for _, r := range rules {
		if perRule[r.Name] == 0 {
			t.Errorf("the live engine never fired %q, so the comparison proves nothing for it", r.Name)
		}
	}
	if !reflect.DeepEqual(live, got) {
		t.Errorf("replay and live disagree\nlive:   %v\nreplay: %v", live, got)
	}
}
