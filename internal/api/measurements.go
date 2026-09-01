package api

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/ipc"
	"github.com/apache/arrow-go/v18/arrow/memory"

	"github.com/timdebruijn/smokeng/internal/store"
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
	// The extra per-packet series, one nullable column each, in the same units
	// and the same sorted order as samples. Null is not zero: it means this
	// interval has no such measurement — the probe does not produce one, or the
	// peer returned no timestamps to compute it from — and the graph says so
	// rather than drawing a flat line.
	{Name: store.SeriesIPDVSend, Type: arrow.ListOf(arrow.PrimitiveTypes.Int32), Nullable: true},
	{Name: store.SeriesIPDVReceive, Type: arrow.ListOf(arrow.PrimitiveTypes.Int32), Nullable: true},
	{Name: store.SeriesServerProcessing, Type: arrow.ListOf(arrow.PrimitiveTypes.Int32), Nullable: true},
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
)

func (s *server) handleMeasurements(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	targetID, err := strconv.ParseInt(q.Get("target_id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "target_id is required"})
		return
	}
	agentID := store.LocalAgentID
	if v := q.Get("agent_id"); v != "" {
		if agentID, err = strconv.ParseInt(v, 10, 64); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad agent_id"})
			return
		}
	}
	from, to, err := timeRange(q.Get("from"), q.Get("to"))
	if err != nil {
		badRequestMsg(w, err.Error())
		return
	}

	if !s.requireVisible(w, r, targetID) {
		return
	}
	// QueryRange materialises every row, samples decoded, before a byte is
	// written. The range comes from the caller, so without a bound one request
	// for a year of a 10-second series is hundreds of megabytes and an OOM
	// kill takes the prober with it. Refuse the range rather than die trying.
	if rows := (to - from) / int64(minIntervalS); rows > maxRowsPerRequest {
		badRequestMsg(w, fmt.Sprintf(
			"that range could hold about %d intervals, more than the %d one request returns; ask for a shorter window",
			rows, maxRowsPerRequest))
		return
	}
	ms, err := s.st.QueryRange(r.Context(), targetID, agentID, from, to)
	if err != nil {
		internalError(w, err)
		return
	}

	w.Header().Set("Content-Type", "application/vnd.apache.arrow.stream")
	// A trailing window's contents change every interval; never cache it.
	w.Header().Set("Cache-Control", "no-store")
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
	seriesB := make([]*array.ListBuilder, len(store.KnownSeries))
	seriesV := make([]*array.Int32Builder, len(store.KnownSeries))
	for i := range store.KnownSeries {
		seriesB[i] = rb.Field(6 + i).(*array.ListBuilder)
		seriesV[i] = seriesB[i].ValueBuilder().(*array.Int32Builder)
	}

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
		for j, name := range store.KnownSeries {
			vals, ok := m.Series[name]
			if !ok {
				seriesB[j].AppendNull()
				continue
			}
			seriesB[j].Append(true)
			seriesV[j].AppendValues(vals, nil)
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
