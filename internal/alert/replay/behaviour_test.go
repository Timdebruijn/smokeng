package replay_test

import (
	"context"
	"errors"
	"math"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/timdebruijn/smokeng/internal/alert"
	"github.com/timdebruijn/smokeng/internal/alert/replay"
	"github.com/timdebruijn/smokeng/internal/store"
	"github.com/timdebruijn/smokeng/internal/tree"
)

// lossPattern builds a loss series: g good, b all lost, . missing.
func lossPattern(p string) map[int64]row {
	return gen(9, len(p), func(i int) spec {
		switch p[i] {
		case 'b':
			s := base(20)
			s.lossPct = 100
			return s
		case '.':
			return spec{skip: true}
		}
		return base(20)
	})
}

func opts(name string, from, to int) replay.Options {
	return replay.Options{RuleName: name, From: at(from), To: at(to)}
}

// Protects the hysteresis on both sides: a short blip never fires, a short
// recovery never resolves, and the transition lands on the interval that
// completes the count, as live does.
func TestHysteresisBothWays(t *testing.T) {
	e := newEnv(t)
	e.save(e.rule(e.prod, "loss", alert.MetricLoss, alert.OpGreater, 20, 3, 3))
	// blip (2 bad), fire at 11, 2 good then bad again (no resolve), resolve at 18
	e.put(e.web, 0, lossPattern("ggggbbgggbbbggbbggggggg"))
	rep := run(t, e.st, opts("loss", 0, 23))
	if len(rep.Series) != 1 || len(rep.Series[0].Episodes) != 1 {
		t.Fatalf("want one series with one episode, got %+v", rep.Series)
	}
	ep := rep.Series[0].Episodes[0]
	if ep.Start.Unix() != at(11) || ep.End == nil || ep.End.Unix() != at(18) || ep.Open {
		t.Errorf("episode = %v..%v open=%v, want %d..%d", ep.Start.Unix(), ep.End, ep.Open, at(11), at(18))
	}
	if ep.Seconds != 7*60 {
		t.Errorf("seconds = %d, want %d", ep.Seconds, 7*60)
	}
}

// Protects the quality flags: an interval whose loss is smokeng's own (socket
// overflow) must not count for loss, and one whose RTTs a clock step spoiled
// must not count for RTT metrics, or a replay would page about the tool. The
// same shapes unflagged are the control that these rules would fire.
func TestFlaggedIntervalsDoNotCount(t *testing.T) {
	e := newEnv(t)
	e.save(e.rule(e.prod, "loss", alert.MetricLoss, alert.OpGreater, 20, 2, 2))
	e.save(e.rule(e.prod, "p95", alert.MetricP95, alert.OpGreater, 35, 2, 2))
	mk := func(flagged bool) map[int64]row {
		return gen(4, 60, func(i int) spec {
			s := base(20)
			if i >= 20 && i < 30 {
				s.lossPct, s.mean = 80, 200
				if flagged {
					s.flags = store.FlagSocketOverflow
				}
			}
			if i >= 40 && i < 50 {
				s.mean = 200
				if flagged {
					s.flags = store.FlagClockStep
				}
			}
			return s
		})
	}
	e.put(e.web, 0, mk(true))
	e.put(e.db, 0, mk(false))
	loss := run(t, e.st, opts("loss", 0, 60))
	p95 := run(t, e.st, opts("p95", 0, 60))
	if n := len(find(t, loss, "/Production/web", "local").Episodes); n != 0 {
		t.Errorf("loss fired on %d flagged episodes", n)
	}
	if n := len(find(t, loss, "/Production/db", "local").Episodes); n != 1 {
		t.Errorf("loss on the unflagged control: %d episodes, want 1", n)
	}
	// Overflow only spoils loss and a clock step only RTT: the overflow rows'
	// 200 ms are still good RTTs (one p95 episode), the clock-step rows' are not.
	if n := len(find(t, p95, "/Production/web", "local").Episodes); n != 1 {
		t.Errorf("p95 on web: %d episodes, want 1 (the overflow stretch only)", n)
	}
	if n := len(find(t, p95, "/Production/db", "local").Episodes); n != 2 {
		t.Errorf("p95 on the unflagged control: %d episodes, want 2", n)
	}
}

func find(t *testing.T, r *replay.Report, target, vantage string) replay.SeriesReport {
	t.Helper()
	for _, s := range r.Series {
		if s.Target == target && s.Vantage == vantage {
			return s
		}
	}
	t.Fatalf("no series %s@%s in %d", target, vantage, len(r.Series))
	return replay.SeriesReport{}
}

// Protects the silence semantics: a silence is judged at the transition's own
// time (these windows are long past by the wall clock), scoped by subtree, and
// the three modes list, drop or ignore it as asked — including a window that
// covers a fire but not its resolve, and a silence on another subtree or
// another rule that must not cover anything.
func TestSilenceModes(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	loss := e.save(e.rule(e.prod, "loss", alert.MetricLoss, alert.OpGreater, 20, 2, 2))
	other := e.save(e.rule(e.prod, "other", alert.MetricMedian, alert.OpGreater, 900, 2, 2))
	p := []byte(strings.Repeat("g", 70))
	for _, i := range []int{10, 11, 12, 30, 31, 32, 50, 51, 52} {
		p[i] = 'b'
	}
	e.put(e.web, 0, lossPattern(string(p)))
	e.put(e.web, e.ams, lossPattern(string(p)))
	for _, s := range []alert.Silence{
		{AgentID: &e.ams, StartsAt: at(0), EndsAt: at(70)},    // one vantage, all of it
		{TargetID: &e.prod, StartsAt: at(5), EndsAt: at(20)},  // whole of episode 1
		{TargetID: &e.prod, StartsAt: at(28), EndsAt: at(33)}, // fire of episode 2 only
		{TargetID: &e.lab, StartsAt: at(45), EndsAt: at(60)},  // another subtree
		{RuleID: &other.ID, StartsAt: at(45), EndsAt: at(60)}, // another rule
	} {
		s.Reason, s.CreatedBy = "t", "t"
		if err := e.st.CreateSilence(ctx, &s); err != nil {
			t.Fatal(err)
		}
	}
	_ = loss

	o := opts("loss", 0, 70)
	rep := run(t, e.st, o)
	eps := find(t, rep, "/Production/web", "local").Episodes
	if a := find(t, rep, "/Production/web", "ams").Episodes; len(a) != 3 || !a[0].FireSilenced || !a[2].FireSilenced || !a[2].ResolveSilenced {
		t.Errorf("a silence scoped to agent ams must cover its series entirely: %+v", a)
	}
	if len(eps) != 3 || rep.Totals.Suppressed != 5 || rep.Silences != replay.SilenceReport {
		t.Fatalf("report mode: %d episodes, %d suppressed, mode %q", len(eps), rep.Totals.Suppressed, rep.Silences)
	}
	want := [][2]bool{{true, true}, {true, false}, {false, false}}
	for i, ep := range eps {
		if ep.FireSilenced != want[i][0] || ep.ResolveSilenced != want[i][1] {
			t.Errorf("episode %d: fire/resolve silenced = %v/%v, want %v", i, ep.FireSilenced, ep.ResolveSilenced, want[i])
		}
	}

	o.Silences = replay.SilenceRespect
	rep = run(t, e.st, o)
	if got := find(t, rep, "/Production/web", "local").Episodes; len(got) != 1 || got[0].Start.Unix() != at(51) ||
		rep.Totals.Episodes != 1 || rep.Totals.Suppressed != 5 || find(t, rep, "/Production/web", "local").Fired != 1 {
		t.Errorf("respect: %+v totals %+v", got, rep.Totals)
	}

	o.Silences = replay.SilenceIgnore
	rep = run(t, e.st, o)
	if rep.Totals.Episodes != 6 || rep.Totals.Suppressed != 0 {
		t.Errorf("ignore: %d episodes, %d suppressed", rep.Totals.Episodes, rep.Totals.Suppressed)
	}
	for _, s := range rep.Series {
		for _, ep := range s.Episodes {
			if ep.FireSilenced || ep.ResolveSilenced {
				t.Errorf("ignore mode consulted a silence: %+v", ep)
			}
		}
	}
}

// Protects the honest-quiet behaviour: a shape rule with less history than it
// wants is quiet and says how little it had, as live is after a restart — and
// with enough history the same data does fire, so the quiet is the warm-up and
// not the data.
func TestWarmupWithTooLittleHistory(t *testing.T) {
	e := newEnv(t)
	r := e.rule(e.prod, "smoke-auto", alert.MetricShape, alert.OpGreater, 6, 2, 2)
	r.Mode = alert.ModeAuto
	e.save(r)
	e.put(e.web, 0, gen(5, 120, func(i int) spec {
		if i >= 12 && i < 40 {
			return spec{mean: 80, sd: 1.5}
		}
		return base(20)
	}))
	e.put(e.db, 0, gen(6, 320, func(i int) spec {
		if i >= 270 && i < 290 {
			return spec{mean: 80, sd: 1.5}
		}
		return base(20)
	}))

	short := run(t, e.st, replay.Options{RuleName: "smoke-auto", From: at(10), To: at(120), Target: "/Production/web"})
	s := short.Series[0]
	if len(s.Episodes) != 0 {
		t.Errorf("fired with 10 of 90 warm-up intervals: %+v", s.Episodes)
	}
	if len(s.Warnings) != 1 || !strings.Contains(s.Warnings[0], "10 of 90") {
		t.Errorf("series warnings = %q, want one saying 10 of 90", s.Warnings)
	}
	if !slicesContain(short.Warnings, "10 of 90") {
		t.Errorf("top-level warnings = %q", short.Warnings)
	}

	full := run(t, e.st, replay.Options{RuleName: "smoke-auto", From: at(200), To: at(320), Target: "/Production/db"})
	if len(full.Series[0].Episodes) == 0 || len(full.Series[0].Warnings) != 0 {
		t.Errorf("with full warm-up: episodes %d, warnings %q; want a fire and no warning",
			len(full.Series[0].Episodes), full.Series[0].Warnings)
	}
}

func slicesContain(ss []string, sub string) bool {
	for _, s := range ss {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// Protects the golden-baseline warnings: no captured reference means the rule
// cannot fire (and says so, without failing); a reference captured inside the
// replayed window means the stretch is compared with itself.
func TestGoldenWarnings(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	g := e.rule(e.prod, "gold", alert.MetricShape, alert.OpGreater, 8, 2, 2)
	g.Baseline = alert.BaselineGolden
	g = e.save(g)
	e.put(e.web, 0, gen(8, 100, func(i int) spec {
		if i >= 60 {
			return spec{mean: 60, sd: 1.5}
		}
		return base(20)
	}))

	rep := run(t, e.st, opts("gold", 0, 100))
	if !slicesContain(rep.Warnings, "no captured reference") || rep.Totals.Episodes != 0 {
		t.Errorf("no reference: warnings %q, episodes %d", rep.Warnings, rep.Totals.Episodes)
	}

	ms, _ := e.st.QueryRange(ctx, e.web, 0, tree.FamilyV4, at(0), at(30))
	var ref []uint32
	for _, m := range ms {
		ref = append(ref, m.Samples...)
	}
	slicesSort(ref)
	save := func(from, to int64) {
		t.Helper()
		if err := e.st.SaveAlertBaseline(ctx, &alert.Baselined{RuleID: g.ID, TargetID: e.web,
			FromTS: from, ToTS: to, Intervals: 30, Samples: ref}); err != nil {
			t.Fatal(err)
		}
	}
	save(at(0), at(30)) // inside the replayed window
	rep = run(t, e.st, opts("gold", 0, 100))
	if !slicesContain(rep.Warnings, "compared with itself") {
		t.Errorf("overlapping reference: warnings %q", rep.Warnings)
	}
	if rep.Totals.Episodes == 0 {
		t.Error("a golden rule with a reference should fire on the shifted stretch")
	}

	rep = run(t, e.st, opts("gold", 40, 100)) // reference ends before the window
	if len(rep.Warnings) != 0 {
		t.Errorf("disjoint reference warned: %q", rep.Warnings)
	}
}

// Protects the clean start: hysteresis begins "not firing" at From, so a
// condition that already holds fires for_intervals into the window, not at its
// first row and not (as live would have) before it.
func TestCleanStart(t *testing.T) {
	e := newEnv(t)
	e.save(e.rule(e.prod, "loss", alert.MetricLoss, alert.OpGreater, 20, 3, 3))
	p := []byte(strings.Repeat("g", 100))
	for i := 40; i < 80; i++ {
		p[i] = 'b'
	}
	e.put(e.web, 0, lossPattern(string(p)))
	rep := run(t, e.st, opts("loss", 50, 100))
	eps := rep.Series[0].Episodes
	if len(eps) != 1 || eps[0].Start.Unix() != at(52) {
		t.Fatalf("episodes = %+v, want one starting at %d (3rd interval of the window)", eps, at(52))
	}

	// The same for a rule with warm-up, whose warm-up rows would otherwise
	// leave it already firing when the window opens: the hysteresis is reset,
	// the shape memory is not.
	e.save(e.rule(e.prod, "smoke", alert.MetricShape, alert.OpGreater, 10, 3, 3))
	e.put(e.db, 0, gen(10, 160, func(i int) spec {
		if i >= 100 && i < 140 {
			return spec{mean: 100, sd: 1}
		}
		return spec{mean: 20, sd: 1}
	}))
	rep = run(t, e.st, replay.Options{RuleName: "smoke", From: at(110), To: at(160), Target: "/Production/db"})
	if eps := rep.Series[0].Episodes; len(eps) == 0 || eps[0].Start.Unix() != at(112) {
		t.Errorf("shape rule: episodes %+v, want the first starting at %d", eps, at(112))
	}
}

// Protects the episode bookkeeping: crossing value is the value at the fire, the
// peak follows the rule's direction (max for >, min for <), an episode still
// firing at To stays open and counts to To.
func TestEpisodeValuesAndOpenEpisode(t *testing.T) {
	e := newEnv(t)
	e.save(e.rule(e.prod, "loss", alert.MetricLoss, alert.OpGreater, 20, 2, 2))
	e.save(e.rule(e.prod, "fast", alert.MetricMedian, alert.OpLess, 10, 2, 2))
	losses := []int{0, 30, 50, 90, 40, 0, 0, 0, 60, 60, 60}
	e.put(e.web, 0, gen(3, len(losses), func(i int) spec {
		s := base(20)
		s.lossPct = losses[i]
		return s
	}))
	medians := []float64{20, 9, 5, 2, 6, 20, 20, 20, 8, 7}
	e.put(e.db, 0, gen(3, len(medians), func(i int) spec { return spec{mean: medians[i], sd: 0.01} }))

	rep := run(t, e.st, opts("loss", 0, 11))
	s := find(t, rep, "/Production/web", "local")
	if len(s.Episodes) != 2 {
		t.Fatalf("loss episodes = %+v", s.Episodes)
	}
	a, b := s.Episodes[0], s.Episodes[1]
	if a.CrossedAt != 50 || a.Peak != 90 || a.End == nil || a.End.Unix() != at(6) {
		t.Errorf("first = %+v, want crossed 50, peak 90, ended at interval 6", a)
	}
	if !b.Open || b.End != nil || b.Start.Unix() != at(9) || b.Seconds != at(11)-at(9) {
		t.Errorf("open episode = %+v", b)
	}
	if s.Fired != 2 || s.Resolved != 1 || s.FiringSeconds != a.Seconds+b.Seconds || s.LongestSeconds != max(a.Seconds, b.Seconds) {
		t.Errorf("series counters = %+v", s)
	}

	rep = run(t, e.st, opts("fast", 0, 10))
	f := find(t, rep, "/Production/db", "local").Episodes
	if len(f) != 2 || math.Abs(f[0].CrossedAt-5) > 0.2 || math.Abs(f[0].Peak-2) > 0.2 {
		t.Errorf("a < rule must peak at the minimum: %+v", f)
	}
}

// Protects scope as after an import: a file rule on a node without stored rules
// takes over that subtree and nothing else; one replacing a stored rule takes
// its id (so rule-scoped silences and the golden baseline still apply) while a
// new one gets an id that cannot be mistaken for a stored rule's.
func TestFileRulesScope(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	stored := e.save(e.rule(e.prod, "loss", alert.MetricLoss, alert.OpGreater, 20, 2, 2))
	sg := e.rule(e.prod, "gold", alert.MetricShape, alert.OpGreater, 8, 2, 2)
	sg.Baseline = alert.BaselineGolden
	sg = e.save(sg)
	bad := gen(2, 40, func(i int) spec {
		s := base(20)
		if i >= 10 && i < 20 {
			s.lossPct, s.mean = 90, 60
		}
		return s
	})
	e.put(e.web, 0, bad)
	e.put(e.gw, 0, bad)
	ms, _ := e.st.QueryRange(ctx, e.web, 0, tree.FamilyV4, at(0), at(8))
	var ref []uint32
	for _, m := range ms {
		ref = append(ref, m.Samples...)
	}
	slicesSort(ref)
	if err := e.st.SaveAlertBaseline(ctx, &alert.Baselined{RuleID: sg.ID, TargetID: e.web,
		FromTS: at(0), ToTS: at(8), Intervals: 8, Samples: ref}); err != nil {
		t.Fatal(err)
	}
	if err := e.st.CreateSilence(ctx, &alert.Silence{RuleID: &stored.ID, StartsAt: at(0), EndsAt: at(40), Reason: "t", CreatedBy: "t"}); err != nil {
		t.Fatal(err)
	}

	t.Run("a node without stored rules", func(t *testing.T) {
		lab := e.rule(e.lab, "lab-loss", alert.MetricLoss, alert.OpGreater, 20, 2, 2)
		rep := run(t, e.st, replay.Options{FileRules: []alert.Rule{lab}, From: at(0), To: at(40)})
		if len(rep.Series) != 1 || rep.Series[0].Target != "/Lab/gw" || rep.Series[0].RuleNode != "/Lab" ||
			len(rep.Series[0].Episodes) != 1 {
			t.Errorf("series = %+v, want only /Lab/gw with one episode", rep.Series)
		}
	})

	t.Run("ids and baselines", func(t *testing.T) {
		repl := e.rule(e.prod, "loss", alert.MetricLoss, alert.OpGreater, 30, 2, 2)
		fresh := e.rule(e.prod, "fresh", alert.MetricLoss, alert.OpGreater, 20, 2, 2)
		kept := e.rule(e.prod, "gold", alert.MetricShape, alert.OpGreater, 6, 2, 2)
		kept.Baseline = alert.BaselineGolden
		lost := e.rule(e.prod, "gold2", alert.MetricShape, alert.OpGreater, 6, 2, 2)
		lost.Baseline = alert.BaselineGolden
		in := []alert.Rule{repl, fresh, kept, lost}
		rep := run(t, e.st, replay.Options{FileRules: in, From: at(0), To: at(40), Target: "/Production/web"})
		silenced := map[string]bool{}
		for _, s := range rep.Series {
			if len(s.Episodes) == 0 {
				if s.Rule != "gold2" { // no reference: cannot fire
					t.Errorf("%s produced no episode", s.Rule)
				}
				continue
			}
			silenced[s.Rule] = s.Episodes[0].FireSilenced
		}
		if !silenced["loss"] || silenced["fresh"] {
			t.Errorf("rule-scoped silence: loss=%v fresh=%v, want true/false (the replacement takes the stored id, a new rule must not)",
				silenced["loss"], silenced["fresh"])
		}
		if slicesContain(rep.Warnings, `"gold" on /Production is a golden`) || !slicesContain(rep.Warnings, `"gold2" on /Production is a golden`) {
			t.Errorf("golden warnings = %q: the replacement keeps its baseline, the new rule has none", rep.Warnings)
		}
		if in[0].ID != 0 {
			t.Error("Run changed the caller's rules")
		}
	})

	t.Run("target and vantage", func(t *testing.T) {
		e.put(e.web, e.ams, bad)
		e.put(e.db, 0, bad)
		rep := run(t, e.st, replay.Options{RuleName: "loss", From: at(0), To: at(40), Target: "/Production/web"})
		if len(rep.Series) != 2 || rep.Series[0].Vantage != "ams" || rep.Series[1].Vantage != "local" {
			t.Errorf("target filter: %+v", rep.Series)
		}
		rep = run(t, e.st, replay.Options{RuleName: "loss", From: at(0), To: at(40), Vantage: "ams"})
		if len(rep.Series) != 1 || rep.Series[0].Target != "/Production/web" {
			t.Errorf("vantage filter (db has no ams rows): %+v", rep.Series)
		}
		rep = run(t, e.st, replay.Options{RuleName: "loss", From: at(0), To: at(40)})
		var got []string
		for _, s := range rep.Series {
			got = append(got, s.Target+"@"+s.Vantage)
		}
		if !reflect.DeepEqual(got, []string{"/Production/db@local", "/Production/web@ams", "/Production/web@local"}) {
			t.Errorf("series order / presence: %v", got)
		}
		for name, o := range map[string]replay.Options{
			"no target":  {RuleName: "loss", From: at(0), To: at(40), Target: "/Production/nope"},
			"no agent":   {RuleName: "loss", From: at(0), To: at(40), Vantage: "nobody"},
			"no rule":    {RuleName: "nope", From: at(0), To: at(40)},
			"no name":    {From: at(0), To: at(40)},
			"empty span": {RuleName: "loss", From: at(40), To: at(40)},
		} {
			_, err := replay.Run(ctx, e.st, o)
			if err == nil {
				t.Errorf("%s: no error", name)
			} else if name == "no rule" && !strings.Contains(err.Error(), "loss") {
				t.Errorf("the error should list the rules that exist: %v", err)
			}
		}
	})
}

// Protects "disabled rules are replayed, disabled targets are not": testing a
// rule before enabling it is the point, but live never evaluates a disabled
// target.
func TestDisabledRuleReplayedDisabledTargetNot(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	r := e.rule(e.prod, "loss", alert.MetricLoss, alert.OpGreater, 20, 2, 2)
	r.Enabled = false
	e.save(r)
	e.put(e.web, 0, lossPattern("ggbbbggg"))
	e.put(e.db, 0, lossPattern("ggbbbggg"))
	ts, _ := e.st.ListTargets(ctx)
	for _, x := range ts {
		if x.ID == e.db {
			x.Enabled = false
			if err := e.st.UpsertTarget(ctx, &x); err != nil {
				t.Fatal(err)
			}
		}
	}
	rep := run(t, e.st, opts("loss", 0, 8))
	if len(rep.Series) != 1 || rep.Series[0].Target != "/Production/web" || len(rep.Series[0].Episodes) != 1 {
		t.Errorf("series = %+v", rep.Series)
	}
}

// Protects bounded memory: a series is read in chunks that never reach past To
// or grow without limit, and a store that refuses a range as too large gets it
// again in halves with an identical result.
func TestReadsAreChunkedAndBounded(t *testing.T) {
	e := newEnv(t)
	e.save(e.rule(e.prod, "loss", alert.MetricLoss, alert.OpGreater, 20, 2, 2))
	p := []byte(strings.Repeat("g", 2600))
	for _, i := range []int{100, 101, 102, 1500, 1501, 1502} {
		p[i] = 'b'
	}
	e.put(e.web, 0, lossPattern(string(p)))
	o := opts("loss", 0, 2500)

	sp := &spy{Source: e.st}
	want := run(t, sp, o)
	reads := 0
	for _, r := range sp.ranges {
		if r.target == e.web && r.agent == 0 {
			reads++
		}
		if r.to > o.To {
			t.Errorf("read [%d, %d) reaches past To=%d", r.from, r.to, o.To)
		}
		if r.to-r.from > 16*500*60 {
			t.Errorf("read of %d s is not bounded", r.to-r.from)
		}
	}
	if reads < 5 {
		t.Errorf("%d reads of 2500 intervals: not chunked", reads)
	}

	small := &spy{Source: e.st, tooLarge: 100 * 60}
	got := run(t, small, o)
	if !reflect.DeepEqual(want, got) {
		t.Errorf("halving changed the result")
	}
	if len(small.ranges) <= len(sp.ranges) {
		t.Error("the too-large refusal was never exercised")
	}
}

// Protects the report contract that callers rely on: schema 1, the notes that
// say what a replay is not, deterministic order, and the totals arithmetic.
func TestReportShape(t *testing.T) {
	e := newEnv(t)
	e.save(e.rule(e.prod, "loss", alert.MetricLoss, alert.OpGreater, 20, 2, 2))
	e.put(e.web, 0, lossPattern("ggbbbggg"))
	rep := run(t, e.st, replay.Options{RuleName: "loss", From: at(0), To: at(1440)})
	if rep.Schema != 1 || rep.Warnings == nil || rep.Series == nil {
		t.Errorf("schema/nil slices: %+v", rep)
	}
	for _, kw := range []string{"not notifications", "starts clean", "restart", "interval_s", "deleted"} {
		if !slicesContain(rep.Notes, kw) {
			t.Errorf("notes lack %q: %q", kw, rep.Notes)
		}
	}
	if rep.Totals.Series != 1 || rep.Totals.SeriesFiring != 1 || rep.Totals.Episodes != 1 || rep.Totals.EpisodesPerDay != 1 {
		t.Errorf("totals = %+v, want 1 episode in exactly one day", rep.Totals)
	}
	if rep.Series[0].Intervals != 8 || rep.Series[0].Condition != "loss > 20% for 2 intervals" ||
		rep.Series[0].RuleNode != "/Production" {
		t.Errorf("series = %+v", rep.Series[0])
	}
}

// Protects the deterministic order callers diff reports by: rule node, rule
// name, target path, agent name — whatever order the rules and rows arrived in.
func TestSeriesOrderIsDeterministic(t *testing.T) {
	e := newEnv(t)
	e.put(e.web, 0, lossPattern("ggbbbggg"))
	e.put(e.db, 0, lossPattern("ggbbbggg"))
	e.put(e.web, e.ams, lossPattern("ggbbbggg"))
	zeta := e.rule(e.prod, "zeta", alert.MetricLoss, alert.OpGreater, 20, 2, 2)
	alpha := e.rule(e.prod, "alpha", alert.MetricLoss, alert.OpGreater, 20, 2, 2)
	rep := run(t, e.st, replay.Options{FileRules: []alert.Rule{zeta, alpha}, From: at(0), To: at(8)})
	var got []string
	for _, s := range rep.Series {
		got = append(got, s.Rule+" "+s.Target+"@"+s.Vantage)
	}
	want := []string{
		"alpha /Production/db@local", "alpha /Production/web@ams", "alpha /Production/web@local",
		"zeta /Production/db@local", "zeta /Production/web@ams", "zeta /Production/web@local",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("order = %v", got)
	}
}

// failing refuses every read of one target.
type failing struct {
	replay.Source
	target int64
	err    error
}

func (f *failing) QueryRange(ctx context.Context, tg, ag int64, fam tree.Family, from, to int64) ([]store.Measurement, error) {
	if tg == f.target {
		return nil, f.err
	}
	return f.Source.QueryRange(ctx, tg, ag, fam, from, to)
}

// Protects the error a replay reports when series run side by side: the read
// that failed, named by its series, and not the cancellation that failure
// caused in the series still running.
func TestAFailedSeriesIsTheErrorReported(t *testing.T) {
	e := newEnv(t)
	e.save(e.rule(e.prod, "loss", alert.MetricLoss, alert.OpGreater, 20, 2, 2))
	for _, tg := range []int64{e.web, e.db} {
		e.put(tg, 0, lossPattern(strings.Repeat("g", 2600)))
		e.put(tg, e.ams, lossPattern(strings.Repeat("g", 2600)))
	}
	boom := errors.New("disk on fire")
	_, err := replay.Run(context.Background(), &failing{Source: e.st, target: e.db, err: boom}, opts("loss", 0, 2500))
	if !errors.Is(err, boom) {
		t.Fatalf("Run = %v, want the read error", err)
	}
	if !strings.Contains(err.Error(), "db") {
		t.Errorf("the error does not name the series that failed: %v", err)
	}
}

// inflight counts how many reads are in progress at once.
type inflight struct {
	replay.Source
	now, peak atomic.Int64
}

func (f *inflight) QueryRange(ctx context.Context, tg, ag int64, fam tree.Family, from, to int64) ([]store.Measurement, error) {
	n := f.now.Add(1)
	defer f.now.Add(-1)
	for p := f.peak.Load(); n > p && !f.peak.CompareAndSwap(p, n); p = f.peak.Load() {
	}
	// Long enough that every series allowed in is in at the same time.
	time.Sleep(2 * time.Millisecond)
	return f.Source.QueryRange(ctx, tg, ag, fam, from, to)
}

// Protects the memory bound across series: a rule on the root reaches three
// targets from two agents, six series, and no more than four are ever being
// read at once.
func TestNoMoreThanFourSeriesAtOnce(t *testing.T) {
	e := newEnv(t)
	e.save(e.rule(1, "loss", alert.MetricLoss, alert.OpGreater, 20, 2, 2))
	src := &inflight{Source: e.st}
	run(t, src, opts("loss", 0, 2500))
	if p := src.peak.Load(); p > 4 || p < 1 {
		t.Errorf("%d series were being read at once, want between 1 and 4", p)
	}
}

// Protects the family of a replayed series and the family scope of a silence in
// a replay: the series says the family it was read over (the target's current
// one), a silence scoped to the other family covers nothing of it, and one
// scoped to its own family covers it, as the live manager decides.
func TestReplayNamesTheFamilyAndMatchesSilencesWithIt(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.save(e.rule(e.prod, "loss", alert.MetricLoss, alert.OpGreater, 20, 2, 2))
	e.put(e.web, 0, lossPattern("ggbbbbggg"))
	v4, v6 := tree.FamilyV4, tree.FamilyV6
	for _, s := range []alert.Silence{
		{Family: &v6, StartsAt: at(0), EndsAt: at(20), Reason: "t", CreatedBy: "t"},
	} {
		if err := e.st.CreateSilence(ctx, &s); err != nil {
			t.Fatal(err)
		}
	}
	series := find(t, run(t, e.st, opts("loss", 0, 9)), "/Production/web", "local")
	if series.Family != v4 {
		t.Errorf("series family = %v, want v4", series.Family)
	}
	if len(series.Episodes) != 1 || series.Episodes[0].FireSilenced || series.Episodes[0].ResolveSilenced {
		t.Errorf("a silence scoped to v6 covered a v4 series: %+v", series.Episodes)
	}

	if err := e.st.CreateSilence(ctx, &alert.Silence{Family: &v4, StartsAt: at(0), EndsAt: at(20), Reason: "t", CreatedBy: "t"}); err != nil {
		t.Fatal(err)
	}
	series = find(t, run(t, e.st, opts("loss", 0, 9)), "/Production/web", "local")
	if len(series.Episodes) != 1 || !series.Episodes[0].FireSilenced {
		t.Errorf("a silence scoped to v4 did not cover a v4 series: %+v", series.Episodes)
	}
}
