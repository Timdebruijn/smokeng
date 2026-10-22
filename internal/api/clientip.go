package api

import (
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
)

// TrustedProxies is the set of peers whose forwarding headers may be believed.
//
// The scope of this is deliberately narrow, and worth stating plainly:
// smokeng makes no authorisation decision on a client address. Agents
// authenticate with Ed25519 signatures and browsers with a session cookie, so
// a forged X-Forwarded-For cannot bypass anything — it can only put a lie in a
// log line. That is the whole reason this exists: behind a proxy, every log
// entry otherwise names the proxy, which makes them useless for saying where a
// refused enrolment or a rejected submission actually came from.
//
// Because nothing is gated on it, an empty list is a safe default: without one
// the peer address is used, which is always true even when it is unhelpful.
type TrustedProxies []netip.Prefix

// ParseTrustedProxies reads a comma-separated list of CIDRs, or bare addresses,
// which are taken as single-host ranges.
func ParseTrustedProxies(s string) (TrustedProxies, error) {
	var out TrustedProxies
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if !strings.Contains(part, "/") {
			ip, ok := parseIP(part)
			if !ok {
				return nil, &net.ParseError{Type: "IP address or CIDR", Text: part}
			}
			out = append(out, netip.PrefixFrom(ip, ip.BitLen()))
			continue
		}
		p, err := netip.ParsePrefix(part)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// parseIP is netip.ParseAddr for a literal address only: no zone, and an
// IPv4-mapped IPv6 address is the IPv4 address it names.
func parseIP(s string) (netip.Addr, bool) {
	ip, err := netip.ParseAddr(s)
	if err != nil || ip.Zone() != "" {
		return netip.Addr{}, false
	}
	return ip.Unmap(), true
}

func (t TrustedProxies) has(ip netip.Addr) bool {
	return slices.ContainsFunc(t, func(p netip.Prefix) bool { return p.Contains(ip) })
}

// clientIP reports the address a request came from, seeing through any proxies
// that were configured as trusted.
//
// X-Forwarded-For is read right to left, discarding entries only while the hop
// they came from is trusted. The first untrusted hop is the answer, because
// everything to the left of it was written by someone we have no reason to
// believe. With no trusted proxies configured, the peer is the answer and the
// header is ignored entirely.
func (s *server) clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer, ok := parseIP(host)
	if !ok || !s.trusted.has(peer) {
		return host
	}
	hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		ip, ok := parseIP(strings.TrimSpace(hops[i]))
		if !ok {
			break // an unparseable entry ends the chain we can believe
		}
		if !s.trusted.has(ip) {
			return ip.String()
		}
		host = ip.String()
	}
	return host
}

// ForwardedHTTPS reports whether a trusted proxy says the browser connected over
// TLS. Like the client address it is believed only from a trusted peer, and what
// it decides is whether a cookie is marked Secure, never who may do what.
func (t TrustedProxies) ForwardedHTTPS(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer, ok := parseIP(host)
	if !ok || !t.has(peer) {
		return false
	}
	proto, _, _ := strings.Cut(r.Header.Get("X-Forwarded-Proto"), ",")
	return strings.EqualFold(strings.TrimSpace(proto), "https")
}
