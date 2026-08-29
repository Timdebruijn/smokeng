// Package timestamp isolates kernel packet timestamping and the socket error
// queue it is read from (DESIGN.md §5.2).
//
// On Linux it enables SO_TIMESTAMPING with software TX+RX timestamps: RX
// stamps arrive as SCM_TIMESTAMPING control messages alongside the packet,
// TX stamps are read back from the socket error queue (MSG_ERRQUEUE) and
// correlated to sends via the SOF_TIMESTAMPING_OPT_ID packet counter. That
// queue is shared: with IP_RECVERR enabled the same reads also surface ICMP
// errors, so both kinds of entry are returned here rather than requiring a
// second, near-identical parser elsewhere.
//
// On other platforms every function reports "no capability" and the caller
// falls back to userspace clocks, recording the degradation in the
// measurement flags — reduced accuracy is observable, never silent.
package timestamp

import "time"

// Caps reports which kernel timestamping capabilities are active on a socket.
type Caps struct {
	KernelRX bool
	KernelTX bool
}

// TXStamp is one transmit timestamp read from the error queue. Counter is the
// SOF_TIMESTAMPING_OPT_ID sequence: 0 for the first packet sent after
// EnableKernel, incrementing per send.
type TXStamp struct {
	Counter uint32
	At      time.Time
}

// ICMPError is an ICMP error reported for one of our packets — the router
// said why the probe failed, instead of the probe simply going unanswered.
type ICMPError struct {
	Type, Code uint8
	// Payload is the offending datagram as the kernel returned it. For a ping
	// socket that is our own echo request, whose sequence number is what
	// attributes the error to a specific ping.
	Payload []byte
}

// ErrQueueEntry is one item from the socket error queue: either a transmit
// timestamp or an ICMP error, never both.
type ErrQueueEntry struct {
	TXStamp   *TXStamp
	ICMPError *ICMPError
}

// EnableKernel attempts to enable kernel software timestamping on fd and
// reports what was achieved. A zero Caps means full userspace fallback.
func EnableKernel(fd int) Caps { return enableKernel(fd) }

// FromOOB extracts the software RX timestamp from received control messages.
// ok is false when none is present (caller uses its userspace clock).
func FromOOB(oob []byte) (t time.Time, ok bool) { return fromOOB(oob) }

// EnableICMPErrors turns on IP_RECVERR / IPV6_RECVERR, which routes ICMP
// errors for our packets onto the error queue instead of discarding them.
func EnableICMPErrors(fd int, ipv6 bool) bool { return enableICMPErrors(fd, ipv6) }

// ReadErrQueue drains the socket error queue without blocking, returning
// every entry it held. An empty queue returns (nil, nil).
func ReadErrQueue(fd int) ([]ErrQueueEntry, error) { return readErrQueue(fd) }
