package ingest

import (
	"bytes"
	"fmt"
	"log"
	"slices"

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
	// Why the local side could not send, when it could not. Optional on the
	// wire like the series columns: an agent built before it simply omits it.
	{Name: "send_error", Type: arrow.PrimitiveTypes.Uint8, Nullable: true},
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
	sendErrB := rb.Field(7).(*array.Uint8Builder)
	seriesB := make([]*array.ListBuilder, len(seriesColumns))
	seriesV := make([]*array.Int32Builder, len(seriesColumns))
	for i := range seriesColumns {
		seriesB[i] = rb.Field(8 + i).(*array.ListBuilder)
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
		if m.SendErr != nil {
			sendErrB.Append(*m.SendErr)
		} else {
			sendErrB.AppendNull()
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
	return decodeBatch(body, agentID, maxDecodeBytes)
}

// decodeBatch is DecodeBatch with the allocation budget as a parameter, so a
// test can exercise the real reader against a small one instead of having to
// build a payload large enough to cross the production limit.
func decodeBatch(body []byte, agentID int64, limit int) ([]store.Measurement, error) {
	// Before arrow sees a byte: every length the stream declares has to fit in
	// the payload that is actually here, and nothing may be compressed. See
	// screenFrames for why the allocator below cannot do this on its own.
	if err := screenFrames(body); err != nil {
		return nil, err
	}
	return decodeFrames(body, agentID, limit)
}

// MaxBatchRows bounds one submission. An agent sends at most pushBatch (2000)
// rows at a time, and a test in the agent package holds that below this, so a
// larger batch size cannot be introduced there without being refused here: the
// master answers 400 and an agent drops a batch it was told it cannot decode.
// The headroom is for a different batch size, and the cap is there because a
// body of a few megabytes can otherwise carry well over a hundred thousand
// rows, each materialised outside the Arrow budget and written in one
// transaction.
const MaxBatchRows = 10_000

// decodeFrames is decodeBatch past the framing screen, with the allocation
// budget as a parameter so a test can run the real reader against a small one.
// It is also what a test calls to check that its own limits hold without the
// screen in front of it.
func decodeFrames(body []byte, agentID int64, limit int) (ms []store.Measurement, err error) {
	// The allocator bounds what the reader allocates through it and reports
	// crossing the bound by panicking (see limit.go). What actually stops the
	// OOM is the refusal itself: the allocation is never made, and a Go
	// out-of-memory is fatal and unrecoverable.
	//
	// Anything else that panics is the reader meeting a header that cannot be
	// true — a validity bitmap shorter than its column, say. That is hostile or
	// corrupt input, so it is an error about the batch, not a stack trace and
	// not a request that takes net/http's recovery to survive.
	mem := newLimitAllocator(limit)
	defer func() {
		r := recover()
		if r == nil {
			return
		}
		if lim, ok := r.(*errAllocationLimit); ok {
			ms, err = nil, lim
			return
		}
		ms, err = nil, fmt.Errorf("ingest: malformed batch: %v", r)
	}()
	// The size limits are belt and braces behind screenFrames: NewReader builds
	// its message reader without them, which is how a declared header length
	// became an allocation of that size. The body is the most any message in it
	// can honestly be.
	mr := ipc.NewMessageReader(bytes.NewReader(body),
		ipc.WithAllocator(mem),
		ipc.WithMetadataSizeLimit(int64(len(body))),
		ipc.WithBodySizeLimit(int64(len(body))))
	reader, err := ipc.NewReaderFromMessageReader(mr, ipc.WithAllocator(mem))
	if err != nil {
		return nil, err
	}
	defer reader.Release()

	var out []store.Measurement
	var resorted, impossible int
	for reader.Next() {
		rec := reader.Record()
		if len(out)+int(rec.NumRows()) > MaxBatchRows {
			return nil, fmt.Errorf("ingest: batch holds more than %d rows", MaxBatchRows)
		}
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
		// Optional, like the series columns: absent from an older agent's batch.
		var sendErrs *array.Uint8
		if c := col("send_error"); c != nil {
			var ok bool
			if sendErrs, ok = c.(*array.Uint8); !ok {
				return nil, fmt.Errorf("ingest: column \"send_error\" is not uint8")
			}
		}
		// IsNull indexes the validity bitmap without checking its length, and
		// the reader does not check it either, so a bitmap shorter than its
		// column is an out-of-range panic on the first row.
		if err := checkValidity("icmp_error", icmp); err != nil {
			return nil, err
		}
		if sendErrs != nil {
			if err := checkValidity("send_error", sendErrs); err != nil {
				return nil, err
			}
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
			if err := checkValidity(name, l); err != nil {
				return nil, err
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
			lo, hi, err := listBounds(offsets, i, values.Len(), "samples")
			if err != nil {
				return nil, err
			}
			for j := lo; j < hi; j++ {
				m.Samples = append(m.Samples, values.Value(int(j)))
			}
			// The store's blob codec requires ascending order, and an agent
			// that sends otherwise would fail the whole write — taking every
			// other measurement in the batch with it, and, because a rejected
			// batch stays buffered and is retried unchanged, every measurement
			// that agent ever takes afterwards. These are distributions, so
			// order carries no meaning and sorting is lossless: normalise here
			// rather than turn a remote bug into a permanent outage.
			if !slices.IsSorted(m.Samples) {
				slices.Sort(m.Samples)
				resorted++
			}
			// The store's invariant, checked here so a malformed batch is
			// refused at the door rather than at the write.
			if m.Received != len(m.Samples) {
				return nil, fmt.Errorf("ingest: target %d at %d claims %d replies but carries %d samples",
					m.TargetID, m.TS, m.Received, len(m.Samples))
			}
			// A reply needs a probe: more received than sent is not a measurement
			// of anything, and reads as negative loss. The row is dropped and the
			// rest kept. Refusing the batch is a 400, a 400 makes the agent
			// discard the whole batch, and one impossible row would then cost up
			// to a batch of honest ones; a gap is the truthful record here.
			if m.Received > m.Sent {
				impossible++
				continue
			}
			if !icmp.IsNull(i) {
				v := icmp.Value(i)
				m.ICMPErr = &v
			}
			if sendErrs != nil && !sendErrs.IsNull(i) {
				v := sendErrs.Value(i)
				m.SendErr = &v
			}
			for _, name := range seriesColumns {
				l := series[name]
				if l == nil || l.IsNull(i) {
					continue // not measured; absence is the record of that
				}
				vals := l.ListValues().(*array.Int32)
				// Offsets come straight off the wire and the IPC reader never
				// validates them. They were used as a make() capacity, so a
				// 408-byte body with the offset set to MaxInt32 reserved 8 GiB
				// before the read that would have failed ever ran.
				lo, hi, err := listBounds(l.Offsets(), i, vals.Len(), name)
				if err != nil {
					return nil, err
				}
				// A series holds at most one value per probe sent: inter-packet
				// delay variation needs a pair, and server processing time one
				// reply. Anything longer did not come from a measurement, and
				// leaving it unbounded let one row carry millions of values
				// into a blob the store keeps forever.
				if n := int(hi - lo); n > m.Sent {
					return nil, fmt.Errorf("ingest: target %d at %d: series %q has %d values "+
						"but only %d probes were sent", m.TargetID, m.TS, name, n, m.Sent)
				}
				got := make([]int32, 0, hi-lo)
				for j := lo; j < hi; j++ {
					got = append(got, vals.Value(int(j)))
				}
				if !slices.IsSorted(got) {
					slices.Sort(got)
					resorted++
				}
				if m.Series == nil {
					m.Series = make(map[string][]int32, len(seriesColumns))
				}
				m.Series[name] = got
			}
			out = append(out, m)
		}
	}
	if resorted > 0 {
		// Once per batch, not once per row: a broken agent produces this for
		// every measurement it sends, and the useful signal is that it happens
		// at all.
		log.Printf("ingest: agent %d sent %d unsorted distribution(s); they were sorted on the way in, "+
			"which changes nothing about the measurement, but the agent is not producing what it should",
			agentID, resorted)
	}
	if impossible > 0 {
		log.Printf("ingest: agent %d sent %d measurement(s) with more replies than probes; "+
			"those were dropped and the rest of the batch accepted", agentID, impossible)
	}
	return out, reader.Err()
}

// listBounds returns the half-open value range of row i of a list column,
// refusing anything the child array cannot back. Arrow's IPC reader performs no
// validation of its own, so these are the only checks between a hostile header
// and a slice index.
func listBounds(offsets []int32, i, childLen int, name string) (lo, hi int32, err error) {
	if i+1 >= len(offsets) {
		return 0, 0, fmt.Errorf("ingest: column %q has no offset for row %d", name, i)
	}
	lo, hi = offsets[i], offsets[i+1]
	if lo < 0 || hi < lo || int(hi) > childLen {
		return 0, 0, fmt.Errorf("ingest: column %q row %d has offsets [%d,%d) outside its %d values",
			name, i, lo, hi, childLen)
	}
	return lo, hi, nil
}

// checkValidity refuses an array whose validity bitmap is too short for its
// length. No bitmap at all is fine: that is how Arrow says nothing is null.
func checkValidity(name string, a arrow.Array) error {
	bm := a.NullBitmapBytes()
	if len(bm) == 0 {
		return nil
	}
	if need := (a.Data().Offset() + a.Len() + 7) / 8; len(bm) < need {
		return fmt.Errorf("ingest: column %q has a validity bitmap of %d bytes for %d rows",
			name, len(bm), a.Len())
	}
	return nil
}
