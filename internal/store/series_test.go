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
