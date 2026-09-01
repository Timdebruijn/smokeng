package ingest

import (
	"bytes"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/ipc"
	"github.com/apache/arrow-go/v18/arrow/memory"

	"github.com/timdebruijn/smokeng/internal/store"
)

func TestBatchCarriesSeries(t *testing.T) {
	in := []store.Measurement{
		{TargetID: 1, TS: 100, Sent: 3, Received: 3, Samples: []uint32{10, 20, 30},
			Series: map[string][]int32{
				store.SeriesIPDVSend:    {-40, -1, 12},
				store.SeriesIPDVReceive: {-9, 0, 3},
			}},
		// A second row with no series at all, so the null path is exercised in
		// the same batch as the populated one.
		{TargetID: 2, TS: 100, Sent: 1, Received: 1, Samples: []uint32{50}},
	}
	blob, err := EncodeBatch(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := DecodeBatch(blob, 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Fatalf("got %d measurements, want 2", len(out))
	}
	send := out[0].Series[store.SeriesIPDVSend]
	if len(send) != 3 || send[0] != -40 || send[2] != 12 {
		t.Errorf("ipdv_send = %v, want [-40 -1 12]", send)
	}
	if _, ok := out[0].Series[store.SeriesServerProcessing]; ok {
		t.Error("a series that was never measured arrived present")
	}
	if out[1].Series != nil {
		t.Errorf("a measurement with no series arrived carrying %v", out[1].Series)
	}
}

// An agent built before the series columns existed submits a batch without
// them. Rejecting it would throw away every measurement in the submission to
// gain three optional distributions, and would make upgrading the master a
// flag day for every remote agent.
func TestDecodeBatchAcceptsPreSeriesAgent(t *testing.T) {
	old := arrow.NewSchema([]arrow.Field{
		{Name: "target_id", Type: arrow.PrimitiveTypes.Int64},
		{Name: "ts", Type: &arrow.TimestampType{Unit: arrow.Second, TimeZone: "UTC"}},
		{Name: "sent", Type: arrow.PrimitiveTypes.Uint16},
		{Name: "received", Type: arrow.PrimitiveTypes.Uint16},
		{Name: "flags", Type: arrow.PrimitiveTypes.Uint8},
		{Name: "samples", Type: arrow.ListOf(arrow.PrimitiveTypes.Uint32)},
		{Name: "icmp_error", Type: arrow.PrimitiveTypes.Uint16, Nullable: true},
	}, nil)

	var buf bytes.Buffer
	w := ipc.NewWriter(&buf, ipc.WithSchema(old))
	rb := array.NewRecordBuilder(memory.DefaultAllocator, old)
	defer rb.Release()
	rb.Field(0).(*array.Int64Builder).Append(4)
	rb.Field(1).(*array.TimestampBuilder).Append(arrow.Timestamp(900))
	rb.Field(2).(*array.Uint16Builder).Append(2)
	rb.Field(3).(*array.Uint16Builder).Append(2)
	rb.Field(4).(*array.Uint8Builder).Append(0)
	list := rb.Field(5).(*array.ListBuilder)
	list.Append(true)
	list.ValueBuilder().(*array.Uint32Builder).AppendValues([]uint32{11, 22}, nil)
	rb.Field(6).(*array.Uint16Builder).AppendNull()
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
		t.Fatalf("a batch from an agent without the series columns was refused: %v", err)
	}
	if len(out) != 1 || out[0].Received != 2 || len(out[0].Samples) != 2 {
		t.Fatalf("measurement did not survive: %+v", out)
	}
	if out[0].Series != nil {
		t.Errorf("series invented for an agent that sent none: %v", out[0].Series)
	}
}

// A batch missing a column that is genuinely required is still refused, and
// says which one, rather than being read with a silently absent field.
func TestDecodeBatchRefusesMissingRequiredColumn(t *testing.T) {
	bad := arrow.NewSchema([]arrow.Field{
		{Name: "target_id", Type: arrow.PrimitiveTypes.Int64},
		{Name: "ts", Type: &arrow.TimestampType{Unit: arrow.Second, TimeZone: "UTC"}},
	}, nil)
	var buf bytes.Buffer
	w := ipc.NewWriter(&buf, ipc.WithSchema(bad))
	rb := array.NewRecordBuilder(memory.DefaultAllocator, bad)
	defer rb.Release()
	rb.Field(0).(*array.Int64Builder).Append(1)
	rb.Field(1).(*array.TimestampBuilder).Append(arrow.Timestamp(1))
	rec := rb.NewRecord()
	defer rec.Release()
	if err := w.Write(rec); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	_, err := DecodeBatch(buf.Bytes(), 1)
	if err == nil {
		t.Fatal("a batch with no samples column was accepted")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("samples")) {
		t.Errorf("error does not name the missing column: %v", err)
	}
}

// The same distinction has to survive the wire: an empty list and a null
// column mean different things, and only one of them is "not measured".
func TestBatchKeepsEmptySeriesDistinctFromAbsent(t *testing.T) {
	in := []store.Measurement{{
		TargetID: 1, TS: 100, Sent: 1, Received: 1, Samples: []uint32{10},
		Series: map[string][]int32{store.SeriesIPDVSend: {}},
	}}
	out, err := DecodeBatch(mustEncode(t, in), 7)
	if err != nil {
		t.Fatal(err)
	}
	v, ok := out[0].Series[store.SeriesIPDVSend]
	if !ok {
		t.Fatal("an empty-but-measured series arrived absent")
	}
	if len(v) != 0 {
		t.Errorf("ipdv_send = %v, want empty", v)
	}
	if _, ok := out[0].Series[store.SeriesIPDVReceive]; ok {
		t.Error("an absent series arrived present")
	}
}

func mustEncode(t *testing.T, ms []store.Measurement) []byte {
	t.Helper()
	b, err := EncodeBatch(ms)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The reason has to cross the wire, or it exists only for locally probed
// targets. Making EncodeBatch always append null left every test green.
func TestBatchCarriesSendReason(t *testing.T) {
	want := store.SendReasonRefused
	in := []store.Measurement{
		{TargetID: 1, TS: 100, Sent: 2, Received: 1, Flags: store.FlagSendFailed,
			Samples: []uint32{10}, SendErr: &want},
		{TargetID: 2, TS: 100, Sent: 1, Received: 1, Samples: []uint32{20}},
	}
	blob, err := EncodeBatch(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := DecodeBatch(blob, 7)
	if err != nil {
		t.Fatal(err)
	}
	if out[0].SendErr == nil || *out[0].SendErr != want {
		t.Errorf("SendErr = %v, want %d", out[0].SendErr, want)
	}
	if out[1].SendErr != nil {
		t.Errorf("a clean measurement arrived with reason %d", *out[1].SendErr)
	}
}

// A master must read a batch from an agent newer than itself, ignoring any
// column it has never heard of. docs/operations.md tells operators they may
// upgrade in either order on the strength of this; an earlier version of that
// paragraph claimed the opposite, and nothing contradicted it.
func TestDecodeBatchIgnoresUnknownColumn(t *testing.T) {
	fields := append(BatchSchema.Fields(),
		arrow.Field{Name: "from_a_later_release", Type: arrow.PrimitiveTypes.Uint8, Nullable: true})
	future := arrow.NewSchema(fields, nil)

	var buf bytes.Buffer
	w := ipc.NewWriter(&buf, ipc.WithSchema(future))
	rb := array.NewRecordBuilder(memory.DefaultAllocator, future)
	defer rb.Release()
	for i, f := range future.Fields() {
		switch b := rb.Field(i).(type) {
		case *array.Int64Builder:
			b.Append(1)
		case *array.TimestampBuilder:
			b.Append(arrow.Timestamp(100))
		case *array.Uint16Builder:
			if f.Name == "sent" || f.Name == "received" {
				b.Append(1)
			} else {
				b.AppendNull()
			}
		case *array.Uint8Builder:
			b.AppendNull()
		case *array.ListBuilder:
			if f.Name == "samples" {
				b.Append(true)
				b.ValueBuilder().(*array.Uint32Builder).AppendValues([]uint32{10}, nil)
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
	out, err := DecodeBatch(buf.Bytes(), 1)
	if err != nil {
		t.Fatalf("a batch from a newer agent was refused: %v", err)
	}
	if len(out) != 1 || len(out[0].Samples) != 1 {
		t.Fatalf("the measurement did not survive: %+v", out)
	}
}
