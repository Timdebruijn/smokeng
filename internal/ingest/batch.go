package ingest

import (
	"bytes"
	"fmt"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/ipc"
	"github.com/apache/arrow-go/v18/arrow/memory"

	"github.com/timdebruijn/smokeng/internal/store"
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
	// The extra per-packet series, one nullable column each. Null means the
	// probe did not measure it — an irtt peer that returns no timestamps, or
	// any other probe type, for which these are simply not a thing. An empty
	// list would say something different and wrong: measured, and flat.
	{Name: store.SeriesIPDVSend, Type: arrow.ListOf(arrow.PrimitiveTypes.Int32), Nullable: true},
	{Name: store.SeriesIPDVReceive, Type: arrow.ListOf(arrow.PrimitiveTypes.Int32), Nullable: true},
	{Name: store.SeriesServerProcessing, Type: arrow.ListOf(arrow.PrimitiveTypes.Int32), Nullable: true},
}, nil)

// seriesColumns are the schema's optional series columns, in field order. They
// are optional on the wire as well as nullable: an agent built before they
// existed sends a batch without them, and the master reads what is there.
var seriesColumns = []string{store.SeriesIPDVSend, store.SeriesIPDVReceive, store.SeriesServerProcessing}

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
	seriesB := make([]*array.ListBuilder, len(seriesColumns))
	seriesV := make([]*array.Int32Builder, len(seriesColumns))
	for i := range seriesColumns {
		seriesB[i] = rb.Field(7 + i).(*array.ListBuilder)
		seriesV[i] = seriesB[i].ValueBuilder().(*array.Int32Builder)
	}

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
		for i, name := range seriesColumns {
			vals, ok := m.Series[name]
			if !ok {
				seriesB[i].AppendNull()
				continue
			}
			seriesB[i].Append(true)
			seriesV[i].AppendValues(vals, nil)
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
		// Columns are resolved by name, not by position. The series columns
		// were added after the first agents shipped, and an agent that predates
		// them sends a batch without them; matching on a column count would
		// reject that agent's whole submission — every measurement lost, to
		// gain three optional distributions. Missing required columns are still
		// an error, and are named.
		col := func(name string) arrow.Array {
			for i, f := range rec.Schema().Fields() {
				if f.Name == name {
					return rec.Column(i)
				}
			}
			return nil
		}
		var missing []string
		must := func(name string) arrow.Array {
			c := col(name)
			if c == nil {
				missing = append(missing, name)
			}
			return c
		}
		cTargets, cTS, cSent := must("target_id"), must("ts"), must("sent")
		cRecv, cFlags, cSamples, cICMP := must("received"), must("flags"), must("samples"), must("icmp_error")
		if len(missing) > 0 {
			return nil, fmt.Errorf("ingest: batch is missing column(s) %v", missing)
		}
		targets, ok1 := cTargets.(*array.Int64)
		ts, ok2 := cTS.(*array.Timestamp)
		sent, ok3 := cSent.(*array.Uint16)
		recv, ok4 := cRecv.(*array.Uint16)
		flags, ok5 := cFlags.(*array.Uint8)
		samples, ok6 := cSamples.(*array.List)
		icmp, ok7 := cICMP.(*array.Uint16)
		if !ok1 || !ok2 || !ok3 || !ok4 || !ok5 || !ok6 || !ok7 {
			return nil, fmt.Errorf("ingest: batch column types do not match the schema")
		}
		series := make(map[string]*array.List, len(seriesColumns))
		for _, name := range seriesColumns {
			c := col(name)
			if c == nil {
				continue // an agent older than this series
			}
			l, ok := c.(*array.List)
			if !ok {
				return nil, fmt.Errorf("ingest: column %q is not a list", name)
			}
			if _, ok := l.ListValues().(*array.Int32); !ok {
				return nil, fmt.Errorf("ingest: column %q is not a list of int32", name)
			}
			series[name] = l
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
			for _, name := range seriesColumns {
				l := series[name]
				if l == nil || l.IsNull(i) {
					continue // not measured; absence is the record of that
				}
				vals := l.ListValues().(*array.Int32)
				off := l.Offsets()
				got := make([]int32, 0, off[i+1]-off[i])
				for j := off[i]; j < off[i+1]; j++ {
					got = append(got, vals.Value(int(j)))
				}
				if m.Series == nil {
					m.Series = make(map[string][]int32, len(seriesColumns))
				}
				m.Series[name] = got
			}
			out = append(out, m)
		}
	}
	return out, reader.Err()
}
