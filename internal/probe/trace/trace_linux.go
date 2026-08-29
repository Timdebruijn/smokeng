//go:build linux

package trace

import (
	"context"
	"fmt"
	"net/netip"
	"time"
	"unsafe"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
	"golang.org/x/sys/unix"
)

// traceroute walks TTLs from 1 upward on a socket of its own. A dedicated
// socket matters: IP_TTL is a socket option, and setting it on the shared
// measurement socket would silently truncate every other target's probes.
func traceroute(ctx context.Context, opts Options) (Path, error) {
	v6 := opts.Dest.Is6() && !opts.Dest.Is4In6()
	domain, proto := unix.AF_INET, unix.IPPROTO_ICMP
	if v6 {
		domain, proto = unix.AF_INET6, unix.IPPROTO_ICMPV6
	}
	fd, err := unix.Socket(domain, unix.SOCK_DGRAM, proto)
	if err != nil {
		return nil, fmt.Errorf("trace: open socket: %w", err)
	}
	defer unix.Close(fd)

	// A ping socket only joins the kernel's table once bound.
	var bind unix.Sockaddr = &unix.SockaddrInet4{}
	if v6 {
		bind = &unix.SockaddrInet6{}
	}
	if err := unix.Bind(fd, bind); err != nil {
		return nil, fmt.Errorf("trace: bind: %w", err)
	}
	// Without this the time-exceeded replies never reach us, and every hop
	// would look like silence.
	if err := enableRecvErr(fd, v6); err != nil {
		return nil, ErrUnsupported
	}
	if err := unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO,
		&unix.Timeval{Usec: 200_000}); err != nil {
		return nil, err
	}

	var path Path
	for ttl := 1; ttl <= opts.MaxHops; ttl++ {
		if ctx.Err() != nil {
			return path, ctx.Err()
		}
		if err := setTTL(fd, v6, ttl); err != nil {
			return path, fmt.Errorf("trace: set TTL %d: %w", ttl, err)
		}
		hop, arrived, err := probeHop(ctx, fd, opts, v6, ttl)
		if err != nil {
			return path, err
		}
		path = append(path, hop)
		if arrived {
			return path, nil
		}
	}
	return path, nil
}

// probeHop sends one echo at the current TTL and waits for whichever comes
// back first: a time-exceeded from the router at that distance, or an echo
// reply meaning we have arrived.
func probeHop(ctx context.Context, fd int, opts Options, v6 bool, ttl int) (Hop, bool, error) {
	seq := ttl
	var typ icmp.Type = ipv4.ICMPTypeEcho
	if v6 {
		typ = ipv6.ICMPTypeEchoRequest
	}
	msg := icmp.Message{
		Type: typ,
		Body: &icmp.Echo{ID: 0, Seq: seq, Data: []byte("smokeng-trace")},
	}
	wire, err := msg.Marshal(nil)
	if err != nil {
		return Hop{TTL: ttl}, false, err
	}
	var dst unix.Sockaddr
	if v6 {
		dst = &unix.SockaddrInet6{Addr: opts.Dest.As16()}
	} else {
		dst = &unix.SockaddrInet4{Addr: opts.Dest.As4()}
	}

	sent := time.Now()
	if err := unix.Sendto(fd, wire, 0, dst); err != nil {
		// A refusal here is about this hop, not the whole path: record it as
		// unanswered and carry on.
		return Hop{TTL: ttl}, false, nil
	}

	deadline := sent.Add(opts.Timeout)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return Hop{TTL: ttl}, false, ctx.Err()
		}
		// The router's time-exceeded lands on the error queue.
		if addr, ok := readTimeExceeded(fd, seq, v6); ok {
			return Hop{TTL: ttl, Addr: addr, RTT: time.Since(sent)}, false, nil
		}
		// An echo reply means the destination itself answered.
		if arrivedAt(fd, seq, v6) {
			return Hop{TTL: ttl, Addr: opts.Dest, RTT: time.Since(sent)}, true, nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return Hop{TTL: ttl}, false, nil
}

// readTimeExceeded drains the error queue looking for the router that dropped
// our probe. The queue returns the offending datagram, so the sequence number
// confirms the error answers this hop rather than a stale one.
func readTimeExceeded(fd, seq int, v6 bool) (netip.Addr, bool) {
	buf := make([]byte, 1500)
	oob := make([]byte, 1024)
	for {
		n, oobn, _, _, err := unix.Recvmsg(fd, buf, oob, unix.MSG_ERRQUEUE|unix.MSG_DONTWAIT)
		if err != nil {
			return netip.Addr{}, false
		}
		cmsgs, err := unix.ParseSocketControlMessage(oob[:oobn])
		if err != nil {
			continue
		}
		var offender netip.Addr
		var isTimeExceeded bool
		for _, m := range cmsgs {
			if (m.Header.Level == unix.IPPROTO_IP && m.Header.Type == unix.IP_RECVERR) ||
				(m.Header.Level == unix.IPPROTO_IPV6 && m.Header.Type == unix.IPV6_RECVERR) {
				if len(m.Data) < int(unsafe.Sizeof(unix.SockExtendedErr{})) {
					continue
				}
				se := (*unix.SockExtendedErr)(unsafe.Pointer(&m.Data[0]))
				if se.Origin != unix.SO_EE_ORIGIN_ICMP && se.Origin != unix.SO_EE_ORIGIN_ICMP6 {
					continue
				}
				// Type 11 (v4) and type 3 (v6) are "time exceeded".
				if (!v6 && se.Type == 11) || (v6 && se.Type == 3) {
					isTimeExceeded = true
					offender = offenderAddr(m.Data, v6)
				}
			}
		}
		if !isTimeExceeded {
			continue
		}
		// Confirm it answers this probe and not an older one.
		if !matchesSeq(buf[:n], seq, v6) {
			continue
		}
		return offender, offender.IsValid()
	}
}

// offenderAddr reads the router address that follows sock_extended_err in the
// IP_RECVERR control message.
func offenderAddr(data []byte, v6 bool) netip.Addr {
	off := int(unsafe.Sizeof(unix.SockExtendedErr{}))
	if v6 {
		if len(data) < off+int(unsafe.Sizeof(unix.RawSockaddrInet6{})) {
			return netip.Addr{}
		}
		sa := (*unix.RawSockaddrInet6)(unsafe.Pointer(&data[off]))
		return netip.AddrFrom16(sa.Addr)
	}
	if len(data) < off+int(unsafe.Sizeof(unix.RawSockaddrInet4{})) {
		return netip.Addr{}
	}
	sa := (*unix.RawSockaddrInet4)(unsafe.Pointer(&data[off]))
	return netip.AddrFrom4(sa.Addr)
}

// matchesSeq checks the returned datagram is the echo request we just sent.
func matchesSeq(payload []byte, seq int, v6 bool) bool {
	proto := 1
	if v6 {
		proto = 58
	}
	msg, err := icmp.ParseMessage(proto, payload)
	if err != nil {
		return false
	}
	echo, ok := msg.Body.(*icmp.Echo)
	return ok && echo.Seq == seq
}

// arrivedAt reports whether an echo reply for this probe is waiting, meaning
// the destination answered and the path is complete.
func arrivedAt(fd, seq int, v6 bool) bool {
	buf := make([]byte, 1500)
	proto := 1
	if v6 {
		proto = 58
	}
	for {
		n, _, err := unix.Recvfrom(fd, buf, unix.MSG_DONTWAIT)
		if err != nil {
			return false
		}
		msg, err := icmp.ParseMessage(proto, buf[:n])
		if err != nil {
			continue
		}
		if msg.Type != ipv4.ICMPTypeEchoReply && msg.Type != ipv6.ICMPTypeEchoReply {
			continue
		}
		if echo, ok := msg.Body.(*icmp.Echo); ok && echo.Seq == seq {
			return true
		}
	}
}

func enableRecvErr(fd int, v6 bool) error {
	if v6 {
		return unix.SetsockoptInt(fd, unix.IPPROTO_IPV6, unix.IPV6_RECVERR, 1)
	}
	return unix.SetsockoptInt(fd, unix.IPPROTO_IP, unix.IP_RECVERR, 1)
}

func setTTL(fd int, v6 bool, ttl int) error {
	if v6 {
		return unix.SetsockoptInt(fd, unix.IPPROTO_IPV6, unix.IPV6_UNICAST_HOPS, ttl)
	}
	return unix.SetsockoptInt(fd, unix.IPPROTO_IP, unix.IP_TTL, ttl)
}
