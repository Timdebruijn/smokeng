package probe

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/timdebruijn/smokeng/internal/probe/timestamp"
)

// dnsResult is one query's outcome, in the terms the collector records.
type dnsResult struct {
	// TXUser is stamped just before the query is handed to the kernel, and is
	// what finalize falls back to when there is no kernel stamp.
	TXUser time.Time
	// TXKernel is zero when the kernel supplied none. finalize validates it
	// against this query's own bounds before believing it.
	TXKernel time.Time
	RX       time.Time
	RXKernel bool
}

// dnsRoundTrip sends one DNS query on a socket of its own and times the reply,
// taking kernel timestamps where the platform offers them.
//
// This exists instead of the library's Exchange because Exchange owns its
// socket, and a socket we do not own cannot be asked for SO_TIMESTAMPING. What
// that buys is the difference between measuring the network and measuring the
// network plus whatever delayed this goroutine: on Linux a loaded prober
// inflated the userspace-timed p99 by more than an order of magnitude, while
// kernel-stamped probes were unmoved. A resolver's latency is worth knowing to
// the same standard as a ping's.
//
// One socket per query, connected to the server. That means no demultiplexing:
// the socket carries exactly one exchange, so the error queue's TX counter is
// unambiguous and a reply cannot belong to another query. The DNS ID is still
// checked, because a connected UDP socket bounds who may send to us, not what
// they may send.
func dnsRoundTrip(ctx context.Context, query []byte, wantID uint16, addr netip.Addr, port int, spec TargetSpec) (dnsResult, error) {
	var res dnsResult

	// Unmap once, here: a v4 address carried in v6 form must pick AF_INET and
	// IP_TOS, not their v6 counterparts, and deciding that twice from an
	// unmapped and a mapped copy is how the two disagree.
	addr = addr.Unmap()
	domain := unix.AF_INET
	if addr.Is6() {
		domain = unix.AF_INET6
	}
	fd, err := unix.Socket(domain, unix.SOCK_DGRAM, unix.IPPROTO_UDP)
	if err != nil {
		return res, fmt.Errorf("dns: socket: %w", err)
	}
	// From here the fd is owned by this function until it is handed to
	// net.FileConn, which takes a copy of its own.
	closeFD := true
	defer func() {
		if closeFD {
			unix.Close(fd)
		}
	}()

	if spec.DSCP != 0 {
		tos := spec.DSCP << 2
		var soErr error
		if addr.Is6() {
			soErr = unix.SetsockoptInt(fd, unix.IPPROTO_IPV6, unix.IPV6_TCLASS, tos)
		} else {
			soErr = unix.SetsockoptInt(fd, unix.IPPROTO_IP, unix.IP_TOS, tos)
		}
		if soErr != nil {
			return res, fmt.Errorf("dns: set DSCP: %w", soErr)
		}
	}

	// Before the send, or there is no transmit stamp to read back. Off Linux
	// this reports nothing and the userspace clock is used, flagged as such.
	caps := timestamp.EnableKernel(fd)

	sa, err := sockaddrFor(addr, port)
	if err != nil {
		return res, err
	}
	if err := unix.Connect(fd, sa); err != nil {
		return res, fmt.Errorf("dns: connect: %w", err)
	}
	if err := unix.SetNonblock(fd, true); err != nil {
		return res, fmt.Errorf("dns: set nonblocking: %w", err)
	}

	// Hand the socket to the runtime's poller rather than blocking an OS
	// thread per query. Twenty queries an interval across a hundred resolvers
	// is two thousand concurrent waits, and pinning a thread to each is a cost
	// the measurement does not need to pay.
	f := os.NewFile(uintptr(fd), "dns-probe")
	conn, err := net.FileConn(f)
	f.Close() // FileConn duplicated it
	closeFD = false
	if err != nil {
		return res, fmt.Errorf("dns: adopt socket: %w", err)
	}
	defer conn.Close()

	rc, err := conn.(*net.UDPConn).SyscallConn()
	if err != nil {
		return res, err
	}
	deadline := time.Now().Add(time.Duration(spec.TimeoutMS) * time.Millisecond)
	if err := conn.SetDeadline(deadline); err != nil {
		return res, err
	}
	// Shutdown has to interrupt a query in flight, or stopping the service
	// waits out every outstanding timeout. Winding the deadline back is what
	// unblocks the poller; closing the connection here would race the reader.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.SetDeadline(time.Now())
		case <-stop:
		}
	}()

	res.TXUser = time.Now()
	if _, err := conn.Write(query); err != nil {
		return res, fmt.Errorf("dns: send: %w", err)
	}

	buf := make([]byte, 1500)
	oob := make([]byte, 256)
	for {
		n, oobn, rxErr := readMsg(rc, buf, oob)
		if rxErr != nil {
			return res, rxErr
		}
		// A reply for a different query is not ours to time. On a connected
		// socket this is vanishingly unlikely, and checking costs two bytes.
		if n < 2 || binary.BigEndian.Uint16(buf[:2]) != wantID {
			continue
		}
		res.RX = time.Now()
		if caps.KernelRX {
			if t, ok := timestamp.FromOOB(oob[:oobn]); ok {
				res.RX, res.RXKernel = t, true
			}
		}
		break
	}

	// The transmit stamp lands on the error queue once the packet is actually
	// on the wire, which is certainly true by the time its answer is back.
	if caps.KernelTX {
		_ = rc.Control(func(cfd uintptr) {
			entries, err := timestamp.ReadErrQueue(int(cfd))
			if err != nil {
				return
			}
			for _, e := range entries {
				// One query per socket, so any transmit stamp here is this
				// query's — the counter has nothing to disambiguate against.
				if e.TXStamp != nil {
					res.TXKernel = e.TXStamp.At
				}
			}
		})
	}
	return res, nil
}

// readMsg performs one recvmsg through the poller, so a wait costs a goroutine
// rather than a thread.
func readMsg(rc syscall.RawConn, buf, oob []byte) (n, oobn int, err error) {
	var innerErr error
	rcErr := rc.Read(func(fd uintptr) bool {
		var rerr error
		n, oobn, _, _, rerr = unix.Recvmsg(int(fd), buf, oob, 0)
		if errors.Is(rerr, unix.EAGAIN) || errors.Is(rerr, unix.EWOULDBLOCK) {
			return false // not ready; the poller parks us until it is
		}
		innerErr = rerr
		return true
	})
	if rcErr != nil {
		return 0, 0, rcErr // deadline or a closed connection
	}
	if innerErr != nil {
		return 0, 0, innerErr
	}
	return n, oobn, nil
}

// sockaddrFor renders an already-unmapped address for the raw socket layer.
func sockaddrFor(addr netip.Addr, port int) (unix.Sockaddr, error) {
	if addr.Is6() {
		return &unix.SockaddrInet6{Port: port, Addr: addr.As16()}, nil
	}
	if !addr.Is4() {
		return nil, fmt.Errorf("dns: %s is neither IPv4 nor IPv6", addr)
	}
	return &unix.SockaddrInet4{Port: port, Addr: addr.As4()}, nil
}
