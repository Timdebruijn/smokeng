package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/ipc"
	"github.com/apache/arrow-go/v18/arrow/memory"

	"smokeng/internal/store"
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
}, nil)

// handleMeasurements streams one series over [from, to) as Arrow IPC.
// Defaults: agent_id 0 (local), to now, from one hour before to.
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
	to := time.Now().Unix()
	if v := q.Get("to"); v != "" {
		if to, err = strconv.ParseInt(v, 10, 64); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad to"})
			return
		}
	}
	from := to - 3600
	if v := q.Get("from"); v != "" {
		if from, err = strconv.ParseInt(v, 10, 64); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad from"})
			return
		}
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
		if (i+1)%batchRows == 0 && !flush() {
			return // client went away mid-stream
		}
	}
	if rb.Field(0).Len() > 0 || len(ms) == 0 {
		flush() // remainder, or an empty batch so the schema still arrives
	}
}
