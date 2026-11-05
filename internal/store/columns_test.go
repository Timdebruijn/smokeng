package store

import (
	"errors"
	"math/rand/v2"
	"reflect"
	"slices"
	"testing"
)

// A window holding no more intervals than the plot has columns comes back as
// it is, identical to QueryRange, series included: pooling would change
// nothing a plot can show and lose each interval's own details.
func TestQueryColumnsReturnsRawWhenTheWindowFits(t *testing.T) {
	s := openTemp(t)
	ctx := t.Context()
	var ms []Measurement
	for i := range 10 {
		m := rowWith(int64(1000+i*60), 5)
		m.Series = map[string][]int32{SeriesIPDVSend: {-3, 4}}
		ms = append(ms, m)
	}
	if err := s.WriteMeasurements(ctx, ms); err != nil {
		t.Fatal(err)
	}
	want, err := s.QueryRange(ctx, 1, 0, 0, 1<<40)
	if err != nil {
		t.Fatal(err)
	}
	raw, cols, err := s.QueryColumns(ctx, 1, 0, 0, 1<<40, 10)
	if err != nil || cols != nil {
		t.Fatalf("10 intervals in 10 columns: cols = %v, err = %v; want raw", cols, err)
	}
	if !reflect.DeepEqual(raw, want) {
		t.Errorf("raw = %+v\nwant QueryRange's %+v", raw, want)
	}
}

// One interval more than there are columns, and every interval is pooled into
// the column its start falls in. Checked against pooling done here by hand:
// sums are sums, flags are all of them, the quantiles are order statistics of
// the pooled samples and not of each interval's own, and a column with no
// interval is absent rather than an empty row.
func TestQueryColumnsPoolsEveryIntervalIntoItsColumn(t *testing.T) {
	s := openTemp(t)
	ctx := t.Context()
	const from, to, n = 0, 6000, 10 // 600 s per column
	rng := rand.New(rand.NewPCG(1, 2))
	var ms []Measurement
	for ts := int64(0); ts < to; ts += 60 {
		if ts >= 1200 && ts < 1800 {
			continue // column 2 is a gap
		}
		k := rng.IntN(40)
		samples := make([]uint32, k)
		for i := range samples {
			samples[i] = uint32(rng.IntN(100_000))
		}
		slices.Sort(samples)
		m := Measurement{TargetID: 1, TS: ts, Sent: 40, Received: k, Samples: samples}
		if ts == 3000 {
			m.Flags = 0x4
		}
		ms = append(ms, m)
	}
	if err := s.WriteMeasurements(ctx, ms); err != nil {
		t.Fatal(err)
	}
	raw, cols, err := s.QueryColumns(ctx, 1, 0, from, to, n)
	if err != nil || raw != nil {
		t.Fatalf("raw = %d rows, err = %v; want columns", len(raw), err)
	}
	if len(cols) != n-1 {
		t.Fatalf("%d columns, want %d (one gap)", len(cols), n-1)
	}
	for _, c := range cols {
		if c.From == 1200 {
			t.Fatal("the gap came back as a column")
		}
		want := Column{From: c.From, To: c.From + 600}
		var pool []uint32
		for _, m := range ms {
			if m.TS >= c.From && m.TS < c.To {
				want.Rounds++
				want.Sent += m.Sent
				want.Received += m.Received
				if m.Flags != 0 {
					want.Flags |= m.Flags
					want.Flagged++
				}
				pool = append(pool, m.Samples...)
			}
		}
		slices.Sort(pool)
		if len(pool) <= ColumnQuantiles {
			want.Quantiles = pool
		} else {
			for k := range ColumnQuantiles {
				want.Quantiles = append(want.Quantiles, pool[(2*k+1)*len(pool)/(2*ColumnQuantiles)])
			}
		}
		if !reflect.DeepEqual(c, want) {
			t.Errorf("column %d:\n got %+v\nwant %+v", c.From, c, want)
		}
	}
}

// At most q values come back whole; above that, the sample at each mid-rank.
func TestQuantilesAreSamplesAtTheMidRanks(t *testing.T) {
	if got := quantiles([]uint32{3, 1, 2}, 4); !slices.Equal(got, []uint32{1, 2, 3}) {
		t.Errorf("3 values, q=4: %v, want all three sorted", got)
	}
	pool := make([]uint32, 1000)
	for i := range pool {
		pool[i] = uint32(999 - i)
	}
	if got := quantiles(pool, 4); !slices.Equal(got, []uint32{125, 375, 625, 875}) {
		t.Errorf("0..999, q=4: %v, want 125 375 625 875", got)
	}
	if got := quantiles([]uint32(nil), 4); len(got) != 0 {
		t.Errorf("no samples: %v, want none", got)
	}
}

// Column boundaries hold for any window, including one whose width overflows
// 64 bits when multiplied by the column count: every second falls in the
// column that starts at or before it and ends after it, and the columns tile
// [from, to) exactly.
func TestColumnBoundariesTileTheWindow(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4))
	for _, w := range []struct{ from, to int64 }{
		{0, 6000}, {1000, 1007}, {-1 << 62, 1 << 62}, {-(1 << 63), 1<<63 - 1},
	} {
		for _, n := range []int{1, 7, 1000, 8192} {
			p := newPooler(w.from, w.to, n)
			if p.start(0) != w.from || p.start(uint64(n)) != w.to {
				t.Errorf("[%d,%d) n=%d: columns span [%d,%d)", w.from, w.to, n, p.start(0), p.start(uint64(n)))
			}
			for range 1000 {
				ts := w.from + int64(rng.Uint64N(uint64(w.to)-uint64(w.from)))
				i := p.column(ts)
				if i >= uint64(n) || ts < p.start(i) || ts >= p.start(i+1) {
					t.Fatalf("[%d,%d) n=%d: ts %d in column %d = [%d,%d)",
						w.from, w.to, n, ts, i, p.start(i), p.start(i+1))
				}
			}
		}
	}
}

// What a pooled read holds in memory is one column's samples, so that is what
// is bounded: a window pooled into many columns reads fine, the same window in
// one column is refused.
func TestOneColumnsSamplesAreBounded(t *testing.T) {
	lowered(t, 100, 1000, 1000)
	s := openTemp(t)
	ctx := t.Context()
	var ms []Measurement
	for i := range 30 {
		ms = append(ms, rowWith(int64(i*60), 10)) // 300 samples in all
	}
	if err := s.WriteMeasurements(ctx, ms); err != nil {
		t.Fatal(err)
	}
	if _, cols, err := s.QueryColumns(ctx, 1, 0, 0, 1800, 10); err != nil || len(cols) != 10 {
		t.Errorf("30 samples per column: %d columns, %v", len(cols), err)
	}
	if _, _, err := s.QueryColumns(ctx, 1, 0, 0, 1800, 1); !errors.Is(err, ErrRangeTooLarge) {
		t.Errorf("300 samples in one column: err = %v, want ErrRangeTooLarge", err)
	}
}

// A pooled read's size is bounded by its columns, but the time it takes by the
// intervals it walks, so those are bounded too.
func TestAPooledReadWalksABoundedNumberOfIntervals(t *testing.T) {
	old := maxPooledRows
	maxPooledRows = 20
	t.Cleanup(func() { maxPooledRows = old })
	s := openTemp(t)
	ctx := t.Context()
	var ms []Measurement
	for i := range 21 {
		ms = append(ms, rowWith(int64(i*60), 1))
	}
	if err := s.WriteMeasurements(ctx, ms); err != nil {
		t.Fatal(err)
	}
	if _, cols, err := s.QueryColumns(ctx, 1, 0, 0, 20*60, 5); err != nil || len(cols) != 5 {
		t.Errorf("20 intervals: %d columns, %v", len(cols), err)
	}
	if _, _, err := s.QueryColumns(ctx, 1, 0, 0, 21*60, 5); !errors.Is(err, ErrRangeTooLarge) {
		t.Errorf("21 intervals: err = %v, want ErrRangeTooLarge", err)
	}
}

// A window of few intervals but more samples than a raw read returns is pooled
// rather than refused: the columns are the bounded answer.
func TestTooManySamplesForRawArePooled(t *testing.T) {
	lowered(t, 100, 1000, 1000)
	s := openTemp(t)
	ctx := t.Context()
	var ms []Measurement
	for i := range 4 {
		ms = append(ms, rowWith(int64(i*60), 40)) // 160 samples, 4 intervals
	}
	if err := s.WriteMeasurements(ctx, ms); err != nil {
		t.Fatal(err)
	}
	raw, cols, err := s.QueryColumns(ctx, 1, 0, 0, 240, 100)
	if err != nil || raw != nil || len(cols) != 4 {
		t.Errorf("4 intervals of 40 in 100 columns: raw %d, cols %d, %v; want 4 columns", len(raw), len(cols), err)
	}
}

// The extra series pool into the same columns, signed, and keep the three
// states a series has: measured with values, measured with nothing to give
// (one reply, nothing to difference), and not measured at all — absent.
func TestQueryColumnsPoolsTheExtraSeries(t *testing.T) {
	s := openTemp(t)
	ctx := t.Context()
	var ms []Measurement
	for i := range 30 { // three 600 s columns of ten intervals
		m := rowWith(int64(i*60), 3)
		switch {
		case i < 10:
			m.Series = map[string][]int32{SeriesIPDVSend: {int32(-i), int32(i)}}
		case i < 20:
			m.Series = map[string][]int32{SeriesIPDVSend: {}}
		}
		ms = append(ms, m)
	}
	if err := s.WriteMeasurements(ctx, ms); err != nil {
		t.Fatal(err)
	}
	_, cols, err := s.QueryColumns(ctx, 1, 0, 0, 1800, 3)
	if err != nil || len(cols) != 3 {
		t.Fatalf("%d columns, %v", len(cols), err)
	}
	var want []int32
	for i := range 10 {
		want = append(want, int32(-i), int32(i))
	}
	slices.Sort(want)
	if got, ok := cols[0].Series[SeriesIPDVSend]; !ok || !slices.Equal(got, want) {
		t.Errorf("column 0: %v (present %v), want the pooled values %v", got, ok, want)
	}
	if got, ok := cols[1].Series[SeriesIPDVSend]; !ok || len(got) != 0 {
		t.Errorf("column 1: %v (present %v), want present and empty", got, ok)
	}
	if _, ok := cols[2].Series[SeriesIPDVSend]; ok {
		t.Errorf("column 2: present, want absent (nothing measured it)")
	}
}

// The series rows count against the same bound as the measurements.
func TestTheSeriesCountTowardsTheWalkBound(t *testing.T) {
	old := maxPooledRows
	maxPooledRows = 15
	t.Cleanup(func() { maxPooledRows = old })
	s := openTemp(t)
	ctx := t.Context()
	var ms []Measurement
	for i := range 10 {
		m := rowWith(int64(i*60), 1)
		m.Series = map[string][]int32{SeriesIPDVSend: {1}}
		ms = append(ms, m)
	}
	if err := s.WriteMeasurements(ctx, ms); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.QueryColumns(ctx, 1, 0, 0, 600, 2); !errors.Is(err, ErrRangeTooLarge) {
		t.Errorf("10 intervals and 10 series rows against 15: err = %v, want ErrRangeTooLarge", err)
	}
}
