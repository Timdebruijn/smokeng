package api

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/ipc"
)

// columns= asks for a read sized to a plot. A window that fits comes back in the
// ordinary schema; one that does not comes back pooled, one row per column, and
// the two are told apart by their fields. columns= out of range is a 400.
func TestMeasurementsWithColumns(t *testing.T) {
	f := tenants(t)
	h := New(f.st, Options{Alerts: f.mgr}, fstest.MapFS{})
	// hostA has five one-sample intervals of 100 µs, a minute apart, from now-600.
	base := fmt.Sprintf("/api/v1/measurements?target_id=%d&agent_id=0&from=%d&to=%d", f.hostA, f.now-600, f.now)

	for _, bad := range []string{"0", "-1", "x", "8193"} {
		if code, body := call(t, h, "GET", base+"&columns="+bad, nil); code != http.StatusBadRequest || !strings.Contains(body, "columns") {
			t.Errorf("columns=%s: %d %.200s, want a 400 naming columns", bad, code, body)
		}
	}

	read := func(n int) arrow.Table {
		t.Helper()
		code, body := call(t, h, "GET", fmt.Sprintf("%s&columns=%d", base, n), nil)
		if code != http.StatusOK {
			t.Fatalf("columns=%d: %d %.200s", n, code, body)
		}
		rd, err := ipc.NewReader(strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer rd.Release()
		var recs []arrow.RecordBatch
		for rd.Next() {
			rec := rd.RecordBatch()
			rec.Retain()
			recs = append(recs, rec)
		}
		return array.NewTableFromRecords(rd.Schema(), recs)
	}

	raw := read(10)
	if raw.Schema().HasField("rounds") || raw.NumRows() != 5 {
		t.Errorf("5 intervals in 10 columns: %d rows of %v, want the 5 intervals as they are", raw.NumRows(), raw.Schema())
	}

	pooled := read(2) // two 300 s columns; all five start in the first
	if !pooled.Schema().HasField("rounds") || pooled.NumRows() != 1 {
		t.Fatalf("5 intervals in 2 columns: %d rows of %v, want one pooled column", pooled.NumRows(), pooled.Schema())
	}
	col := func(name string) arrow.Array {
		return pooled.Column(pooled.Schema().FieldIndices(name)[0]).Data().Chunk(0)
	}
	if ts, end := col("ts").(*array.Timestamp).Value(0), col("ts_end").(*array.Timestamp).Value(0); int64(ts) != f.now-600 || int64(end) != f.now-300 {
		t.Errorf("column spans [%d,%d), want [now-600, now-300)", ts, end)
	}
	if r, s, v := col("rounds").(*array.Uint32).Value(0), col("sent").(*array.Uint64).Value(0), col("received").(*array.Uint64).Value(0); r != 5 || s != 5 || v != 5 {
		t.Errorf("rounds %d sent %d received %d, want 5 5 5", r, s, v)
	}
	q := col("quantiles").(*array.List)
	start, end := q.ValueOffsets(0)
	vals := q.ListValues().(*array.Uint32).Uint32Values()[start:end]
	if !slices.Equal(vals, []uint32{100, 100, 100, 100, 100}) {
		t.Errorf("quantiles %v, want the five samples", vals)
	}
}
