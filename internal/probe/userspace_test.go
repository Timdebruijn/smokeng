package probe

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/timdebruijn/smokeng/internal/store"
)

// spec builds a one-probe spec pointed at addr:port.
func userspaceSpec(t *testing.T, probeType string, port int) TargetSpec {
	t.Helper()
	return TargetSpec{
		TargetID: 1, Family: "v4", Host: "127.0.0.1",
		IntervalS: 10, Pings: 1, Mode: "burst", BurstGapMS: 10,
		TimeoutMS: 2000, PacketSize: 56,
		ProbeType: probeType, ProbePort: port,
	}
}

func runOne(t *testing.T, spec TargetSpec) store0 {
	t.Helper()
	var late atomic.Int64
	col := newCollector(spec.Pings, &late)
	runUserspaceProbe(context.Background(), col, 0, netip.MustParseAddr("127.0.0.1"), spec)
	m := col.finalize(spec, 0, conditions{})
	return store0{sent: m.Sent, received: m.Received, samples: len(m.Samples), probeErr: m.ProbeErr}
}

type store0 struct {
	sent, received, samples int
	probeErr                *uint16
}

// wantProbeErr fails unless got carries exactly want (0 meaning none).
func wantProbeErr(t *testing.T, got *uint16, want uint16) {
	t.Helper()
	switch {
	case want == 0 && got != nil:
		t.Errorf("probe error %#x, want none", *got)
	case want != 0 && (got == nil || *got != want):
		t.Errorf("probe error %v, want %#x", got, want)
	}
}

// listenTCP opens a listener that accepts and immediately closes, which is the
// far end of a tcp-connect probe.
func listenTCP(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	return l.Addr().(*net.TCPAddr).Port
}

func TestProbeTCPMeasuresAnOpenPort(t *testing.T) {
	got := runOne(t, userspaceSpec(t, "tcp", listenTCP(t)))
	if got.sent != 1 || got.received != 1 || got.samples != 1 {
		t.Fatalf("open port: got %+v, want one sent, one received, one sample", got)
	}
}

// A refused connection is a completed round trip to the host but not to the
// service, and the service is what a tcp probe is pointed at. It counts as
// loss — the same as a timeout — rather than as a fast, healthy sample.
func TestProbeTCPCountsRefusalAsLoss(t *testing.T) {
	// Bind and release, so the port is almost certainly nobody's.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()

	got := runOne(t, userspaceSpec(t, "tcp", port))
	if got.sent != 1 || got.received != 0 {
		t.Fatalf("refused port: got %+v, want one sent and nothing received", got)
	}
	wantProbeErr(t, got.probeErr, store.ProbeError(store.ProbeErrConn, store.ProbeConnRefused))
}

func portOf(t *testing.T, srv *httptest.Server) int {
	t.Helper()
	_, p, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(p)
	if err != nil {
		t.Fatal(err)
	}
	return port
}

func httpProbeAgainst(t *testing.T, h http.HandlerFunc) store0 {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return runOne(t, userspaceSpec(t, "http", portOf(t, srv)))
}

func TestProbeHTTPMeasuresA200(t *testing.T) {
	got := httpProbeAgainst(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	if got.sent != 1 || got.received != 1 {
		t.Fatalf("200: got %+v, want one sent and one received", got)
	}
	wantProbeErr(t, got.probeErr, 0)
}

// The transport worked and the server answered, but it did not serve what was
// asked for. Recording that as a good measurement would draw a healthy band
// over an outage, so it is loss.
func TestProbeHTTPCountsServerErrorAsLoss(t *testing.T) {
	got := httpProbeAgainst(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	if got.sent != 1 || got.received != 0 {
		t.Fatalf("503: got %+v, want one sent and nothing received", got)
	}
	wantProbeErr(t, got.probeErr, store.ProbeError(store.ProbeErrHTTP, 503))
}

// A server that answers the handshake with an alert, here because it wants a
// client certificate, rejected the TLS too; so does a port that answers https
// in plain HTTP.
func TestProbeHTTPSNamesARemoteTLSRejection(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	srv.TLS = &tls.Config{ClientAuth: tls.RequireAnyClientCert}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	spec := userspaceSpec(t, "https", portOf(t, srv))
	spec.TLSSkipVerify = true // so the server's certificate is not what fails
	wantProbeErr(t, runOne(t, spec).probeErr, store.ProbeError(store.ProbeErrConn, store.ProbeConnTLS))

	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	t.Cleanup(plain.Close)
	wantProbeErr(t, runOne(t, userspaceSpec(t, "https", portOf(t, plain))).probeErr,
		store.ProbeError(store.ProbeErrConn, store.ProbeConnTLS))
}

// A request abandoned by its target's context learned nothing about the
// service, so it carries no reason; one that ran out its own timeout did.
func TestProbeHTTPAbandonedHasNoReason(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	spec := userspaceSpec(t, "http", portOf(t, srv))
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	var late atomic.Int64
	col := newCollector(1, &late)
	runUserspaceProbe(ctx, col, 0, netip.MustParseAddr("127.0.0.1"), spec)
	if m := col.finalize(spec, 0, conditions{}); m.ProbeErr != nil {
		t.Errorf("an abandoned request got reason %#x", *m.ProbeErr)
	}
}

// A certificate the prober does not trust is a definite answer, and a
// different one from silence: it says the TLS is wrong, not the network.
func TestProbeHTTPSNamesARejectedCertificate(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	t.Cleanup(srv.Close)
	got := runOne(t, userspaceSpec(t, "https", portOf(t, srv)))
	if got.sent != 1 || got.received != 0 {
		t.Fatalf("untrusted certificate: got %+v, want one sent and nothing received", got)
	}
	wantProbeErr(t, got.probeErr, store.ProbeError(store.ProbeErrConn, store.ProbeConnTLS))
}

// A redirect is one round trip to this host. Following it would add a second,
// to a second host, and report the pair as one measurement of the first — so
// the 3xx itself is the measurement.
func TestProbeHTTPDoesNotFollowRedirects(t *testing.T) {
	var hits atomic.Int64
	got := httpProbeAgainst(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	})
	if got.received != 1 {
		t.Fatalf("302: got %+v, want it counted as a round trip", got)
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("the probe made %d requests; a redirect must not be followed", n)
	}
}

// A probe that outlives its timeout is loss, not a very slow sample.
func TestProbeHTTPTimesOut(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()

	spec := userspaceSpec(t, "http", portOf(t, srv))
	spec.TimeoutMS = 150
	got := runOne(t, spec)
	if got.sent != 1 || got.received != 0 {
		t.Fatalf("slow server: got %+v, want one sent and nothing received", got)
	}
	wantProbeErr(t, got.probeErr, store.ProbeError(store.ProbeErrConn, store.ProbeConnTimeout))
}

// validateSpec is what stops a target being scheduled at all. tcp is the only
// type with a required setting the tree cannot check, because whether a port
// is set is only answerable after inheritance is resolved.
func TestValidateSpecRequiresAPortForTCP(t *testing.T) {
	if err := validateSpec(TargetSpec{ProbeType: "tcp"}); err == nil {
		t.Fatal("tcp with no port was accepted; it would be probed against port 0")
	}
	if err := validateSpec(TargetSpec{ProbeType: "tcp", ProbePort: 443}); err != nil {
		t.Fatalf("tcp with a port was refused: %v", err)
	}
	for _, ty := range []string{"", "icmp", "dns", "http", "https"} {
		if err := validateSpec(TargetSpec{ProbeType: ty}); err != nil {
			t.Fatalf("%q was refused without a port, but has a default: %v", ty, err)
		}
	}
	// irtt needs no port either, but it does need a positive send interval —
	// with one it is accepted, without one (a zero burst gap) it is refused,
	// because a zero-interval session is one irtt rejects before a packet goes
	// out and would otherwise graph a healthy server as total loss forever.
	if err := validateSpec(TargetSpec{ProbeType: "irtt", Mode: "burst", BurstGapMS: 50}); err != nil {
		t.Fatalf("irtt with a positive gap was refused: %v", err)
	}
	if err := validateSpec(TargetSpec{ProbeType: "irtt", Mode: "burst", BurstGapMS: 0}); err == nil {
		t.Fatal("irtt with a zero send interval was accepted; it would fail every interval")
	}
	if err := validateSpec(TargetSpec{ProbeType: "carrier-pigeon"}); err == nil {
		t.Fatal("a type with no prober behind it was accepted")
	}
}

// An unknown type must not silently become loss on the graph: nothing was
// asked of the target, so the target is not what failed.
func TestUnknownProbeTypeIsASendFailure(t *testing.T) {
	var late atomic.Int64
	col := newCollector(1, &late)
	spec := userspaceSpec(t, "carrier-pigeon", 0)
	runUserspaceProbe(context.Background(), col, 0, netip.MustParseAddr("127.0.0.1"), spec)
	m := col.finalize(spec, 0, conditions{})
	if m.Sent != 1 || m.Received != 0 {
		t.Fatalf("got %d sent %d received, want one attempted and nothing back", m.Sent, m.Received)
	}
	if m.Flags&store.FlagSendFailed == 0 {
		t.Fatalf("flags %d do not say the send failed", m.Flags)
	}
}
