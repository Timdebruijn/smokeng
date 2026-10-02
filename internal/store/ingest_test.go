package store

import (
	"path/filepath"
	"slices"
	"testing"
)

func openTemp(t *testing.T) *SQLite {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "ingest.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func rowAt(ts int64, sent int, samples []uint32, ipdv []int32) Measurement {
	m := Measurement{TargetID: 1, AgentID: 4, TS: ts, Sent: sent, Received: len(samples), Samples: samples}
	if ipdv != nil {
		m.Series = map[string][]int32{SeriesIPDVSend: ipdv}
	}
	return m
}

// IngestMeasurements keeps what is stored. A second submission for the same
// interval, however different, changes neither the measurement nor the series
// that belong to it.
func TestIngestMeasurementsKeepsTheFirstWrite(t *testing.T) {
	s := openTemp(t)
	ctx := t.Context()
	first := rowAt(1000, 5, []uint32{10, 20, 30}, []int32{-3, 0, 4})
	if dup, err := s.IngestMeasurements(ctx, []Measurement{first}); err != nil || dup != 0 {
		t.Fatalf("first write: dup=%d err=%v", dup, err)
	}

	rewrite := rowAt(1000, 9, []uint32{77}, []int32{99, 100})
	dup, err := s.IngestMeasurements(ctx, []Measurement{rewrite})
	if err != nil || dup != 1 {
		t.Fatalf("resubmission: dup=%d err=%v, want 1 duplicate", dup, err)
	}

	got, err := s.QueryRange(ctx, 1, 4, 0, 1<<40)
	if err != nil || len(got) != 1 {
		t.Fatalf("QueryRange = %d rows, %v", len(got), err)
	}
	if got[0].Sent != 5 || !slices.Equal(got[0].Samples, []uint32{10, 20, 30}) {
		t.Errorf("the stored measurement was rewritten: %+v", got[0])
	}
	if !slices.Equal(got[0].Series[SeriesIPDVSend], []int32{-3, 0, 4}) {
		t.Errorf("the stored series was rewritten: %v", got[0].Series)
	}
}

func TestIngestMeasurementsCountsOnlyTheDuplicates(t *testing.T) {
	s := openTemp(t)
	ctx := t.Context()
	if _, err := s.IngestMeasurements(ctx, []Measurement{rowAt(1000, 1, []uint32{1}, nil), rowAt(1060, 1, []uint32{1}, nil)}); err != nil {
		t.Fatal(err)
	}
	dup, err := s.IngestMeasurements(ctx, []Measurement{
		rowAt(1000, 1, []uint32{1}, nil), rowAt(1060, 1, []uint32{1}, nil),
		rowAt(1120, 1, []uint32{1}, nil), rowAt(1180, 1, []uint32{1}, nil), rowAt(1240, 1, []uint32{1}, nil),
	})
	if err != nil || dup != 2 {
		t.Fatalf("dup=%d err=%v, want 2 duplicates among 5 rows", dup, err)
	}
	got, err := s.QueryRange(ctx, 1, 4, 0, 1<<40)
	if err != nil || len(got) != 5 {
		t.Fatalf("stored %d rows, want 5 (%v)", len(got), err)
	}
}

// The local prober writes its own rows, and for it replacement is the point:
// that path must not have changed.
func TestWriteMeasurementsStillReplaces(t *testing.T) {
	s := openTemp(t)
	ctx := t.Context()
	if err := s.WriteMeasurements(ctx, []Measurement{rowAt(1000, 5, []uint32{10, 20, 30}, []int32{1, 2})}); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteMeasurements(ctx, []Measurement{rowAt(1000, 9, []uint32{77}, nil)}); err != nil {
		t.Fatal(err)
	}
	got, err := s.QueryRange(ctx, 1, 4, 0, 1<<40)
	if err != nil || len(got) != 1 {
		t.Fatalf("QueryRange = %d rows, %v", len(got), err)
	}
	if got[0].Sent != 9 || !slices.Equal(got[0].Samples, []uint32{77}) {
		t.Errorf("WriteMeasurements no longer replaces: %+v", got[0])
	}
	// The replaced interval takes its series with it: a peer that stopped
	// returning timestamps must not keep serving the last jitter it measured.
	if _, ok := got[0].Series[SeriesIPDVSend]; ok {
		t.Errorf("a replaced row kept its old series: %v", got[0].Series)
	}
}
