package api

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/ipc"
	"github.com/apache/arrow-go/v18/arrow/memory"

	"github.com/timdebruijn/smokeng/internal/losspattern"
	"github.com/timdebruijn/smokeng/internal/store"
	"github.com/timdebruijn/smokeng/internal/tree"
)

const batchRows = 8192

// measurementsSchema is the wire schema (DESIGN.md §7.2). The varint blob
// encoding never crosses the API boundary: samples are a real List<UInt32>
// of RTTs in microseconds, sorted ascending.
var measurementsSchema = arrow.NewSchema([]arrow.Field{
	{Name: "ts", Type: &arrow.TimestampType{Unit: arrow.Second, TimeZone: "UTC"}},
	{Name: "sent", Type: arrow.PrimitiveTypes.Uint16},
	{Name: "received", Type: arrow.PrimitiveTypes.Uint16},
	{Name: "flags", Type: arrow.PrimitiveTypes.Uint8},
	{Name: "samples", Type: arrow.ListOf(arrow.PrimitiveTypes.Uint32)},
	// ICMP type<<8|code for the error that explains this interval's failures,
	// null when nothing was refused.
	{Name: "icmp_error", Type: arrow.PrimitiveTypes.Uint16, Nullable: true},
	// Why the local side could not send this interval, null when it did. The
	// quality flags say that a send failed; this says what failed, which is the
	// difference between a far end refusing the traffic and this prober never
	// getting it out.
	{Name: "send_error", Type: arrow.PrimitiveTypes.Uint8, Nullable: true},
	// Set when the master found this series to lose its probes at fixed places
	// in the send order (internal/losspattern), null otherwise. loss_positions
	// are the send positions, from 1, that lost clearly more than their share:
	// empty for a lopsided profile no single position explains, which is not the
	// same as null. loss_window is how many intervals the finding rests on.
	{Name: "loss_positions", Type: arrow.ListOf(arrow.PrimitiveTypes.Uint16), Nullable: true},
	{Name: "loss_window", Type: arrow.PrimitiveTypes.Uint8, Nullable: true},
	// The extra per-packet series, one nullable column each, in the same units
	// and the same sorted order as samples. Null is not zero: it means this
	// interval has no such measurement — the probe does not produce one, or the
	// peer returned no timestamps to compute it from — and the graph says so
	// rather than drawing a flat line.
	{Name: store.SeriesIPDVSend, Type: arrow.ListOf(arrow.PrimitiveTypes.Int32), Nullable: true},
	{Name: store.SeriesIPDVReceive, Type: arrow.ListOf(arrow.PrimitiveTypes.Int32), Nullable: true},
	{Name: store.SeriesServerProcessing, Type: arrow.ListOf(arrow.PrimitiveTypes.Int32), Nullable: true},
	// Why the far end gave this interval's failed probes no successful answer
	// (store.ProbeError: a refused connection, an HTTP status, a DNS rcode),
	// null when none failed that way. Last, so no field moved for it.
	{Name: "probe_error", Type: arrow.PrimitiveTypes.Uint16, Nullable: true},
}, nil)

// pooledSchema is the wire schema of a pooled read: one row per pixel column
// that holds any interval (see store.Column). A reader tells it from
// measurementsSchema by its fields; "rounds" is only here.
var pooledSchema = arrow.NewSchema([]arrow.Field{
	{Name: "ts", Type: &arrow.TimestampType{Unit: arrow.Second, TimeZone: "UTC"}},
	{Name: "ts_end", Type: &arrow.TimestampType{Unit: arrow.Second, TimeZone: "UTC"}},
	{Name: "rounds", Type: arrow.PrimitiveTypes.Uint32},
	// Sums over the column. 64 bits, because a wide column of a dense series
	// sums more probes than 32 hold.
	{Name: "sent", Type: arrow.PrimitiveTypes.Uint64},
	{Name: "received", Type: arrow.PrimitiveTypes.Uint64},
	{Name: "flags", Type: arrow.PrimitiveTypes.Uint8},
	{Name: "flagged", Type: arrow.PrimitiveTypes.Uint32},
	// How many of the column's intervals lost their probes at fixed send
	// positions (the loss_positions column of a raw read says which). It is not
	// a quality flag, so these are counted here and not in flagged.
	{Name: "positional", Type: arrow.PrimitiveTypes.Uint32},
	{Name: "quantiles", Type: arrow.ListOf(arrow.PrimitiveTypes.Uint32)},
	// The extra series pooled the same way, null where no interval in the
	// column measured them.
	{Name: store.SeriesIPDVSend, Type: arrow.ListOf(arrow.PrimitiveTypes.Int32), Nullable: true},
	{Name: store.SeriesIPDVReceive, Type: arrow.ListOf(arrow.PrimitiveTypes.Int32), Nullable: true},
	{Name: store.SeriesServerProcessing, Type: arrow.ListOf(arrow.PrimitiveTypes.Int32), Nullable: true},
	// The probe error most of the column's intervals that had one carried,
	// null when none had one (store.Column.ProbeErr).
	{Name: "probe_error", Type: arrow.PrimitiveTypes.Uint16, Nullable: true},
	// The ICMP error and send reason likewise: each column's commonest, in the
	// encoding of the raw read's columns of the same names. After probe_error,
	// so no field moved for them.
	{Name: "icmp_error", Type: arrow.PrimitiveTypes.Uint16, Nullable: true},
	{Name: "send_error", Type: arrow.PrimitiveTypes.Uint8, Nullable: true},
}, nil)

// handleMeasurements streams one series over [from, to) as Arrow IPC.
// Defaults: agent_id 0 (local), to now, from one hour before to.
const (
	// minIntervalS is the shortest interval a target can be configured with,
	// and so the densest a series can be. Used only to turn a time range into
	// a worst-case row count.
	minIntervalS = 1
	// maxRowsPerRequest is generous for any window a plot asks for — a day of
	// one-second data is 86 400 — and far below what exhausts memory.
	maxRowsPerRequest = 200_000
	// maxColumns bounds columns=: wider than any screen a plot is drawn on,
	// at device pixels.
	maxColumns = 8192
)

func (s *server) handleMeasurements(w http.ResponseWriter, r *http.Request) {
	targetID, agentID, family, from, to, ok := s.seriesQuery(w, r)
	if !ok {
		return
	}
	if v := r.URL.Query().Get("columns"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxColumns {
			badRequestMsg(w, fmt.Sprintf("columns must be between 1 and %d", maxColumns))
			return
		}
		s.handlePooled(w, r, targetID, agentID, family, from, to, n)
		return
	}
	// QueryRange materialises every row, samples decoded, before a byte is
	// written. The range comes from the caller, so without a bound one request
	// for a year of a 10-second series is hundreds of megabytes and an OOM
	// kill takes the prober with it. Refuse the range rather than die trying.
	if rows := rangeRows(from, to); rows > maxRowsPerRequest {
		badRequestMsg(w, fmt.Sprintf(
			"that range could hold about %d intervals, more than the %d one request returns; ask for a shorter window",
			rows, maxRowsPerRequest))
		return
	}
	ms, err := s.st.QueryRange(r.Context(), targetID, agentID, family, from, to)
	if err != nil {
		internalError(w, err)
		return
	}

	w.Header().Set("Content-Type", "application/vnd.apache.arrow.stream")
	// A trailing window's contents change every interval; never cache it.
	w.Header().Set("Cache-Control", "no-store")
	writeMeasurementsArrow(w, ms, batchRows)
}

// handlePooled answers a read for a plot n columns wide. The window-width guard
// above does not apply: what it returns is bounded by n, what it holds in memory
// by one column's samples, and the time it takes by the intervals the store
// will walk (see store.QueryColumns), all three counted rather than guessed.
func (s *server) handlePooled(w http.ResponseWriter, r *http.Request, targetID, agentID int64, family tree.Family, from, to int64, n int) {
	raw, cols, err := s.st.QueryColumns(r.Context(), targetID, agentID, family, from, to, n)
	if err != nil {
		internalError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/vnd.apache.arrow.stream")
	w.Header().Set("Cache-Control", "no-store")
	if cols == nil {
		writeMeasurementsArrow(w, raw, batchRows)
		return
	}
	writePooledArrow(w, cols, batchRows)
}

// writePooledArrow writes columns as an Arrow IPC stream, as
// writeMeasurementsArrow writes intervals.
func writePooledArrow(w io.Writer, cols []store.Column, batchRows int) {
	writer := ipc.NewWriter(w, ipc.WithSchema(pooledSchema))
	defer writer.Close()

	rb := array.NewRecordBuilder(memory.DefaultAllocator, pooledSchema)
	defer rb.Release()
	tsB := rb.Field(0).(*array.TimestampBuilder)
	endB := rb.Field(1).(*array.TimestampBuilder)
	roundsB := rb.Field(2).(*array.Uint32Builder)
	sentB := rb.Field(3).(*array.Uint64Builder)
	recvB := rb.Field(4).(*array.Uint64Builder)
	flagsB := rb.Field(5).(*array.Uint8Builder)
	flaggedB := rb.Field(6).(*array.Uint32Builder)
	positionalB := rb.Field(7).(*array.Uint32Builder)
	listB := rb.Field(8).(*array.ListBuilder)
	valB := listB.ValueBuilder().(*array.Uint32Builder)
	seriesB := make([]*array.ListBuilder, len(store.KnownSeries))
	seriesV := make([]*array.Int32Builder, len(store.KnownSeries))
	for i := range store.KnownSeries {
		seriesB[i] = rb.Field(9 + i).(*array.ListBuilder)
		seriesV[i] = seriesB[i].ValueBuilder().(*array.Int32Builder)
	}
	probeErrB := rb.Field(9 + len(store.KnownSeries)).(*array.Uint16Builder)
	icmpB := rb.Field(10 + len(store.KnownSeries)).(*array.Uint16Builder)
	sendErrB := rb.Field(11 + len(store.KnownSeries)).(*array.Uint8Builder)

	flush := func() bool {
		rec := rb.NewRecord()
		defer rec.Release()
		return writer.Write(rec) == nil
	}
	for i, c := range cols {
		tsB.Append(arrow.Timestamp(c.From))
		endB.Append(arrow.Timestamp(c.To))
		roundsB.Append(uint32(c.Rounds))
		sentB.Append(uint64(c.Sent))
		recvB.Append(uint64(c.Received))
		flagsB.Append(c.Flags)
		flaggedB.Append(uint32(c.Flagged))
		positionalB.Append(uint32(c.Positional))
		listB.Append(true)
		valB.AppendValues(c.Quantiles, nil)
		for j, name := range store.KnownSeries {
			vals, ok := c.Series[name]
			if !ok {
				seriesB[j].AppendNull()
				continue
			}
			seriesB[j].Append(true)
			seriesV[j].AppendValues(vals, nil)
		}
		if c.ProbeErr != nil {
			probeErrB.Append(*c.ProbeErr)
		} else {
			probeErrB.AppendNull()
		}
		if c.ICMPErr != nil {
			icmpB.Append(*c.ICMPErr)
		} else {
			icmpB.AppendNull()
		}
		if c.SendErr != nil {
			sendErrB.Append(*c.SendErr)
		} else {
			sendErrB.AppendNull()
		}
		if (i+1)%batchRows == 0 && !flush() {
			return
		}
	}
	if rb.Field(0).Len() > 0 {
		flush()
	}
}

// writeMeasurementsArrow writes measurements as an Arrow IPC stream: the schema,
// batches of at most batchRows, and the end-of-stream marker the reader requires
// (a stream cut short has none, and is refused rather than shown as a shorter
// series). It is a function of its own so a test can write exactly what the
// handler writes, for the frontend's decoder to read.
func writeMeasurementsArrow(w io.Writer, ms []store.Measurement, batchRows int) {
	writer := ipc.NewWriter(w, ipc.WithSchema(measurementsSchema))
	defer writer.Close()

	rb := array.NewRecordBuilder(memory.DefaultAllocator, measurementsSchema)
	defer rb.Release()
	tsB := rb.Field(0).(*array.TimestampBuilder)
	sentB := rb.Field(1).(*array.Uint16Builder)
	recvB := rb.Field(2).(*array.Uint16Builder)
	flagsB := rb.Field(3).(*array.Uint8Builder)
	listB := rb.Field(4).(*array.ListBuilder)
	valB := listB.ValueBuilder().(*array.Uint32Builder)
	icmpB := rb.Field(5).(*array.Uint16Builder)
	sendErrB := rb.Field(6).(*array.Uint8Builder)
	posB := rb.Field(7).(*array.ListBuilder)
	posV := posB.ValueBuilder().(*array.Uint16Builder)
	windowB := rb.Field(8).(*array.Uint8Builder)
	seriesB := make([]*array.ListBuilder, len(store.KnownSeries))
	seriesV := make([]*array.Int32Builder, len(store.KnownSeries))
	for i := range store.KnownSeries {
		seriesB[i] = rb.Field(9 + i).(*array.ListBuilder)
		seriesV[i] = seriesB[i].ValueBuilder().(*array.Int32Builder)
	}
	probeErrB := rb.Field(9 + len(store.KnownSeries)).(*array.Uint16Builder)

	flush := func() bool {
		rec := rb.NewRecord()
		defer rec.Release()
		return writer.Write(rec) == nil
	}
	for i, m := range ms {
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
		if p := m.Positional; p != nil {
			posB.Append(true)
			for _, at := range losspattern.Positions(p.Counts, p.Lost) {
				posV.Append(uint16(at))
			}
			windowB.Append(uint8(p.Intervals))
		} else {
			posB.AppendNull()
			windowB.AppendNull()
		}
		for j, name := range store.KnownSeries {
			vals, ok := m.Series[name]
			if !ok {
				seriesB[j].AppendNull()
				continue
			}
			seriesB[j].Append(true)
			seriesV[j].AppendValues(vals, nil)
		}
		if m.ProbeErr != nil {
			probeErrB.Append(*m.ProbeErr)
		} else {
			probeErrB.AppendNull()
		}
		if (i+1)%batchRows == 0 && !flush() {
			return // client went away mid-stream
		}
	}
	if rb.Field(0).Len() > 0 || len(ms) == 0 {
		flush() // remainder, or an empty batch so the schema still arrives
	}
}

// timeRange parses the window shared by every series endpoint, defaulting to
// the trailing hour so the two cannot drift apart.
func timeRange(fromRaw, toRaw string) (from, to int64, err error) {
	to = time.Now().Unix()
	if toRaw != "" {
		if to, err = strconv.ParseInt(toRaw, 10, 64); err != nil {
			return 0, 0, errors.New("bad to")
		}
	}
	from = to - 3600
	if fromRaw != "" {
		if from, err = strconv.ParseInt(fromRaw, 10, 64); err != nil {
			return 0, 0, errors.New("bad from")
		}
	}
	return from, to, nil
}
