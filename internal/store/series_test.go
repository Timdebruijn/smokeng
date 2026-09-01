package store

import (
	"context"
	"testing"
)

func writeOne(t *testing.T, s *SQLite, m Measurement) {
	t.Helper()
	if err := s.WriteMeasurements(context.Background(), []Measurement{m}); err != nil {
		t.Fatal(err)
	}
}

func TestSeriesRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	writeOne(t, s, Measurement{
		TargetID: 1, AgentID: 2, TS: 1000, Sent: 3, Received: 3,
		Samples: []uint32{100, 200, 300},
		Series: map[string][]int32{
			SeriesIPDVSend:    {-40, -1, 12},
			SeriesIPDVReceive: {-9, 0, 3},
		},
	})
	got, err := s.QueryRange(ctx, 1, 2, 0, 2000)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d measurements, want 1", len(got))
	}
	send := got[0].Series[SeriesIPDVSend]
	if len(send) != 3 || send[0] != -40 || send[2] != 12 {
		t.Errorf("ipdv_send = %v, want [-40 -1 12]", send)
	}
	if len(got[0].Series[SeriesIPDVReceive]) != 3 {
		t.Errorf("ipdv_receive = %v", got[0].Series[SeriesIPDVReceive])
	}
	if _, ok := got[0].Series[SeriesServerProcessing]; ok {
		t.Error("a series that was never written came back present")
	}
}

// A measurement with no extra series must not come back carrying an empty map
// that reads as "measured, and every value was zero".
func TestSeriesAbsentStaysAbsent(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	writeOne(t, s, Measurement{TargetID: 1, TS: 1000, Sent: 1, Received: 1, Samples: []uint32{50}})
	got, err := s.QueryRange(ctx, 1, 0, 0, 2000)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Series != nil {
		t.Errorf("Series = %v, want nil", got[0].Series)
	}
}

// Rewriting a measurement replaces its series. A peer that stops returning
// timestamps must stop reporting jitter, not keep serving the last reading it
// managed to take.
func TestSeriesReplacedOnRewrite(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	m := Measurement{TargetID: 1, TS: 1000, Sent: 2, Received: 2, Samples: []uint32{10, 20},
		Series: map[string][]int32{SeriesIPDVSend: {-5, 5}}}
	writeOne(t, s, m)
	m.Series = nil
	writeOne(t, s, m)
	got, err := s.QueryRange(ctx, 1, 0, 0, 2000)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Series != nil {
		t.Errorf("stale series survived a rewrite: %v", got[0].Series)
	}
}

func TestSeriesRejectsUnknownName(t *testing.T) {
	err := openStore(t).WriteMeasurements(context.Background(), []Measurement{{
		TargetID: 1, TS: 1000, Sent: 1, Received: 1, Samples: []uint32{10},
		Series: map[string][]int32{"ipdv_sideways": {1}},
	}})
	if err == nil {
		t.Fatal("an unknown series name was accepted")
	}
}

// Retention takes the series with the measurement. A series row whose
// measurement has been pruned is not merely litter: reading a range that
// contains one is a hard error, so leaving them behind would make the window
// unreadable rather than untidy.
func TestPruneRemovesSeries(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	for ts := int64(1000); ts < 5000; ts += 1000 {
		writeOne(t, s, Measurement{TargetID: 1, TS: ts, Sent: 1, Received: 1,
			Samples: []uint32{10}, Series: map[string][]int32{SeriesIPDVSend: {-1}}})
	}
	if _, err := s.PruneMeasurements(ctx, 1, 3000, 3600); err != nil {
		t.Fatal(err)
	}
	var orphans int
	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM measurement_series ms
		WHERE NOT EXISTS (SELECT 1 FROM measurements m
		  WHERE m.target_id = ms.target_id AND m.agent_id = ms.agent_id AND m.ts = ms.ts)`,
	).Scan(&orphans); err != nil {
		t.Fatal(err)
	}
	if orphans != 0 {
		t.Errorf("%d series rows outlived their measurement", orphans)
	}
	got, err := s.QueryRange(ctx, 1, 0, 0, 9000)
	if err != nil {
		t.Fatalf("range containing pruned data no longer reads: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("got %d measurements after prune, want 2", len(got))
	}
}

// An agent measures the extra series too; they have to survive the trip to the
// master, and leave the outbox when the measurement does.
func TestPendingMeasurementsCarrySeries(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	writeOne(t, s, Measurement{TargetID: 1, AgentID: 7, TS: 1000, Sent: 2, Received: 2,
		Samples: []uint32{10, 20}, Series: map[string][]int32{SeriesIPDVSend: {-3, 8}}})
	pend, err := s.PendingMeasurements(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pend) != 1 {
		t.Fatalf("got %d pending, want 1", len(pend))
	}
	if v := pend[0].Series[SeriesIPDVSend]; len(v) != 2 || v[0] != -3 {
		t.Errorf("series lost on the way out of the outbox: %v", pend[0].Series)
	}
	if err := s.DropSubmitted(ctx, pend); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM measurement_series").Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Errorf("%d series rows left in the outbox after submission", left)
	}
}

// The orphan check is the one invariant this design rests on, and nothing
// exercised it: replacing the error with a `continue` left the suite green.
// A series row whose measurement is gone means retention deleted one and not
// the other, and reading past it would serve a jitter distribution belonging
// to an interval that no longer exists.
func TestOrphanSeriesIsRefused(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	writeOne(t, s, Measurement{TargetID: 1, TS: 1000, Sent: 1, Received: 1,
		Samples: []uint32{10}, Series: map[string][]int32{SeriesIPDVSend: {-1}}})
	// Delete the measurement behind the store's back, as a torn prune would.
	if _, err := s.db.ExecContext(ctx, "DELETE FROM measurements WHERE ts = 1000"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.QueryRange(ctx, 1, 0, 0, 2000); err == nil {
		t.Fatal("a series row with no measurement was read without complaint")
	}
	// And it is found even when nothing survives in the window, which is where
	// an orphan actually sits.
	if _, err := s.QueryRange(ctx, 1, 0, 900, 1100); err == nil {
		t.Error("an orphan alone in its window went unnoticed")
	}
}

// Series belong to the (target, agent) series that measured them. Dropping the
// agent filter let one agent's jitter attach to another's measurement, and the
// suite stayed green.
func TestSeriesAreScopedToTheirAgent(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	writeOne(t, s, Measurement{TargetID: 1, AgentID: 1, TS: 1000, Sent: 1, Received: 1,
		Samples: []uint32{10}, Series: map[string][]int32{SeriesIPDVSend: {-11}}})
	writeOne(t, s, Measurement{TargetID: 1, AgentID: 2, TS: 1000, Sent: 1, Received: 1,
		Samples: []uint32{20}, Series: map[string][]int32{SeriesIPDVSend: {-22}}})

	for agent, want := range map[int64]int32{1: -11, 2: -22} {
		got, err := s.QueryRange(ctx, 1, agent, 0, 2000)
		if err != nil {
			t.Fatal(err)
		}
		v := got[0].Series[SeriesIPDVSend]
		if len(v) != 1 || v[0] != want {
			t.Errorf("agent %d read ipdv_send %v, want [%d]", agent, v, want)
		}
	}
}

// An undecodable optional series must not take the interval with it. Failing
// the read returned 500 for the latency graph, the loss rail and baseline
// capture because one byte of an optional distribution was wrong.
func TestCorruptSeriesLeavesTheIntervalReadable(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	writeOne(t, s, Measurement{TargetID: 1, TS: 1000, Sent: 2, Received: 2,
		Samples: []uint32{10, 20}, Series: map[string][]int32{SeriesIPDVSend: {-5, 5}}})
	// A version byte no decoder knows.
	if _, err := s.db.ExecContext(ctx,
		"UPDATE measurement_series SET samples = ? WHERE series = ?",
		[]byte{0x7f, 0x01}, SeriesIPDVSend); err != nil {
		t.Fatal(err)
	}
	got, err := s.QueryRange(ctx, 1, 0, 0, 2000)
	if err != nil {
		t.Fatalf("one corrupt optional series made the whole window unreadable: %v", err)
	}
	if len(got) != 1 || len(got[0].Samples) != 2 {
		t.Fatalf("the round trip did not survive: %+v", got)
	}
	if _, ok := got[0].Series[SeriesIPDVSend]; ok {
		t.Error("an undecodable series was served as if it had decoded")
	}
}

// A page of the outbox can span far more targets than one SQL statement may
// hold: SQLite caps expression-tree depth at 1000, and the per-pair query is
// built from OR'd groups. Over that ceiling the statement does not degrade, it
// errors — failing the whole drain and leaving the agent to retry the identical
// query every fifteen seconds forever, which is worse than the table scan the
// per-pair query was written to avoid.
//
// 1200 pairs is past the ceiling; a single-pair test cannot see this, which is
// why the fix shipped once with the suite green.
func TestPendingSeriesSpansManyTargets(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	const pairs = 1200
	for i := range pairs {
		writeOne(t, s, Measurement{
			TargetID: int64(i + 1), AgentID: 0, TS: 1000, Sent: 1, Received: 1,
			Samples: []uint32{10},
			Series:  map[string][]int32{SeriesIPDVSend: {int32(-i - 1)}},
		})
	}
	got, err := s.PendingMeasurements(ctx, 2000)
	if err != nil {
		t.Fatalf("draining %d pairs: %v", pairs, err)
	}
	if len(got) != pairs {
		t.Fatalf("got %d measurements, want %d", len(got), pairs)
	}
	// Every one carries its own series, not its neighbour's: the chunking must
	// not scramble the key matching.
	for _, m := range got {
		v := m.Series[SeriesIPDVSend]
		if len(v) != 1 || v[0] != int32(-m.TargetID) {
			t.Fatalf("target %d carries %v, want [%d]", m.TargetID, v, -m.TargetID)
		}
	}
}

// Two agents measuring the same target at the same second must each carry
// their own series out of the outbox.
//
// Note what actually enforces that here, because it is not the SQL: the agent
// clause in the WHERE exists so the primary key can serve the query, and
// removing it leaves this test green — the exact (target, agent, ts) match
// against byKey is what keeps the rows apart. That is the opposite of the
// master's attachSeries, where target and agent come from the caller and the
// WHERE clause is the only thing separating two agents' series; there, removing
// it does fail a test, and deliberately so.
func TestPendingSeriesScopedToTheirAgent(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	writeOne(t, s, Measurement{TargetID: 1, AgentID: 1, TS: 1000, Sent: 1, Received: 1,
		Samples: []uint32{10}, Series: map[string][]int32{SeriesIPDVSend: {-11}}})
	writeOne(t, s, Measurement{TargetID: 1, AgentID: 2, TS: 1000, Sent: 1, Received: 1,
		Samples: []uint32{20}, Series: map[string][]int32{SeriesIPDVSend: {-22}}})
	got, err := s.PendingMeasurements(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d pending, want 2", len(got))
	}
	for _, m := range got {
		want := map[int64]int32{1: -11, 2: -22}[m.AgentID]
		v := m.Series[SeriesIPDVSend]
		if len(v) != 1 || v[0] != want {
			t.Errorf("agent %d carries %v, want [%d]", m.AgentID, v, want)
		}
	}
}

// A series that was measured but produced no values must stay distinguishable
// from one that was never measured. Both hold zero values; only the second is
// an instrumentation problem, and collapsing them lets a lossy target report a
// fault it does not have.
func TestSeriesMeasuredButEmptySurvives(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	writeOne(t, s, Measurement{
		TargetID: 1, TS: 1000, Sent: 1, Received: 1, Samples: []uint32{50},
		Series: map[string][]int32{SeriesIPDVSend: {}},
	})
	got, err := s.QueryRange(ctx, 1, 0, 0, 2000)
	if err != nil {
		t.Fatal(err)
	}
	v, ok := got[0].Series[SeriesIPDVSend]
	if !ok {
		t.Fatal("an empty-but-measured series came back absent")
	}
	if len(v) != 0 {
		t.Errorf("ipdv_send = %v, want empty", v)
	}
	if _, ok := got[0].Series[SeriesIPDVReceive]; ok {
		t.Error("a series that was never written came back present")
	}
}

// The send-failure reason has to reach the database and come back. Making
// WriteMeasurements persist NULL instead left every test green, which means
// the column could have been silently dead in production.
func TestSendReasonRoundTripsThroughTheStore(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	want := SendReasonRefused
	writeOne(t, s, Measurement{
		TargetID: 1, TS: 1000, Sent: 2, Received: 1, Flags: FlagSendFailed,
		Samples: []uint32{10}, SendErr: &want,
	})
	// And an interval that sent everything records nothing.
	writeOne(t, s, Measurement{
		TargetID: 1, TS: 2000, Sent: 1, Received: 1, Samples: []uint32{20},
	})
	got, err := s.QueryRange(ctx, 1, 0, 0, 3000)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d measurements", len(got))
	}
	if got[0].SendErr == nil || *got[0].SendErr != want {
		t.Errorf("SendErr = %v, want %d", got[0].SendErr, want)
	}
	if got[1].SendErr != nil {
		t.Errorf("a clean interval came back with reason %d", *got[1].SendErr)
	}
}

// And out of the agent's outbox, or remote agents lose it entirely.
func TestSendReasonSurvivesTheOutbox(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	want := SendReasonSessionShort
	writeOne(t, s, Measurement{
		TargetID: 1, AgentID: 7, TS: 1000, Sent: 2, Received: 1, Flags: FlagSendFailed,
		Samples: []uint32{10}, SendErr: &want,
	})
	pend, err := s.PendingMeasurements(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pend) != 1 || pend[0].SendErr == nil || *pend[0].SendErr != want {
		t.Fatalf("the reason did not survive the outbox: %+v", pend)
	}
}
