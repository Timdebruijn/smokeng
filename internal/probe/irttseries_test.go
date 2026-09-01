package probe

import (
	"testing"
	"time"

	"github.com/heistp/irtt"
	"github.com/timdebruijn/smokeng/internal/store"
)

// rt builds one answered round trip with the server timestamps that make the
// directional series computable.
func rt(clientSend, serverRecv, serverSend, clientRecv time.Duration) irtt.RoundTrip {
	return irtt.RoundTrip{
		RoundTripData: &irtt.RoundTripData{
			Client: irtt.Timestamp{
				Send:    irtt.Time{Wall: int64(clientSend), Mono: clientSend},
				Receive: irtt.Time{Wall: int64(clientRecv), Mono: clientRecv},
			},
			Server: irtt.Timestamp{
				Receive: irtt.Time{Wall: int64(serverRecv), Mono: serverRecv},
				Send:    irtt.Time{Wall: int64(serverSend), Mono: serverSend},
			},
		},
	}
}

// The directional series come out of the session smokeng already runs, and a
// negative value has to survive: inter-packet delay variation is negative
// exactly when a packet arrived sooner than the one before it, and discarding
// the sign would hide whether a link jitters symmetrically or only bursts late.
func TestIRTTSeriesRecorded(t *testing.T) {
	ms := time.Millisecond
	r := &irtt.Result{RoundTrips: []irtt.RoundTrip{
		rt(0, 10*ms, 11*ms, 20*ms),
		rt(100*ms, 108*ms, 109*ms, 121*ms), // arrived 2ms sooner: send IPDV -2ms
		rt(200*ms, 214*ms, 215*ms, 223*ms), // and 6ms later than that
	}}
	// IPDV is set by irtt when it builds a Result; construct it here the same
	// way, since the struct literal above bypasses that.
	for i := 1; i < len(r.RoundTrips); i++ {
		r.RoundTrips[i].SendIPDV = r.RoundTrips[i].SendIPDVSince(r.RoundTrips[i-1].RoundTripData)
		r.RoundTrips[i].ReceiveIPDV = r.RoundTrips[i].ReceiveIPDVSince(r.RoundTrips[i-1].RoundTripData)
	}
	r.RoundTrips[0].SendIPDV = irtt.InvalidDuration
	r.RoundTrips[0].ReceiveIPDV = irtt.InvalidDuration

	col := &collector{}
	recordIRTTSeries(col, r)

	send := col.series[store.SeriesIPDVSend]
	if len(send) != 2 {
		t.Fatalf("ipdv_send = %v, want 2 values (the first packet has no predecessor)", send)
	}
	if send[0] >= 0 {
		t.Errorf("ipdv_send = %v, want a negative value preserved", send)
	}
	if send[0] > send[1] {
		t.Errorf("ipdv_send not sorted ascending: %v", send)
	}
	if len(col.series[store.SeriesIPDVReceive]) != 2 {
		t.Errorf("ipdv_receive = %v", col.series[store.SeriesIPDVReceive])
	}
	// Server processing time needs both server stamps, which these have.
	if got := col.series[store.SeriesServerProcessing]; len(got) != 3 {
		t.Errorf("server_processing = %v, want 3 values", got)
	}
}

// A peer that returns no timestamps yields no directional series at all. It
// must not yield an empty one: "this server does not report it" and "there was
// no variation" are different facts and only absence can carry the first.
func TestIRTTSeriesAbsentWithoutServerTimestamps(t *testing.T) {
	ms := time.Millisecond
	var trips []irtt.RoundTrip
	for i := range 3 {
		d := time.Duration(i) * 100 * ms
		trips = append(trips, irtt.RoundTrip{RoundTripData: &irtt.RoundTripData{
			Client: irtt.Timestamp{
				Send:    irtt.Time{Wall: int64(d), Mono: d},
				Receive: irtt.Time{Wall: int64(d + 20*ms), Mono: d + 20*ms},
			},
			// Server left zero: no timestamps came back.
		}})
	}
	r := &irtt.Result{RoundTrips: trips}
	for i := 1; i < len(r.RoundTrips); i++ {
		r.RoundTrips[i].SendIPDV = r.RoundTrips[i].SendIPDVSince(r.RoundTrips[i-1].RoundTripData)
		r.RoundTrips[i].ReceiveIPDV = r.RoundTrips[i].ReceiveIPDVSince(r.RoundTrips[i-1].RoundTripData)
	}
	r.RoundTrips[0].SendIPDV = irtt.InvalidDuration
	r.RoundTrips[0].ReceiveIPDV = irtt.InvalidDuration

	col := &collector{}
	recordIRTTSeries(col, r)
	if len(col.series) != 0 {
		t.Errorf("series recorded from a peer that sent no timestamps: %v", col.series)
	}
}

// irtt reports "not measured" with a sentinel duration rather than an error.
// Storing it would put a number near the edge of int64 into a jitter
// distribution, where it would render as a spike nobody could explain.
func TestIRTTSentinelNeverStored(t *testing.T) {
	if _, ok := irttMicros(irtt.InvalidDuration); ok {
		t.Error("the invalid-duration sentinel was accepted as a measurement")
	}
	if v, ok := irttMicros(1500 * time.Microsecond); !ok || v != 1500 {
		t.Errorf("irttMicros(1500µs) = %d, %v", v, ok)
	}
	if v, ok := irttMicros(-1500 * time.Microsecond); !ok || v != -1500 {
		t.Errorf("irttMicros(-1500µs) = %d, %v", v, ok)
	}
	if _, ok := irttMicros(time.Duration(1<<62) * time.Nanosecond); ok {
		t.Error("a duration too large for int32 microseconds was accepted")
	}
}

// End to end against a real irtt server: the directional series have to come
// out of an actual session, not only out of a hand-built Result. This is the
// test that would have caught the original omission, where every unit test
// passed and the measurement simply arrived without half of what irtt sent.
func TestIRTTSeriesEndToEnd(t *testing.T) {
	m := irttOnce(t, irttServer(t))
	if m.Received == 0 {
		t.Fatalf("got %d/%d from the local irtt server", m.Received, m.Sent)
	}
	for _, name := range []string{store.SeriesIPDVSend, store.SeriesIPDVReceive} {
		vals, ok := m.Series[name]
		if !ok {
			t.Errorf("%s missing; a local irtt server does return timestamps", name)
			continue
		}
		// One fewer than the replies: the first packet has no predecessor.
		if want := m.Received - 1; len(vals) != want {
			t.Errorf("%s has %d values, want %d", name, len(vals), want)
		}
		for i := 1; i < len(vals); i++ {
			if vals[i] < vals[i-1] {
				t.Errorf("%s not sorted ascending: %v", name, vals)
				break
			}
		}
	}
}
