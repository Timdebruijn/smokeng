package api

import (
	"bytes"
	"path/filepath"
	"slices"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/ipc"

	"github.com/timdebruijn/smokeng/internal/store"
	"github.com/timdebruijn/smokeng/internal/tree"
)

func readTable(t *testing.T, b []byte) arrow.Table {
	t.Helper()
	rd, err := ipc.NewReader(bytes.NewReader(b))
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

func column(tb arrow.Table, name string) arrow.Array {
	return tb.Column(tb.Schema().FieldIndices(name)[0]).Data().Chunk(0)
}

// A series that loses the same tail of every burst is flagged by the master from
// the twelfth such interval on. The browser has to be told which intervals, which
// positions, and over how many intervals: and a pooled read how many per column.
func TestPositionalLossReachesTheBrowser(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "positional.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	st.DetectPositionalLoss()
	ctx := t.Context()

	const n = 14 // flagged: the 12th, 13th and 14th
	for i := range n {
		lost := make([]bool, 20)
		lost[17], lost[18], lost[19] = true, true, true
		m := store.Measurement{TargetID: 1, Family: tree.FamilyV4, TS: 1000 + 60*int64(i), Sent: 20, Received: 17, Samples: make([]uint32, 17)}
		m.LossMask = store.EncodeLossMask(false, lost)
		if err := st.WriteMeasurements(ctx, []store.Measurement{m}); err != nil {
			t.Fatal(err)
		}
	}

	ms, err := st.QueryRange(ctx, 1, 0, tree.FamilyV4, 1000, 1000+60*n)
	if err != nil || len(ms) != n {
		t.Fatalf("%d rows, err %v", len(ms), err)
	}
	var buf bytes.Buffer
	writeMeasurementsArrow(&buf, ms, batchRows)
	raw := readTable(t, buf.Bytes())
	pos := column(raw, "loss_positions").(*array.List)
	win := column(raw, "loss_window").(*array.Uint8)
	for i := range n {
		if flagged := i >= 11; pos.IsValid(i) != flagged || win.IsValid(i) != flagged {
			t.Fatalf("interval %d: positions valid %v, window valid %v, want both %v", i, pos.IsValid(i), win.IsValid(i), flagged)
		}
	}
	start, end := pos.ValueOffsets(13)
	if got := pos.ListValues().(*array.Uint16).Uint16Values()[start:end]; !slices.Equal(got, []uint16{18, 19, 20}) {
		t.Errorf("positions = %v, want 18 19 20 (1-based)", got)
	}
	if win.Value(11) != 12 || win.Value(13) != 14 {
		t.Errorf("window = %d, %d, want 12, 14", win.Value(11), win.Value(13))
	}

	// Pooled into two columns of seven intervals each, the flagged 12th to 14th
	// all fall in the second.
	_, cols, err := st.QueryColumns(ctx, 1, 0, tree.FamilyV4, 1000, 1000+60*n, 2)
	if err != nil || len(cols) != 2 {
		t.Fatalf("%d columns, err %v", len(cols), err)
	}
	buf.Reset()
	writePooledArrow(&buf, cols, batchRows)
	pooled := column(readTable(t, buf.Bytes()), "positional").(*array.Uint32)
	if pooled.Value(0) != 0 || pooled.Value(1) != 3 {
		t.Errorf("positional per column = %d, %d, want 0, 3", pooled.Value(0), pooled.Value(1))
	}
}
