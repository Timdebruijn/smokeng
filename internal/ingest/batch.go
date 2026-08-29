package ingest

import (
	"bytes"
	"fmt"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/ipc"
	"github.com/apache/arrow-go/v18/arrow/memory"

	"smokeng/internal/store"
)

// BatchSchema is the wire format for a submission: the measurement schema the
// browser already receives (DESIGN.md §7.2), plus the target id, since one
// batch spans many targets. Agent and master share this encoder and decoder,
// so there is no second format to keep in step.
var BatchSchema = arrow.NewSchema([]arrow.Field{
	{Name: "target_id", Type: arrow.PrimitiveTypes.Int64},
	{Name: "ts", Type: &arrow.TimestampType{Unit: arrow.Second, TimeZone: "UTC"}},
	{Name: "sent", Type: arrow.PrimitiveTypes.Uint16},
	{Name: "received", Type: arrow.PrimitiveTypes.Uint16},
	{Name: "flags", Type: arrow.PrimitiveTypes.Uint8},
	{Name: "samples", Type: arrow.ListOf(arrow.PrimitiveTypes.Uint32)},
	{Name: "icmp_error", Type: arrow.PrimitiveTypes.Uint16, Nullable: true},
}, nil)

// EncodeBatch serialises measurements for submission.
func EncodeBatch(ms []store.Measurement) ([]byte, error) {
	var buf bytes.Buffer
	w := ipc.NewWriter(&buf, ipc.WithSchema(BatchSchema))
	rb := array.NewRecordBuilder(memory.DefaultAllocator, BatchSchema)
	defer rb.Release()

	targetB := rb.Field(0).(*array.Int64Builder)
	tsB := rb.Field(1).(*array.TimestampBuilder)
	sentB := rb.Field(2).(*array.Uint16Builder)
	recvB := rb.Field(3).(*array.Uint16Builder)
	flagsB := rb.Field(4).(*array.Uint8Builder)
	listB := rb.Field(5).(*array.ListBuilder)
	valB := listB.ValueBuilder().(*array.Uint32Builder)
	icmpB := rb.Field(6).(*array.Uint16Builder)

	for _, m := range ms {
		targetB.Append(m.TargetID)
		tsB.Append(arrow.Timestamp(m.TS))
		sentB.Append(uint16(m.Sent))
		recvB.Append(uint16(m.Received))
		flagsB.Append(m.Flags)
		listB.Append(true)
		valB.AppendValues(m.Samples, nil)
		if m.ICMPErr != nil {
			icmpB.Append(*m.ICMPErr)
		} else {
			icmpB.AppendNull()
		}
	}
	rec := rb.NewRecord()
	defer rec.Release()
	if err := w.Write(rec); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// DecodeBatch reads a submission, stamping every measurement with the
// authenticated agent's id. The id is never taken from the payload: an agent
// must not be able to write to another agent's series by saying so.
func DecodeBatch(body []byte, agentID int64) ([]store.Measurement, error) {
	reader, err := ipc.NewReader(bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer reader.Release()

	var out []store.Measurement
	for reader.Next() {
		rec := reader.Record()
		if rec.NumCols() != int64(len(BatchSchema.Fields())) {
			return nil, fmt.Errorf("ingest: batch has %d columns, want %d",
				rec.NumCols(), len(BatchSchema.Fields()))
		}
		targets, ok1 := rec.Column(0).(*array.Int64)
		ts, ok2 := rec.Column(1).(*array.Timestamp)
		sent, ok3 := rec.Column(2).(*array.Uint16)
		recv, ok4 := rec.Column(3).(*array.Uint16)
		flags, ok5 := rec.Column(4).(*array.Uint8)
		samples, ok6 := rec.Column(5).(*array.List)
		icmp, ok7 := rec.Column(6).(*array.Uint16)
		if !ok1 || !ok2 || !ok3 || !ok4 || !ok5 || !ok6 || !ok7 {
			return nil, fmt.Errorf("ingest: batch column types do not match the schema")
		}
		values, ok := samples.ListValues().(*array.Uint32)
		if !ok {
			return nil, fmt.Errorf("ingest: samples are not a list of uint32")
		}
		offsets := samples.Offsets()

		for i := range int(rec.NumRows()) {
			m := store.Measurement{
				TargetID: targets.Value(i),
				AgentID:  agentID,
				TS:       int64(ts.Value(i)),
				Sent:     int(sent.Value(i)),
				Received: int(recv.Value(i)),
				Flags:    flags.Value(i),
			}
			for j := offsets[i]; j < offsets[i+1]; j++ {
				m.Samples = append(m.Samples, values.Value(int(j)))
			}
			// The store's invariant, checked here so a malformed batch is
			// refused at the door rather than at the write.
			if m.Received != len(m.Samples) {
				return nil, fmt.Errorf("ingest: target %d at %d claims %d replies but carries %d samples",
					m.TargetID, m.TS, m.Received, len(m.Samples))
			}
			if !icmp.IsNull(i) {
				v := icmp.Value(i)
				m.ICMPErr = &v
			}
			out = append(out, m)
		}
	}
	return out, reader.Err()
}
