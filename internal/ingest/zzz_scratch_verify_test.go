package ingest

import (
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/ipc"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// THROWAWAY verification test, not part of the PR. Confirms or refutes the
// claim in docs/operations.md that "a master too old to know a column an
// agent sends refuses the batch outright." Deleted immediately after running.
var schemaWithExtraCol = arrow.NewSchema([]arrow.Field{
	{Name: "target_id", Type: arrow.PrimitiveTypes.Int64},
	{Name: "ts", Type: &arrow.TimestampType{Unit: arrow.Second, TimeZone: "UTC"}},
	{Name: "sent", Type: arrow.PrimitiveTypes.Uint16},
	{Name: "received", Type: arrow.PrimitiveTypes.Uint16},
	{Name: "flags", Type: arrow.PrimitiveTypes.Uint8},
	{Name: "samples", Type: arrow.ListOf(arrow.PrimitiveTypes.Uint32)},
	{Name: "icmp_error", Type: arrow.PrimitiveTypes.Uint16, Nullable: true},
	{Name: "totally_unknown_future_column", Type: arrow.PrimitiveTypes.Float64, Nullable: true},
}, nil)

type zzzBuf struct{ b []byte }

func (w *zzzBuf) Write(p []byte) (int, error) { w.b = append(w.b, p...); return len(p), nil }

func TestZZZOldMasterAgainstNewerAgentExtraColumn(t *testing.T) {
	rb := array.NewRecordBuilder(memory.DefaultAllocator, schemaWithExtraCol)
	defer rb.Release()

	rb.Field(0).(*array.Int64Builder).Append(42)
	rb.Field(1).(*array.TimestampBuilder).Append(arrow.Timestamp(1000))
	rb.Field(2).(*array.Uint16Builder).Append(3)
	rb.Field(3).(*array.Uint16Builder).Append(3)
	rb.Field(4).(*array.Uint8Builder).Append(0)
	lb := rb.Field(5).(*array.ListBuilder)
	lb.Append(true)
	lb.ValueBuilder().(*array.Uint32Builder).AppendValues([]uint32{1, 2, 3}, nil)
	rb.Field(6).(*array.Uint16Builder).AppendNull()
	rb.Field(7).(*array.Float64Builder).Append(3.14)

	rec := rb.NewRecord()
	defer rec.Release()

	var buf zzzBuf
	w := ipc.NewWriter(&buf, ipc.WithSchema(schemaWithExtraCol))
	if err := w.Write(rec); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	ms, err := DecodeBatch(buf.b, 7)
	if err != nil {
		t.Fatalf("DECODE REFUSED a batch with only one extra unrecognised column: %v", err)
	}
	t.Logf("decoded fine: %d measurement(s): %+v", len(ms), ms)
}
