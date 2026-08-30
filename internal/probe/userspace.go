package probe

import (
	"errors"
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

// Helpers shared by the probe types that do not use the shared icmp socket:
// dns, tcp, http, https and irtt.
//
// Of these only dns is kernel-timestamped, on a socket of its own (see
// dnssocket.go). The others are timed around a userspace call and cannot be
// otherwise — a tcp or tls handshake completes inside the kernel and userspace
// only sees the call return — so finalize flags every measurement they produce
// as userspace on both sides (DESIGN.md §5.2). That is not a detail to gloss
// over: a band widened by a busy prober must never be readable as a slow
// service.

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

// isLocalResourceError reports whether a dial error was smokeng's own machine
// running out of something, rather than a statement about the target or the
// path to it.
//
// The distinction matters because the two are recorded differently: a target
// that refuses a connection or times out is genuine loss, measured; but the
// prober running out of file descriptors or ephemeral ports is a failure to
// measure at all, and recording that as loss would draw an outage on a target
// that may be perfectly healthy. Only the unambiguous local-exhaustion errnos
// are treated this way — EMFILE and friends can only be ours. ECONNREFUSED,
// timeouts, and unreachable-host/network are left as loss, because for a
// remote target those describe the thing being measured, and a monitoring
// tool should show them.
func isLocalResourceError(err error) bool {
	for _, e := range []syscall.Errno{
		syscall.EMFILE,        // this process is out of file descriptors
		syscall.ENFILE,        // the whole machine is out of file descriptors
		syscall.EADDRNOTAVAIL, // no ephemeral port to source the connection from
		syscall.ENOBUFS,       // no kernel buffer to build the packet
		syscall.ENOMEM,        // out of memory
	} {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}
