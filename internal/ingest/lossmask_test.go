package ingest

import (
	"bytes"
	"log"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/ipc"
	"github.com/apache/arrow-go/v18/arrow/memory"

	"github.com/timdebruijn/smokeng/internal/store"
)

// The mask is only useful on the master if it survives the trip, and a row
// without one must not come out with an empty one.
func TestBatchCarriesLossMask(t *testing.T) {
	mask := store.EncodeLossMask(true, []bool{false, false, true, true})
	in := []store.Measurement{
		{TargetID: 1, TS: 100, Sent: 4, Received: 2, Samples: []uint32{10, 20}, LossMask: mask},
		{TargetID: 2, TS: 100, Sent: 1, Received: 1, Samples: []uint32{20}},
	}
	out, err := DecodeBatch(mustEncode(t, in), 7)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(out[0].LossMask, mask) || out[1].LossMask != nil {
		t.Errorf("masks = %v, %v; want %v, nil", out[0].LossMask, out[1].LossMask, mask)
	}
}

// An agent older than the column sends a batch without it. It has to be
// accepted whole, every row with no mask: refusing it makes upgrading the master
// a flag day, and a refused batch is discarded by the agent.
func TestDecodeBatchAcceptsAgentWithoutLossMask(t *testing.T) {
	var fields []arrow.Field
	for _, f := range BatchSchema.Fields() {
		if f.Name != "loss_mask" {
			fields = append(fields, f)
		}
	}
	old := arrow.NewSchema(fields, nil)
	var buf bytes.Buffer
	w := ipc.NewWriter(&buf, ipc.WithSchema(old))
	rb := array.NewRecordBuilder(memory.DefaultAllocator, old)
	defer rb.Release()
	for i, f := range old.Fields() {
		switch b := rb.Field(i).(type) {
		case *array.Int64Builder:
			b.Append(1)
		case *array.TimestampBuilder:
			b.Append(arrow.Timestamp(100))
		case *array.Uint16Builder:
			if f.Name == "sent" || f.Name == "received" {
				b.Append(2)
			} else {
				b.AppendNull()
			}
		case *array.Uint8Builder:
			if f.Name == "flags" {
				b.Append(0)
			} else {
				b.AppendNull()
			}
		case *array.ListBuilder:
			if f.Name == "samples" {
				b.Append(true)
				b.ValueBuilder().(*array.Uint32Builder).AppendValues([]uint32{10, 20}, nil)
			} else {
				b.AppendNull()
			}
		}
	}
	rec := rb.NewRecord()
	defer rec.Release()
	if err := w.Write(rec); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	out, err := DecodeBatch(buf.Bytes(), 3)
	if err != nil {
		t.Fatalf("a batch without loss_mask was refused: %v", err)
	}
	if len(out) != 1 || out[0].LossMask != nil || out[0].Received != 2 {
		t.Errorf("got %+v", out)
	}
}

// A mask comes from another machine. One that does not fit its row is dropped
// and the row kept, because refusing the batch makes the agent discard it and
// lose measurements over an annotation; the drop is logged once per batch.
func TestDecodeBatchDropsInvalidLossMask(t *testing.T) {
	good := store.EncodeLossMask(false, []bool{false, true, false, false})
	badCount := store.EncodeLossMask(false, []bool{true, true, false, false})
	badLen := append(append([]byte(nil), good...), 0)
	badHeader := append([]byte(nil), good...)
	badHeader[0] |= 4
	row := func(id int64, mask []byte) store.Measurement {
		return store.Measurement{TargetID: id, TS: 100, Sent: 4, Received: 3,
			Samples: []uint32{1, 2, 3}, LossMask: mask}
	}
	in := []store.Measurement{row(1, good), row(2, badCount), row(3, badLen), row(4, badHeader)}

	var logged bytes.Buffer
	log.SetOutput(&logged)
	defer log.SetOutput(os.Stderr)

	out, err := DecodeBatch(mustEncode(t, in), 7)
	if err != nil {
		t.Fatalf("a bad mask refused the batch: %v", err)
	}
	if len(out) != 4 {
		t.Fatalf("got %d rows, want all 4 kept", len(out))
	}
	if !reflect.DeepEqual(out[0].LossMask, good) {
		t.Errorf("the valid mask was lost: %v", out[0].LossMask)
	}
	for _, m := range out[1:] {
		if m.LossMask != nil {
			t.Errorf("target %d kept an invalid mask %v", m.TargetID, m.LossMask)
		}
	}
	if n := strings.Count(logged.String(), "loss mask"); n != 1 || !strings.Contains(logged.String(), "3 loss mask") {
		t.Errorf("want one log line counting 3 dropped masks, got:\n%s", logged.String())
	}
}
