package main

import "testing"

// A proxy terminating TLS on the same host leaves smokeng listening on
// loopback while the browser is on https. Deciding from the listen address
// alone dropped Secure from the session cookie there, which puts it one
// downgrade away from travelling in the clear.
func TestCookieSecurityFollowsTheAddressTheBrowserUsed(t *testing.T) {
	cases := []struct {
		name        string
		externalURL string
		listen      string
		wantInsecur bool
	}{
		{"proxy on the same host, TLS outside", "https://smokeng.example.org", "127.0.0.1:8080", false},
		{"no proxy, bound publicly", "", "0.0.0.0:8080", false},
		{"local development", "", "127.0.0.1:8080", true},
		{"external address is plain HTTP", "http://smokeng.lan:8080", "127.0.0.1:8080", true},
	}
	for _, tc := range cases {
		if got := cookieInsecure(tc.externalURL, tc.listen); got != tc.wantInsecur {
			t.Errorf("%s: cookieInsecure = %v, want %v", tc.name, got, tc.wantInsecur)
		}
	}
}
