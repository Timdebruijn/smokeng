package probe

import (
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

// Helpers shared by the probe types that are timed around a userspace call
// rather than by the kernel: dns, tcp, http, https and irtt.
//
// None of them can be kernel-timestamped, so finalize flags every measurement
// they produce as userspace on both sides (DESIGN.md §5.2). That is not a
// detail to gloss over: a band widened by a busy prober must never be
// readable as a slow service.

// tcpNetwork pins the dial to one address family. The address is already
// resolved to a literal of the right family, so "tcp" would work — but naming
// the family means a misresolution fails loudly here instead of quietly
// measuring the other protocol.
func tcpNetwork(family string) string {
	if family == "v6" {
		return "tcp6"
	}
	return "tcp4"
}

// dscpControl returns a dialer Control hook that marks the socket, or nil when
// the target asks for no marking.
//
// Without this a target's dscp setting would apply to its icmp probes and be
// silently ignored for every other type — the graph would say it was measuring
// a marked flow while the kernel sent it unmarked.
func dscpControl(dscp int, family string) func(network, address string, c syscall.RawConn) error {
	if dscp == 0 {
		return nil
	}
	return func(_, _ string, rc syscall.RawConn) error {
		var soErr error
		ctlErr := rc.Control(func(fd uintptr) {
			// DSCP occupies the top six bits of the TOS/traffic-class octet.
			tos := dscp << 2
			if family == "v6" {
				soErr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IPV6, unix.IPV6_TCLASS, tos)
			} else {
				soErr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_TOS, tos)
			}
		})
		if ctlErr != nil {
			return ctlErr
		}
		return soErr
	}
}

// probeDialer is the dialer every connection-based userspace probe uses.
func probeDialer(spec TargetSpec) *net.Dialer {
	return &net.Dialer{Control: dscpControl(spec.DSCP, spec.Family)}
}
