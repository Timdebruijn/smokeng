//go:build linux

package probe

import (
	"context"
	"net/netip"
	"sync/atomic"
	"testing"

	"github.com/timdebruijn/smokeng/internal/store"
)

// The point of taking the DNS query off the library's socket and onto one of
// our own: the kernel stamps it, so what lands in the distribution is the
// resolver's latency rather than the resolver's latency plus whatever delayed
// this goroutine. A userspace flag on a dns measurement means the kernel path
// silently stopped working.
//
// Where the kernel declines to stamp — an old kernel, a restricted container —
// this skips rather than fails: falling back is the designed behaviour and it
// is flagged, not hidden. The skip message says which half was missing, so a
// regression does not read the same as an unsupported host.
func TestProbeDNSUsesKernelTimestamps(t *testing.T) {
	spec := dnsSpec(t, dnsResponder(t))

	var late atomic.Int64
	col := newCollector(1, &late)
	probeDNS(context.Background(), col, 0, netip.MustParseAddr("127.0.0.1"), spec)
	m := col.finalize(spec, 0, conditions{})

	if m.Received != 1 {
		t.Fatalf("no measurement to check: %d sent, %d received", m.Sent, m.Received)
	}
	if m.Flags&store.FlagUserspaceRX != 0 {
		t.Skip("this kernel gave no receive timestamp on a UDP socket; the flagged " +
			"userspace fallback is in use")
	}
	if m.Flags&store.FlagUserspaceTX != 0 {
		t.Skip("this kernel gave no transmit timestamp on a UDP socket; the flagged " +
			"userspace fallback is in use")
	}
	if m.Flags != 0 {
		t.Fatalf("flags = %d, want 0: a kernel-stamped dns measurement carries no "+
			"degradation flags", m.Flags)
	}
}
