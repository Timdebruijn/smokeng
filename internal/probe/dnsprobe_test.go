package probe

import (
	"context"
	"encoding/binary"
	"net"
	"net/netip"
	"sync/atomic"
	"testing"

	"github.com/timdebruijn/smokeng/internal/store"
)

// dnsResponder answers any query by echoing it back with the response bit set.
// The probe times a round trip; it does not read the answer section, so this
// is a complete far end for what is being measured.
func dnsResponder(t *testing.T) int {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			if n < 3 {
				continue
			}
			reply := make([]byte, n)
			copy(reply, buf[:n])
			reply[2] |= 0x80 // QR: this is a response
			_, _ = pc.WriteTo(reply, from)
		}
	}()
	return pc.LocalAddr().(*net.UDPAddr).Port
}

func dnsSpec(t *testing.T, port int) TargetSpec {
	t.Helper()
	s := userspaceSpec(t, "dns", port)
	s.DNSQuery = "example.com"
	s.DNSRRType = "A"
	return s
}

func TestProbeDNSMeasuresAResolver(t *testing.T) {
	spec := dnsSpec(t, dnsResponder(t))
	got := runOne(t, spec)
	if got.sent != 1 || got.received != 1 || got.samples != 1 {
		t.Fatalf("got %+v, want one sent, one received, one sample", got)
	}
}

// A resolver that never answers is loss, not a send failure: the query did go
// out, and the distinction is between a broken target and a broken smokeng.
func TestProbeDNSSilentResolverIsLoss(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close() // bound, so nothing is refused; simply never answers

	spec := dnsSpec(t, pc.LocalAddr().(*net.UDPAddr).Port)
	spec.TimeoutMS = 150

	var late atomic.Int64
	col := newCollector(1, &late)
	probeDNS(context.Background(), col, 0, netip.MustParseAddr("127.0.0.1"), spec)
	m := col.finalize(spec, 0, conditions{})
	if m.Sent != 1 || m.Received != 0 {
		t.Fatalf("got %d sent %d received, want one attempted and nothing back", m.Sent, m.Received)
	}
	if m.Flags&store.FlagSendFailed != 0 {
		t.Fatal("an unanswered query was recorded as a send failure; the resolver is what " +
			"failed, not the prober")
	}
}

// A reply carrying somebody else's transaction id is not this query's answer.
func TestProbeDNSIgnoresAForeignTransactionID(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			reply := make([]byte, n)
			copy(reply, buf[:n])
			// Answer, but under a different id.
			binary.BigEndian.PutUint16(reply[:2], binary.BigEndian.Uint16(reply[:2])+1)
			reply[2] |= 0x80
			_, _ = pc.WriteTo(reply, from)
		}
	}()

	spec := dnsSpec(t, pc.LocalAddr().(*net.UDPAddr).Port)
	spec.TimeoutMS = 200
	got := runOne(t, spec)
	if got.received != 0 {
		t.Fatalf("got %+v; a reply under another id was counted as this query's answer", got)
	}
}

// A DNS query that fails to reach the wire (here an oversized datagram forcing
// EMSGSIZE) must come back with Sent=false so probeDNS records it as a send
// failure, not as the resolver going lossy. TXUser is still stamped — it is set
// just before the write — which is exactly why Sent, not TXUser, is the signal.
func TestDNSRoundTripWriteFailureIsNotSent(t *testing.T) {
	port := dnsResponder(t) // a real listener, so connect() succeeds
	spec := dnsSpec(t, port)

	oversized := make([]byte, 70000) // larger than a UDP datagram can carry
	res, err := dnsRoundTrip(context.Background(), oversized, 0,
		netip.MustParseAddr("127.0.0.1"), port, spec)
	if err == nil {
		t.Fatal("an oversized query was sent without error; expected the write to fail")
	}
	if res.Sent {
		t.Fatal("Sent is true after a failed write; probeDNS would record resolver loss for a " +
			"query that never left the host")
	}
	if res.TXUser.IsZero() {
		t.Fatal("TXUser should still be stamped — the point is that Sent, not TXUser, tells the " +
			"caller the query never went out")
	}
}
