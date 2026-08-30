//go:build linux

package probe

import (
	"net/netip"
	"sync/atomic"
	"testing"
)

// C1 regression: once a send has failed in a way that may have desynced the
// kernel transmit-stamp counter, no later ping may be given a kernel TX stamp.
//
// The failure it guards against is silent and severe: after a desync the
// byCounter map hands each ping the *next* ping's transmit stamp, which — in
// burst mode, where the inter-send gap is shorter than the RTT — lands inside
// the victim's [txUser, rx] window and so passes finalize's bounds check as a
// confident, fully-kernel-timestamped, wrong RTT. The fix abandons kernel TX
// stamps for the socket's life once desync is possible; the structural proof
// is that byCounter takes no new entries after the latch.
func TestKernelTXAbandonedAfterDesync(t *testing.T) {
	var late atomic.Int64
	c, err := openConn("v4", 0, &late)
	if err != nil {
		t.Skipf("datagram ICMP socket not permitted (see net.ipv4.ping_group_range): %v", err)
	}
	defer c.close()
	if !c.caps.KernelTX {
		t.Skip("this kernel does not offer TX timestamping; there is no kernel TX path to abandon")
	}

	loopback := netip.MustParseAddr("127.0.0.1")

	// Before the latch, a send registers the ping for kernel-TX attribution.
	col := newCollector(1, &late)
	if err := c.send(col, 0, loopback, 56); err != nil {
		t.Fatalf("send before desync: %v", err)
	}
	c.mu.Lock()
	beforeEntries := len(c.byCounter)
	c.mu.Unlock()
	if beforeEntries == 0 {
		t.Fatal("a healthy send registered no byCounter entry, so the test cannot tell the " +
			"abandonment apart from that")
	}

	// Latch as the Sendto error path does, then send again.
	c.txDesync.Store(true)
	col2 := newCollector(1, &late)
	c.mu.Lock()
	countBefore := len(c.byCounter)
	c.mu.Unlock()
	if err := c.send(col2, 0, loopback, 56); err != nil {
		t.Fatalf("send after desync: %v", err)
	}
	c.mu.Lock()
	countAfter := len(c.byCounter)
	c.mu.Unlock()

	if countAfter != countBefore {
		t.Fatalf("a send after the desync latch added a byCounter entry (%d → %d); a later "+
			"packet's transmit stamp could then be attributed to it as a confident wrong RTT",
			countBefore, countAfter)
	}
}
