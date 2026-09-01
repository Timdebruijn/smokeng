package probe

import (
	"context"
	"errors"
	"log"
	"net"
	"net/netip"
	"strconv"
	"syscall"
	"time"
)

// probeTCP times one TCP handshake against the target's port.
//
// What lands in the distribution is the SYN → SYN-ACK round trip, read from
// userspace once the kernel has completed the handshake. That is a real round
// trip over the path ICMP takes, but through a queue ICMP does not share, and
// that difference is the whole reason for the type: a router that
// deprioritises ICMP, or a middlebox that answers pings itself while the
// service behind it is unreachable, both look healthy to an echo request.
//
// The handshake is what is measured, not the service on top of it. For "is it
// answering", use the http probe.
func probeTCP(ctx context.Context, col *collector, idx int, addr netip.Addr, spec TargetSpec) {
	dctx, cancel := context.WithTimeout(ctx, time.Duration(spec.TimeoutMS)*time.Millisecond)
	defer cancel()

	dest := net.JoinHostPort(addr.String(), strconv.Itoa(spec.ProbePort))

	col.markSent(idx, 0, time.Now())
	c, err := probeDialer(spec).DialContext(dctx, tcpNetwork(spec.Family), dest)
	if err != nil {
		// The prober running out of file descriptors or ephemeral ports is not
		// the target failing — recording it as loss would draw an outage on a
		// service that may be answering fine. Flag it as ours instead.
		if isLocalResourceError(err) {
			col.markSendFailed(idx, sendReasonFor(err))
			if idx == 0 {
				log.Printf("probe: target %d (%s): tcp/%d dial failed locally: %v; "+
					"recorded as a send failure, not loss", spec.TargetID, spec.Host, spec.ProbePort, err)
			}
			return
		}
		// A refusal is a completed round trip to the host, but not to the
		// service — and the service is what a tcp probe is pointed at, so it
		// counts as loss exactly as a timeout does. Said once per interval
		// rather than once per probe: twenty identical lines a minute would
		// describe one unchanging fact.
		if idx == 0 && errors.Is(err, syscall.ECONNREFUSED) {
			log.Printf("probe: target %d (%s): tcp/%d refused the connection; counted as loss",
				spec.TargetID, spec.Host, spec.ProbePort)
		}
		return
	}
	// Stamp before tearing down, so the teardown's cost is not in the reading.
	col.onRX(idx, time.Now(), false)

	// Abort rather than close politely. The connection carried no data, so
	// there is nothing a FIN would flush — and a FIN leaves this side holding
	// an ephemeral port in TIME_WAIT for a minute per probe. Twenty probes a
	// minute against a few dozen services is thousands of ports pinned by a
	// prober that measured nothing with them.
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetLinger(0)
	}
	c.Close()
}
