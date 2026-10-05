package store

import (
	"errors"
	"testing"
)

func lowered(t *testing.T, samples, availability, paths int) {
	t.Helper()
	s, a, p := maxSamplesPerRead, maxAvailabilityRows, maxPathRows
	maxSamplesPerRead, maxAvailabilityRows, maxPathRows = samples, availability, paths
	t.Cleanup(func() { maxSamplesPerRead, maxAvailabilityRows, maxPathRows = s, a, p })
}

func rowWith(ts int64, n int) Measurement {
	samples := make([]uint32, n)
	for i := range samples {
		samples[i] = uint32(100 + i)
	}
	return Measurement{TargetID: 1, AgentID: 0, TS: ts, Sent: n, Received: n, Samples: samples}
}

// A request bounded by the width of its window is not bounded by what the window
// holds: an interval can carry 65,535 samples, so 200,000 intervals are tens of
// billions. The samples actually read are counted.
func TestQueryRangeRefusesMoreSamplesThanOneReadReturns(t *testing.T) {
	lowered(t, 100, 1000, 1000)
	s := openTemp(t)
	ctx := t.Context()
	write := func(rows, each int) {
		var ms []Measurement
		for i := range rows {
			ms = append(ms, rowWith(int64(1000+i*60), each))
		}
		if err := s.WriteMeasurements(ctx, ms); err != nil {
			t.Fatal(err)
		}
	}
	write(10, 10) // exactly 100 samples
	if got, err := s.QueryRange(ctx, 1, 0, 0, 1<<40); err != nil || len(got) != 10 {
		t.Fatalf("a range of exactly the limit = %d rows, %v", len(got), err)
	}
	write(11, 10) // 110: replaces the same timestamps with one more row
	if _, err := s.QueryRange(ctx, 1, 0, 0, 1<<40); !errors.Is(err, ErrRangeTooLarge) {
		t.Fatalf("a range one row over the limit: err = %v, want ErrRangeTooLarge", err)
	}
	// A narrower window over the same data is fine.
	if got, err := s.QueryRange(ctx, 1, 0, 0, 1000+60*5); err != nil || len(got) != 5 {
		t.Errorf("a narrower window = %d rows, %v", len(got), err)
	}
}

// The count comes from the received column and is checked before the blob is
// decoded, so the row that would cross the limit is never allocated. Proved by
// corrupting that row's blob: it must be refused for its size and not for being
// undecodable.
func TestTheRowThatCrossesTheLimitIsNotDecoded(t *testing.T) {
	lowered(t, 50, 1000, 1000)
	s := openTemp(t)
	ctx := t.Context()
	if err := s.WriteMeasurements(ctx, []Measurement{rowWith(1000, 30), rowWith(1060, 30)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE measurements SET samples = X'01FFFFFFFFFFFFFFFFFFFF' WHERE ts = 1060`); err != nil {
		t.Fatal(err)
	}
	_, err := s.QueryRange(ctx, 1, 0, 0, 1<<40)
	if !errors.Is(err, ErrRangeTooLarge) {
		t.Fatalf("err = %v, want ErrRangeTooLarge: the corrupt row was decoded before it was counted", err)
	}
}

func TestAvailabilitySeriesRefusesMoreRowsThanOneReadReturns(t *testing.T) {
	lowered(t, 1000, 50, 1000)
	s := openTemp(t)
	ctx := t.Context()
	var ms []Measurement
	for i := range 51 {
		ms = append(ms, rowWith(int64(1000+i*60), 1))
	}
	if err := s.WriteMeasurements(ctx, ms[:50]); err != nil {
		t.Fatal(err)
	}
	if got, err := s.AvailabilitySeries(ctx, 1, 0, 0, 1<<40); err != nil || len(got) != 50 {
		t.Fatalf("exactly the limit = %d rows, %v", len(got), err)
	}
	if err := s.WriteMeasurements(ctx, ms[50:]); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AvailabilitySeries(ctx, 1, 0, 0, 1<<40); !errors.Is(err, ErrRangeTooLarge) {
		t.Fatalf("one row over: err = %v, want ErrRangeTooLarge", err)
	}
}

func TestPathChangesRefusesMoreRowsThanOneReadReturns(t *testing.T) {
	lowered(t, 1000, 1000, 5)
	s := openTemp(t)
	ctx := t.Context()
	for i := range 5 {
		if err := s.RecordPath(ctx, 1, 0, int64(1000+i*60), "10.0.0.1"); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := s.PathChanges(ctx, 1, 0, 0, 1<<40); err != nil || len(got) != 5 {
		t.Fatalf("exactly the limit = %d rows, %v", len(got), err)
	}
	if err := s.RecordPath(ctx, 1, 0, 2000, "10.0.0.2"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PathChanges(ctx, 1, 0, 0, 1<<40); !errors.Is(err, ErrRangeTooLarge) {
		t.Fatalf("one row over: err = %v, want ErrRangeTooLarge", err)
	}
}

// The tests above lower the limits, so the real ones are pinned here.
func TestTheReadLimitsAreTheDocumentedNumbers(t *testing.T) {
	for name, c := range map[string][2]int{
		"MaxSamplesPerRead":   {MaxSamplesPerRead, 20_000_000},
		"MaxAvailabilityRows": {MaxAvailabilityRows, 2_000_000},
		"MaxPathRows":         {MaxPathRows, 100_000},
	} {
		if c[0] != c[1] {
			t.Errorf("%s = %d, the documentation says %d", name, c[0], c[1])
		}
	}
	// And the variables a test lowers start at them.
	if maxSamplesPerRead != MaxSamplesPerRead || maxAvailabilityRows != MaxAvailabilityRows || maxPathRows != MaxPathRows {
		t.Error("the working limits do not start at the documented ones")
	}
}
