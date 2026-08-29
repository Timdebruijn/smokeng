package api

import (
	"net/http/httptest"
	"testing"
)

// X-Forwarded-For is written by whoever is upstream, so believing it from an
// arbitrary peer would let anyone put any address in the log. It is read only
// while the hop it came from is one the operator named, and the first hop that
// is not is the answer.
func TestClientIPBelievesOnlyTrustedHops(t *testing.T) {
	trusted, err := ParseTrustedProxies("10.0.0.0/8, 192.168.1.1")
	if err != nil {
		t.Fatal(err)
	}
	s := &server{trusted: trusted}

	cases := []struct {
		name, peer, xff, want string
	}{
		{"no header", "10.0.0.5:1234", "", "10.0.0.5"},
		{"one trusted proxy", "10.0.0.5:1234", "203.0.113.7", "203.0.113.7"},
		{"chain through two trusted hops", "10.0.0.5:1234", "203.0.113.7, 10.0.0.9", "203.0.113.7"},
		// The peer is not trusted, so nothing it claims is believed.
		{"untrusted peer forging a header", "198.51.100.4:1234", "203.0.113.7", "198.51.100.4"},
		// Everything left of the first untrusted hop was written by someone we
		// have no reason to believe.
		{"forged prefix behind a real proxy", "10.0.0.5:1234", "1.2.3.4, 203.0.113.7", "203.0.113.7"},
		{"bare address form", "192.168.1.1:1234", "203.0.113.7", "203.0.113.7"},
		{"garbage in the chain", "10.0.0.5:1234", "not-an-ip", "10.0.0.5"},
	}
	for _, tc := range cases {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = tc.peer
		if tc.xff != "" {
			r.Header.Set("X-Forwarded-For", tc.xff)
		}
		if got := s.clientIP(r); got != tc.want {
			t.Errorf("%s: clientIP = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// With none configured, the header is ignored entirely: the peer is always
// true, even when it is only the proxy.
func TestClientIPIgnoresTheHeaderWithoutTrustedProxies(t *testing.T) {
	s := &server{}
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.5:1234"
	r.Header.Set("X-Forwarded-For", "203.0.113.7")
	if got := s.clientIP(r); got != "10.0.0.5" {
		t.Errorf("clientIP = %q, want the peer", got)
	}
}
