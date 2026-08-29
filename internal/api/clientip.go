package api

import (
	"net"
	"net/http"
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
type TrustedProxies []*net.IPNet

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
			ip := net.ParseIP(part)
			if ip == nil {
				return nil, &net.ParseError{Type: "IP address or CIDR", Text: part}
			}
			bits := 32
			if ip.To4() == nil {
				bits = 128
			}
			out = append(out, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
			continue
		}
		_, n, err := net.ParseCIDR(part)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, nil
}

func (t TrustedProxies) has(ip net.IP) bool {
	for _, n := range t {
		if n.Contains(ip) {
			return true
		}
	}
	return false
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
	peer := net.ParseIP(host)
	if peer == nil || len(s.trusted) == 0 || !s.trusted.has(peer) {
		return host
	}
	hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		ip := net.ParseIP(strings.TrimSpace(hops[i]))
		if ip == nil {
			break // an unparseable entry ends the chain we can believe
		}
		if !s.trusted.has(ip) {
			return ip.String()
		}
		host = ip.String()
	}
	return host
}
