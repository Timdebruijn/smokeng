package ingest

import (
	"bytes"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/ipc"
	"github.com/apache/arrow-go/v18/arrow/memory"

	"github.com/timdebruijn/smokeng/internal/store"
	"github.com/timdebruijn/smokeng/internal/tree"
)

// Protects the family crossing the wire. Without the column, the master files a
// backlog under the target's family at arrival, and cannot tell at all once a
// target is measured over both families. Unknown must travel as null, not as 0:
// the master reads null as "decide for yourself" and 0 as a malformed value.
func TestBatchCarriesFamily(t *testing.T) {
	in := []store.Measurement{
		{TargetID: 1, TS: 100, Sent: 1, Received: 1, Samples: []uint32{10}, Family: tree.FamilyV4},
		{TargetID: 1, TS: 160, Sent: 1, Received: 1, Samples: []uint32{10}, Family: tree.FamilyV6},
		{TargetID: 1, TS: 220, Sent: 1, Received: 1, Samples: []uint32{10}, Family: tree.FamilyUnknown},
	}
	out, err := DecodeBatch(mustEncode(t, in), 7)
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []tree.Family{tree.FamilyV4, tree.FamilyV6, tree.FamilyUnknown} {
		if out[i].Family != want {
			t.Errorf("row %d: family = %v, want %v", i, out[i].Family, want)
		}
	}
}

// Protects the rule that a column added to the batch is optional: an agent that
// predates family sends none, and its rows decode as unknown for the master to
// place. The batch is a current one with the column cut out, so this fails if
// the decoder starts requiring it.
func TestDecodeBatchAcceptsAgentWithoutFamily(t *testing.T) {
	body := withoutColumn(t, mustEncode(t, []store.Measurement{
		{TargetID: 1, TS: 100, Sent: 1, Received: 1, Samples: []uint32{10}, Family: tree.FamilyV6},
	}), "family")
	out, err := DecodeBatch(body, 7)
	if err != nil {
		t.Fatalf("a batch without the family column was refused: %v", err)
	}
	if len(out) != 1 || out[0].Family != tree.FamilyUnknown {
		t.Fatalf("decoded %+v, want one row of unknown family", out)
	}
}

// Protects the measurements of a batch whose family column holds something
// that is not a family. The agent discards a batch the master refuses, so
// refusing it for a label would lose every row in it; the value is read as none
// stated and the row is kept, for the master to file as an older agent's.
func TestDecodeBatchKeepsARowWhoseFamilyItCannotRead(t *testing.T) {
	for _, bad := range []uint8{0, 5, 255} {
		var buf bytes.Buffer
		w := ipc.NewWriter(&buf, ipc.WithSchema(BatchSchema))
		rb := array.NewRecordBuilder(memory.DefaultAllocator, BatchSchema)
		field[*array.Int64Builder](t, rb, "target_id").Append(1)
		field[*array.TimestampBuilder](t, rb, "ts").Append(arrow.Timestamp(100))
		field[*array.Uint16Builder](t, rb, "sent").Append(1)
		field[*array.Uint16Builder](t, rb, "received").Append(0)
		field[*array.Uint8Builder](t, rb, "flags").Append(0)
		field[*array.ListBuilder](t, rb, "samples").Append(true)
		field[*array.Uint16Builder](t, rb, "icmp_error").AppendNull()
		field[*array.Uint8Builder](t, rb, "send_error").AppendNull()
		field[*array.BinaryBuilder](t, rb, "loss_mask").AppendNull()
		field[*array.Uint8Builder](t, rb, "family").Append(bad)
		for _, name := range store.KnownSeries {
			field[*array.ListBuilder](t, rb, name).AppendNull()
		}
		rec := rb.NewRecord()
		if err := w.Write(rec); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		rec.Release()
		rb.Release()

		out, err := DecodeBatch(buf.Bytes(), 7)
		if err != nil {
			t.Fatalf("family %d: the batch was refused: %v", bad, err)
		}
		if len(out) != 1 || out[0].Family != tree.FamilyUnknown || out[0].TargetID != 1 || out[0].Sent != 1 {
			t.Fatalf("family %d: decoded %+v, want the row kept with no family stated", bad, out)
		}
	}
}

// withoutColumn returns the batch with one column removed, as an agent built
// before that column would have sent it.
func withoutColumn(t *testing.T, body []byte, name string) []byte {
	t.Helper()
	r, err := ipc.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Release()
	if !r.Next() {
		t.Fatal("no record in the batch")
	}
	rec := r.Record()
	var fields []arrow.Field
	var cols []arrow.Array
	for i, f := range rec.Schema().Fields() {
		if f.Name != name {
			fields = append(fields, f)
			cols = append(cols, rec.Column(i))
		}
	}
	if len(fields) == rec.Schema().NumFields() {
		t.Fatalf("the batch has no column %q to remove", name)
	}
	schema := arrow.NewSchema(fields, nil)
	out := array.NewRecord(schema, cols, rec.NumRows())
	defer out.Release()
	var buf bytes.Buffer
	w := ipc.NewWriter(&buf, ipc.WithSchema(schema))
	if err := w.Write(out); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
