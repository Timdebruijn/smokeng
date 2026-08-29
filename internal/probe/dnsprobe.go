package probe

import (
	"context"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"codeberg.org/miekg/dns"
)

// defaultDNSPort is where a resolver listens unless the target says otherwise.
const defaultDNSPort = 53

// One client for every DNS probe: it holds no per-query state, and the
// alternative is building one per probe several times a second.
var dnsClient = dns.NewClient()

// probeDNS times one query against the target, which is the resolver being
// measured — not the name being asked about.
//
// A resolver going slow is invisible to ICMP: the host answers pings promptly
// while every lookup behind it crawls. That is the whole reason this type
// exists, and why it times the query rather than the host.
//
// The RTT is taken around a userspace call, so it carries the scheduler's
// jitter in exactly the way DESIGN.md §5.2 describes. finalize flags it as
// userspace on both sides because no kernel timestamp is ever recorded here —
// a band widened by a busy prober must not be readable as a slow resolver.
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

	qctx, cancel := context.WithTimeout(ctx, time.Duration(spec.TimeoutMS)*time.Millisecond)
	defer cancel()

	col.markSent(idx, 0, time.Now())
	// Not the library's own rtt: that measures its call, and what belongs in
	// the distribution is the span every other probe type reports — the moment
	// before the request left to the moment the answer was in hand.
	_, _, err := dnsClient.Exchange(qctx, m, "udp", server)
	if err != nil {
		// No answer inside the timeout is loss, the same as an unanswered
		// echo. It is not recorded as a send failure: the query did go out.
		return
	}
	col.onRX(idx, time.Now(), false)
}

// rrType maps the configured record type. The set is the one tree.Validate
// accepts, kept here as a plain table so the probe does not depend on however
// the library happens to spell its lookups.
func rrType(s string) (uint16, bool) {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "A":
		return dns.TypeA, true
	case "AAAA":
		return dns.TypeAAAA, true
	case "CNAME":
		return dns.TypeCNAME, true
	case "MX":
		return dns.TypeMX, true
	case "NS":
		return dns.TypeNS, true
	case "PTR":
		return dns.TypePTR, true
	case "SOA":
		return dns.TypeSOA, true
	case "SRV":
		return dns.TypeSRV, true
	case "TXT":
		return dns.TypeTXT, true
	}
	return 0, false
}
