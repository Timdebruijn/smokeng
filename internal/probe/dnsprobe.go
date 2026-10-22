package probe

import (
	"context"
	"log"
	"net"
	"net/netip"
	"strconv"
	"strings"

	"codeberg.org/miekg/dns"

	"github.com/timdebruijn/smokeng/internal/store"
)

// defaultDNSPort is where a resolver listens unless the target says otherwise.
const defaultDNSPort = 53

// probeDNS times one query against the target, which is the resolver being
// measured — not the name being asked about.
//
// A resolver going slow is invisible to ICMP: the host answers pings promptly
// while every lookup behind it crawls. That is the whole reason this type
// exists, and why it times the query rather than the host.
//
// The query runs on a socket of smokeng's own rather than the library's, so
// the kernel can stamp it (dnssocket.go). Where it does, a dns measurement is
// as free of the prober's own jitter as an icmp one; where it does not, the
// userspace fallback is flagged like everywhere else. A band widened by a busy
// prober must not be readable as a slow resolver.
func probeDNS(ctx context.Context, col *collector, idx int, addr netip.Addr, spec TargetSpec) {
	name := strings.TrimSpace(spec.DNSQuery)
	if name == "" {
		// Asking the root for its NS records is the smallest question every
		// resolver can answer, so it is what we ask when nobody said.
		name = "."
	}
	rr := dns.TypeA
	if name == "." {
		rr = dns.TypeNS
	}
	if v, ok := rrType(spec.DNSRRType); ok {
		rr = v
	}

	port := spec.ProbePort
	if port == 0 {
		port = defaultDNSPort
	}
	server := net.JoinHostPort(addr.String(), strconv.Itoa(port))

	m := dns.NewMsg(name, rr)
	m.ID = dns.ID()
	if err := m.Pack(); err != nil {
		// Nothing was asked of the resolver, so the resolver is not what
		// failed. That distinction is the difference between a broken target
		// and a broken smokeng.
		col.markSendFailed(idx, store.SendReasonSocket)
		log.Printf("probe: target %d: pack DNS query: %v", spec.TargetID, err)
		return
	}

	res, err := dnsRoundTrip(ctx, m.Data, m.ID, addr, port, spec)
	if !res.Sent {
		// The query never reached the wire — the socket would not open or
		// connect, or the write itself failed (a firewall REJECT on OUTPUT,
		// ENOBUFS, an oversized query). That is smokeng's side, not the
		// resolver's, so it must not read as a healthy resolver going lossy.
		col.markSendFailed(idx, sendReasonFor(err))
		if idx == 0 {
			log.Printf("probe: target %d (%s): dns query to %s never sent: %v",
				spec.TargetID, spec.Host, server, err)
		}
		return
	}
	col.markSent(idx, 0, res.TXUser)
	if err != nil {
		// The query went out and no answer came back inside the timeout: loss,
		// the same as an unanswered echo, and genuinely the resolver's.
		return
	}
	// Hand both stamps to the collector and let finalize decide. It already
	// validates a kernel transmit stamp against its own probe's bounds and
	// flags the fallbacks, and routing this through the same place means a
	// dns measurement is scrutinised exactly as an icmp one is.
	if !res.TXKernel.IsZero() {
		col.onTXKernel(idx, res.TXKernel)
	}
	col.onRX(idx, res.RX, res.RXKernel)
}

// rrType maps the configured record type; tree.Validate has already limited
// it to the set smokeng asks for.
func rrType(s string) (uint16, bool) {
	t, ok := dns.StringToType[strings.ToUpper(strings.TrimSpace(s))]
	return t, ok
}
