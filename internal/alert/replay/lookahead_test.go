package replay_test

import (
	"context"
	"reflect"
	"sync"
	"testing"

	"github.com/timdebruijn/smokeng/internal/alert"
	"github.com/timdebruijn/smokeng/internal/alert/replay"
	"github.com/timdebruijn/smokeng/internal/store"
	"github.com/timdebruijn/smokeng/internal/tree"
)

func replayNames(t *testing.T, src replay.Source, names []string, from, to int) []trans {
	t.Helper()
	var out []trans
	for _, n := range names {
		out = append(out, transitions(run(t, src, opts(n, from, to)))...)
	}
	sortTrans(out)
	return out
}

// Protects against lookahead, by the strongest test there is: change the data
// after a time T beyond recognition and everything before T must come out
// unchanged — for a scalar rule and for the rolling shape rules, whose
// baselines are the obvious place for the future to leak into the past. Also a
// window that ends at T must agree with one that runs on.
func TestNoLookaheadTruncationInvariance(t *testing.T) {
	const T = 350
	names := []string{"loss", "p95", "smoke-roll", "smoke-auto"}
	mk := func(wild bool) *env {
		e := newEnv(t)
		e.everyKind()
		e.put(e.web, 0, gen(1, 600, func(i int) spec {
			if wild && i >= T {
				return spec{mean: 500, sd: 100, lossPct: 80}
			}
			j := i
			s := base(20)
			switch {
			case j >= 200 && j < 210:
				s.lossPct = 60
			case j >= 300 && j < 341:
				s.mean = 40
			}
			return s
		}))
		return e
	}
	a := before(replayNames(t, mk(false).st, names, 0, 600), at(T))
	b := before(replayNames(t, mk(true).st, names, 0, 600), at(T))
	if len(a) < 6 {
		t.Fatalf("only %d transitions before T: the test would prove nothing: %v", len(a), a)
	}
	if !reflect.DeepEqual(a, b) {
		t.Errorf("transitions before T changed with the data after it\nplain: %v\nwild:  %v", a, b)
	}
	c := before(replayNames(t, mk(false).st, names, 0, T), at(T))
	if !reflect.DeepEqual(a, c) {
		t.Errorf("a window ending at T disagrees with one running past it\nlong:  %v\nshort: %v", a, c)
	}
}

// Protects the same thing the other way: a path stable for a long time and then
// shifted at T must be caught within a few intervals of T by a rolling shape
// rule. A baseline that had seen the future would have absorbed the shift and
// stayed quiet.
func TestRollingShapeFiresPromptlyAtAShift(t *testing.T) {
	const T = 400
	e := newEnv(t)
	tun := e.rule(e.prod, "tun", alert.MetricShape, alert.OpGreater, 10, 2, 2)
	auto := e.rule(e.prod, "auto", alert.MetricShape, alert.OpGreater, 6, 2, 2)
	auto.Mode = alert.ModeAuto
	e.save(tun)
	e.save(auto)
	e.put(e.web, 0, gen(12, 460, func(i int) spec {
		if i >= T && i < T+50 {
			return spec{mean: 100, sd: 1}
		}
		return spec{mean: 20, sd: 1}
	}))
	for _, name := range []string{"tun", "auto"} {
		eps := run(t, e.st, opts(name, 0, 460)).Series[0].Episodes
		if len(eps) == 0 || eps[0].Start.Unix() < at(T) || eps[0].Start.Unix() > at(T+3) {
			t.Errorf("%s: episodes %+v, want a fire within 3 intervals after %d", name, eps, at(T))
		}
	}
}

// Protects the warm-up: starting the window later, in a quiet moment, must not
// change what happens from there on, because the rows before the window fill the
// shape buffers exactly as live rows would have. Without the warm-up the
// rolling and auto rules are blind for their first stretch and this differs.
func TestWarmupMakesTheStartOfTheWindowIrrelevant(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	rules := e.everyKind()
	e.fill()
	early, _ := e.st.QueryRange(ctx, e.web, 0, tree.FamilyV4, at(0), at(100))
	var ref []uint32
	for _, m := range early {
		ref = append(ref, m.Samples...)
	}
	slicesSort(ref)
	for _, r := range rules {
		if r.Name == "smoke-gold" {
			if err := e.st.SaveAlertBaseline(ctx, &alert.Baselined{RuleID: r.ID, TargetID: e.web,
				FromTS: at(0), ToTS: at(100), Intervals: 100, Samples: ref}); err != nil {
				t.Fatal(err)
			}
		}
	}
	names := []string{"loss", "p95", "bimodal", "smoke-auto", "smoke-roll", "smoke-gold"}
	const from = 255
	full := replayNames(t, e.st, names, 0, 600)
	late := replayNames(t, e.st, names, from, 600)
	var want []trans
	for _, x := range full {
		if x.TS >= at(from) {
			want = append(want, x)
		}
	}
	if len(want) < 6 || !reflect.DeepEqual(want, late) {
		t.Errorf("starting at %d changed the outcome\nfull (>= from): %v\nlate: %v", from, want, late)
	}
}

// Protects the guarantee that makes the command safe next to a server: with
// another handle writing measurements the whole time, a replay on a read-only
// handle succeeds and changes nothing — not the alert state, not the event log,
// not the measurements it read.
func TestReadOnlyBesideALiveWriter(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	loss := e.save(e.rule(e.prod, "loss", alert.MetricLoss, alert.OpGreater, 20, 2, 2))
	e.put(e.web, 0, lossPattern("ggbbbgggbbbggg"))
	if err := e.st.SaveAlertStates(ctx, []alert.State{{RuleID: loss.ID, TargetID: e.web, Firing: true, Since: at(3), Streak: 1, LastTS: at(5), Value: 100}}); err != nil {
		t.Fatal(err)
	}
	if err := e.st.RecordAlertEvents(ctx, []alert.Event{{TS: at(3), RuleID: loss.ID, TargetID: e.web, Firing: true, RuleName: "loss", Value: 100}}); err != nil {
		t.Fatal(err)
	}
	snapshot := func() (any, any, int) {
		st, err := e.st.ListAlertStates(ctx)
		if err != nil {
			t.Fatal(err)
		}
		ev, err := e.st.ListAlertEvents(ctx, 1000)
		if err != nil {
			t.Fatal(err)
		}
		ms, err := e.st.QueryRange(ctx, e.web, 0, tree.FamilyV4, 0, at(100))
		if err != nil {
			t.Fatal(err)
		}
		return st, ev, len(ms)
	}
	s0, ev0, n0 := snapshot()

	ro, err := store.OpenReadOnly(e.path)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() { // the "server": new intervals arriving beyond the window
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			m := store.Measurement{TargetID: e.web, AgentID: 0, Family: tree.FamilyV4, TS: at(1000 + i), Sent: 5, Received: 1,
				Samples: []uint32{5000}}
			if err := e.st.WriteMeasurements(ctx, []store.Measurement{m}); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	for i := 0; i < 20; i++ {
		rep, err := replay.Run(ctx, ro, opts("loss", 0, 14))
		if err != nil {
			close(stop)
			wg.Wait()
			t.Fatal(err)
		}
		if rep.Totals.Episodes != 2 {
			t.Errorf("run %d: %d episodes, want 2", i, rep.Totals.Episodes)
		}
	}
	close(stop)
	wg.Wait()

	s1, ev1, n1 := snapshot()
	if !reflect.DeepEqual(s0, s1) || !reflect.DeepEqual(ev0, ev1) || n0 != n1 {
		t.Errorf("a replay changed the database: states %v -> %v, events %v -> %v, rows %d -> %d", s0, s1, ev0, ev1, n0, n1)
	}
}
