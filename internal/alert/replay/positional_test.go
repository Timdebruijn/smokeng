package replay_test

import (
	"context"
	"math/rand/v2"
	"testing"

	"github.com/timdebruijn/smokeng/internal/alert"
	"github.com/timdebruijn/smokeng/internal/alert/replay"
	"github.com/timdebruijn/smokeng/internal/store"
)

// A replay goes through the same step as the live manager, so the positional
// loss policy has to show in it: on a series whose loss sits at fixed send
// positions (a rate limiter) a loss rule has far fewer episodes under exclude
// than under flag_only, while on a series with the same loss percentage in the
// same intervals but scattered positions the two policies agree. Protects both
// halves: the exclusion reaches the replay, and it keys on the detector's flag
// and not on loss as such.
func TestReplayFollowsThePositionalLossPolicy(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.st.DetectPositionalLoss() // before any write: flags are set as rows arrive

	const (
		n     = 300
		sent  = 20
		lost  = 3 // 15% in every lossy interval, in both series
		limit = 10.0
	)
	lossyAt := func(i int) bool { return i%5 < 2 } // two lossy intervals, three clean
	rng := rand.New(rand.NewPCG(5, 9))
	series := func(target int64, positions func() []int) {
		var ms []store.Measurement
		for i := range n {
			lostAt := []int(nil)
			if lossyAt(i) {
				lostAt = positions()
			}
			mask := make([]bool, sent)
			for _, p := range lostAt {
				mask[p] = true
			}
			recv := sent - len(lostAt)
			sm := make([]uint32, recv)
			for k := range sm {
				sm[k] = 5000
			}
			m := store.Measurement{TargetID: target, TS: at(i), Sent: sent, Received: recv, Samples: sm}
			if len(lostAt) > 0 {
				m.LossMask = store.EncodeLossMask(false, mask)
			}
			ms = append(ms, m)
		}
		if err := e.st.WriteMeasurements(ctx, ms); err != nil {
			t.Fatal(err)
		}
	}
	// A rate limiter drops the tail of the burst every time.
	series(e.web, func() []int { return []int{17, 18, 19} })
	// Real-looking loss: the same count, anywhere.
	series(e.db, func() []int { return rng.Perm(sent)[:lost] })

	// How many intervals the detector flagged, so the test knows its premise held.
	flagged := func(target int64) int {
		ms, err := e.st.QueryRange(ctx, target, 0, at(0), at(n))
		if err != nil {
			t.Fatal(err)
		}
		c := 0
		for _, m := range ms {
			if m.Positional != nil {
				c++
			}
		}
		return c
	}
	if got := flagged(e.web); got < 50 {
		t.Fatalf("setup: only %d intervals of the rate-limited series flagged", got)
	}
	if got := flagged(e.db); got != 0 {
		t.Fatalf("setup: %d intervals of the scattered series flagged", got)
	}

	e.save(e.rule(e.prod, "loss", alert.MetricLoss, alert.OpGreater, limit, 2, 2))
	setPolicy := func(v string) {
		ts, err := e.st.ListTargets(ctx)
		if err != nil {
			t.Fatal(err)
		}
		// Only the group is changed; the root keeps its default.
		for _, x := range ts {
			if x.ID == e.prod {
				x.Settings.PositionalLoss = &v
				if err := e.st.UpsertTarget(ctx, &x); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	type result struct{ episodes, seconds int }
	replayOf := func(target string) result {
		rep := run(t, e.st, replay.Options{RuleName: "loss", From: at(0), To: at(n), Target: target})
		var r result
		for _, s := range rep.Series {
			r.episodes += s.Fired
			r.seconds += int(s.FiringSeconds)
		}
		return r
	}

	setPolicy("exclude")
	limExcl, scatExcl := replayOf("/Production/web"), replayOf("/Production/db")
	setPolicy("flag_only")
	limFlag, scatFlag := replayOf("/Production/web"), replayOf("/Production/db")
	t.Logf("rate limited: exclude %+v, flag_only %+v", limExcl, limFlag)
	t.Logf("scattered:    exclude %+v, flag_only %+v", scatExcl, scatFlag)

	if limFlag.episodes < 50 {
		t.Fatalf("setup: flag_only gave only %d episodes on the rate-limited series", limFlag.episodes)
	}
	if limExcl.episodes*3 > limFlag.episodes || limExcl.seconds*3 > limFlag.seconds {
		t.Errorf("exclude did not cut the rate-limited series: %+v vs %+v", limExcl, limFlag)
	}
	if scatExcl != scatFlag || scatExcl.episodes < 50 {
		t.Errorf("the scattered series changed with the policy (or never fired): %+v vs %+v", scatExcl, scatFlag)
	}
}
