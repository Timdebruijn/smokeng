// Package timestamp isolates kernel packet timestamping (DESIGN.md §5.2).
//
// On Linux it enables SO_TIMESTAMPING with software TX+RX timestamps: RX
// stamps arrive as SCM_TIMESTAMPING control messages alongside the packet,
// TX stamps are read back from the socket error queue (MSG_ERRQUEUE) and
// correlated to sends via the SOF_TIMESTAMPING_OPT_ID packet counter. On
// other platforms every function reports "no capability" and the caller falls
// back to userspace clocks, recording the degradation in the measurement
// flags — reduced accuracy is observable, never silent.
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

// EnableKernel attempts to enable kernel software timestamping on fd and
// reports what was achieved. A zero Caps means full userspace fallback.
func EnableKernel(fd int) Caps { return enableKernel(fd) }

// FromOOB extracts the software RX timestamp from received control messages.
// ok is false when none is present (caller uses its userspace clock).
func FromOOB(oob []byte) (t time.Time, ok bool) { return fromOOB(oob) }

// ReadErrQueue drains currently queued TX timestamps from the socket error
// queue without blocking. An empty queue returns (nil, nil).
func ReadErrQueue(fd int) ([]TXStamp, error) { return readErrQueue(fd) }
