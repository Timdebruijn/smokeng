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
	r := &irtt.Result{Config: dualStamped(), RoundTrips: []irtt.RoundTrip{
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
	recordIRTTSeries(col, r, 20)

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
	// A server that stamps nothing negotiates AtNone; that, not an
	// inconsistent round trip, is how this reaches the prober.
	r := &irtt.Result{
		Config:     &irtt.ClientConfig{StampAt: irtt.AtNone, Clock: irtt.BothClocks},
		RoundTrips: trips,
	}
	for i := 1; i < len(r.RoundTrips); i++ {
		r.RoundTrips[i].SendIPDV = r.RoundTrips[i].SendIPDVSince(r.RoundTrips[i-1].RoundTripData)
		r.RoundTrips[i].ReceiveIPDV = r.RoundTrips[i].ReceiveIPDVSince(r.RoundTrips[i-1].RoundTripData)
	}
	r.RoundTrips[0].SendIPDV = irtt.InvalidDuration
	r.RoundTrips[0].ReceiveIPDV = irtt.InvalidDuration

	col := &collector{}
	recordIRTTSeries(col, r, 20)
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

// dualStamped is what a default irtt server negotiates: both clocks, a stamp
// at both ends. Every series is supportable.
func dualStamped() *irtt.ClientConfig {
	return &irtt.ClientConfig{StampAt: irtt.AtBoth, Clock: irtt.BothClocks}
}

// The server may restrict what it stamps, and irtt reports that with an event
// rather than an error — the session succeeds and the numbers keep arriving,
// meaning something else. Each restriction must silence exactly the series it
// invalidates and no more.
func TestSeriesTrustFollowsNegotiatedStamps(t *testing.T) {
	cases := []struct {
		name                  string
		cfg                   *irtt.ClientConfig
		send, receive, server bool
	}{
		{"both ends, both clocks", dualStamped(), true, true, true},
		// One documented server flag (--tstamp=single) restricts AtBoth to
		// AtMidpoint, where the two server stamps hold the same value. Nothing
		// survives that: server processing time becomes a computable zero, and
		// both IPDV figures become differences against the midpoint, so each
		// carries half the server's hold time. See
		// TestMidpointLeaksServerProcessingIntoJitter.
		{"midpoint", &irtt.ClientConfig{StampAt: irtt.AtMidpoint, Clock: irtt.BothClocks}, false, false, false},
		// A one-sided stamp is not reachable from an AtBoth request — a server
		// may only downgrade to AtNone, AtMidpoint, or leave it alone — but the
		// gate is written in terms of what each figure needs rather than in
		// terms of what this client happens to ask for.
		{"send only", &irtt.ClientConfig{StampAt: irtt.AtSend, Clock: irtt.BothClocks}, false, false, false},
		{"receive only", &irtt.ClientConfig{StampAt: irtt.AtReceive, Clock: irtt.BothClocks}, false, false, false},
		{"no stamps", &irtt.ClientConfig{StampAt: irtt.AtNone, Clock: irtt.BothClocks}, false, false, false},
		// Without the monotonic clock, IPDV is a difference of wall-clock
		// readings: a constant offset still cancels, an NTP step does not.
		{"wall clock only", &irtt.ClientConfig{StampAt: irtt.AtBoth, Clock: irtt.Wall}, false, false, false},
		{"no config", nil, false, false, false},
	}
	for _, c := range cases {
		got := trustFrom(c.cfg)
		if got.sendIPDV != c.send || got.receiveIPDV != c.receive || got.serverProcessing != c.server {
			t.Errorf("%s: trust = %+v, want send=%v receive=%v server=%v",
				c.name, got, c.send, c.receive, c.server)
		}
	}
}

// The critical case, end to end through the recorder: a midpoint-stamping
// server must produce no server_processing series at all, rather than a
// distribution of zeros that claims the far end replied instantly.
func TestMidpointServerRecordsNoProcessingTime(t *testing.T) {
	ms := time.Millisecond
	var trips []irtt.RoundTrip
	for i := range 3 {
		d := time.Duration(i) * 100 * ms
		mid := d + 10*ms
		trips = append(trips, rt(d, mid, mid, d+20*ms)) // send == receive: the midpoint
	}
	r := &irtt.Result{
		Config:     &irtt.ClientConfig{StampAt: irtt.AtMidpoint, Clock: irtt.BothClocks},
		RoundTrips: trips,
	}
	for i := 1; i < len(r.RoundTrips); i++ {
		r.RoundTrips[i].SendIPDV = r.RoundTrips[i].SendIPDVSince(r.RoundTrips[i-1].RoundTripData)
		r.RoundTrips[i].ReceiveIPDV = r.RoundTrips[i].ReceiveIPDVSince(r.RoundTrips[i-1].RoundTripData)
	}
	r.RoundTrips[0].SendIPDV = irtt.InvalidDuration
	r.RoundTrips[0].ReceiveIPDV = irtt.InvalidDuration

	// Confirm the library really does produce a computable zero here, so the
	// test is guarding against the real hazard and not a hypothetical one.
	if d := trips[0].ServerProcessingTime(); d != 0 {
		t.Fatalf("premise wrong: midpoint ServerProcessingTime = %v, want 0", d)
	}

	col := &collector{}
	recordIRTTSeries(col, r, 20)
	if len(col.series) != 0 {
		t.Errorf("a midpoint server produced %v; none of it means what its heading says", col.series)
	}
}

// Why the midpoint case silences the jitter figures too, measured rather than
// argued. Zero network jitter, and a server whose hold time steps from 10ms to
// 20ms between two packets: at a midpoint stamp half that step lands in each
// direction, so a loaded server graphs its own scheduling as network jitter.
// With both stamps the same two packets correctly report no variation at all.
func TestMidpointLeaksServerProcessingIntoJitter(t *testing.T) {
	ms := time.Millisecond
	// Midpoint = the server's receive time plus half its hold.
	mid1 := 5*ms + 5*ms
	mid2 := 100*ms + 5*ms + 10*ms
	a := rt(0, mid1, mid1, 20*ms)
	b := rt(100*ms, mid2, mid2, 130*ms)
	sendMid := b.SendIPDVSince(a.RoundTripData)
	recvMid := b.ReceiveIPDVSince(a.RoundTripData)

	// The same two packets, stamped at both ends.
	a2 := rt(0, 5*ms, 15*ms, 20*ms)
	b2 := rt(100*ms, 105*ms, 125*ms, 130*ms)
	sendBoth := b2.SendIPDVSince(a2.RoundTripData)
	recvBoth := b2.ReceiveIPDVSince(a2.RoundTripData)

	if sendBoth != 0 || recvBoth != 0 {
		t.Fatalf("premise wrong: with both stamps and no network jitter, IPDV = %v/%v, want 0",
			sendBoth, recvBoth)
	}
	if sendMid == 0 && recvMid == 0 {
		t.Fatal("premise wrong: the midpoint stamp reported no contamination, " +
			"so this test no longer demonstrates why the midpoint case is silenced")
	}
	t.Logf("midpoint reports send=%v receive=%v where the truth is 0/0", sendMid, recvMid)
}

// An interval that could measure the series but had nothing to compare — one
// reply, so no consecutive pair — is a different fact from one the peer could
// not stamp, and only one of the two is an instrumentation problem.
func TestMeasuredButEmptyIsRecorded(t *testing.T) {
	ms := time.Millisecond
	r := &irtt.Result{
		Config:     dualStamped(),
		RoundTrips: []irtt.RoundTrip{rt(0, 10*ms, 11*ms, 20*ms)},
	}
	r.RoundTrips[0].SendIPDV = irtt.InvalidDuration
	r.RoundTrips[0].ReceiveIPDV = irtt.InvalidDuration

	col := &collector{}
	recordIRTTSeries(col, r, 20)
	v, ok := col.series[store.SeriesIPDVSend]
	if !ok {
		t.Fatal("a measured series with no computable values was dropped; it reads as 'not measured'")
	}
	if len(v) != 0 {
		t.Errorf("ipdv_send = %v, want empty", v)
	}
}

// A negative round trip is dropped from the latency distribution as an irtt
// anomaly. Its derived figures are no more trustworthy, so they must not
// reappear on the jitter graph instead.
func TestNegativeRoundTripExcludedFromSeries(t *testing.T) {
	ms := time.Millisecond
	// client.rx before server.send: the round trip comes out negative.
	// RTT is client.rx - client.tx minus the server's processing time, so a
	// hold longer than the wire time is what drives it negative.
	bad := rt(0, 10*ms, 35*ms, 20*ms)
	good := rt(100*ms, 108*ms, 109*ms, 121*ms)
	r := &irtt.Result{Config: dualStamped(), RoundTrips: []irtt.RoundTrip{good, bad}}
	if r.RoundTrips[1].RTT() >= 0 {
		t.Fatalf("premise wrong: RTT = %v, want negative", r.RoundTrips[1].RTT())
	}
	r.RoundTrips[1].SendIPDV = r.RoundTrips[1].SendIPDVSince(r.RoundTrips[0].RoundTripData)
	r.RoundTrips[1].ReceiveIPDV = r.RoundTrips[1].ReceiveIPDVSince(r.RoundTrips[0].RoundTripData)
	r.RoundTrips[0].SendIPDV = irtt.InvalidDuration
	r.RoundTrips[0].ReceiveIPDV = irtt.InvalidDuration

	col := &collector{}
	recordIRTTSeries(col, r, 20)
	if v := col.series[store.SeriesIPDVSend]; len(v) != 0 {
		t.Errorf("ipdv_send = %v; the negative round trip's jitter was kept", v)
	}
	// Server processing time comes from the same untrusted round trip.
	if v := col.series[store.SeriesServerProcessing]; len(v) != 1 {
		t.Errorf("server_processing = %v, want only the good round trip's value", v)
	}
}

// A server that paces differently than asked can return more results than the
// interval has room for. The round-trip loop caps at spec.Pings; a longer
// distribution would describe a packet set the stored sent/received do not.
func TestSeriesCappedAtPings(t *testing.T) {
	ms := time.Millisecond
	var trips []irtt.RoundTrip
	for i := range 10 {
		d := time.Duration(i) * 10 * ms
		trips = append(trips, rt(d, d+2*ms, d+3*ms, d+6*ms))
	}
	r := &irtt.Result{Config: dualStamped(), RoundTrips: trips}
	col := &collector{}
	recordIRTTSeries(col, r, 4)
	if v := col.series[store.SeriesServerProcessing]; len(v) != 4 {
		t.Errorf("server_processing has %d values for a 4-probe interval: %v", len(v), v)
	}
}
