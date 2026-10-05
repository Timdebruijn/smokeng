package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/timdebruijn/smokeng/internal/auth"
	"github.com/timdebruijn/smokeng/internal/store"
)

func adminSession() *auth.Session { return &auth.Session{Subject: "a", Role: auth.RoleAdmin} }

// post sends a state-changing request as the admin with the given extra headers
// and reports the status. 400 is "it reached the handler and the empty body was
// refused", which is what a request that is let through looks like here.
func post(t *testing.T, h http.Handler, host string, hdr map[string]string) int {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/v1/targets", strings.NewReader("{}"))
	if host != "" {
		req.Host = host
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

// A session cookie is sent with a request a page on another site makes, and
// SameSite=Lax does not stop one from a sibling subdomain (same site, different
// origin). A browser says where the request came from, so a change that did not
// come from this origin is refused. What does not say, a command-line client or
// an agent, is not a browser and carries no ambient session to ride on.
func TestStateChangingRequestsMustComeFromThisOrigin(t *testing.T) {
	h := New(seededStore(t), Options{Auth: &fakeAuth{session: adminSession()},
		ExternalURL: "https://smokeng.example.org"}, fstest.MapFS{})
	for _, c := range []struct {
		name string
		host string
		hdr  map[string]string
		want bool // let through
	}{
		{"no headers at all", "", nil, true},
		{"fetch metadata: same origin", "", map[string]string{"Sec-Fetch-Site": "same-origin"}, true},
		{"fetch metadata: typed in the address bar", "", map[string]string{"Sec-Fetch-Site": "none"}, true},
		{"fetch metadata: a sibling subdomain", "", map[string]string{"Sec-Fetch-Site": "same-site"}, false},
		{"fetch metadata: another site", "", map[string]string{"Sec-Fetch-Site": "cross-site"}, false},
		{"origin: the public address behind a proxy", "backend:8080", map[string]string{"Origin": "https://smokeng.example.org"}, true},
		{"origin: another host", "", map[string]string{"Origin": "https://evil.example"}, false},
		{"origin: a prefix of this host", "", map[string]string{"Origin": "http://example.com.evil.example"}, false},
		{"origin: null (a sandboxed frame)", "", map[string]string{"Origin": "null"}, false},
		{"origin: not a URL", "", map[string]string{"Origin": "::::"}, false},
		// Fetch metadata is the browser's own word and wins over an Origin it
		// would not have sent differently.
		{"fetch metadata cross-site beats a matching origin", "", map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "http://example.com"}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			code := post(t, h, c.host, c.hdr)
			if got := code != http.StatusForbidden; got != c.want {
				t.Errorf("status %d, let through = %v, want %v", code, got, c.want)
			}
		})
	}
	// Every method that changes something, not just POST.
	for _, method := range []string{"POST", "PUT", "PATCH", "DELETE"} {
		for site, want := range map[string]bool{"cross-site": false, "same-site": false, "same-origin": true} {
			req := httptest.NewRequest(method, "/api/v1/targets/999999", strings.NewReader("{}"))
			req.Header.Set("Sec-Fetch-Site", site)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if got := rec.Code != http.StatusForbidden; got != want {
				t.Errorf("%s from %s: status %d, let through = %v, want %v", method, site, rec.Code, got, want)
			}
		}
	}
	// Reads are not state changes; a cross-site read is not this guard's business.
	req := httptest.NewRequest("GET", "/api/v1/targets", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("a cross-site GET was answered %d", rec.Code)
	}
}

// Agents authenticate by signature and send neither header; a browser has no
// reason to post to the ingest route. Both must keep working as they did.
func TestTheOriginGuardLeavesSignedAgentsAlone(t *testing.T) {
	h, _, agentID, key, mine, _ := ingestFixture(t)
	if rec := submit(t, h, agentID, key, []store.Measurement{measurement(mine, 1_756_400_000)}, time.Now()); rec.Code != http.StatusOK {
		t.Fatalf("a signed agent submission = %d: %s", rec.Code, rec.Body)
	}
}

func TestEveryResponseCarriesTheSecurityHeaders(t *testing.T) {
	h := New(seededStore(t), Options{Auth: &fakeAuth{session: adminSession()}},
		fstest.MapFS{"index.html": {Data: []byte("<html></html>")}})
	for _, path := range []string{"/", "/api/v1/targets", "/healthz", "/nothing-here"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		for name, want := range map[string]string{
			"X-Content-Type-Options":       "nosniff",
			"Referrer-Policy":              "no-referrer",
			"Cross-Origin-Opener-Policy":   "same-origin",
			"Cross-Origin-Resource-Policy": "same-origin",
		} {
			if got := rec.Header().Get(name); got != want {
				t.Errorf("%s: %s = %q, want %q", path, name, got, want)
			}
		}
		csp := rec.Header().Get("Content-Security-Policy")
		for _, must := range []string{"default-src 'self'", "script-src 'self'", "frame-ancestors 'none'", "object-src 'none'", "base-uri 'none'"} {
			if !strings.Contains(csp, must) {
				t.Errorf("%s: Content-Security-Policy %q lacks %q", path, csp, must)
			}
		}
		if strings.Contains(csp, "unsafe-eval") || strings.Contains(csp, "script-src 'self' 'unsafe-inline'") {
			t.Errorf("%s: the policy allows script it does not need to: %q", path, csp)
		}
	}
}

// With no authentication the API answers anyone who can reach it, and what
// reaches it is a browser on the same machine, which a page on any site can
// drive by pointing its own name at 127.0.0.1. The Host header is the one thing
// such a page cannot choose.
func TestAnUnauthenticatedLoopbackServerAnswersOnlyItsOwnNames(t *testing.T) {
	st := seededStore(t)
	h := New(st, Options{RestrictHosts: true, ExternalURL: "https://smokeng.example.org"}, fstest.MapFS{})
	get := func(host string) int {
		req := httptest.NewRequest("GET", "/api/v1/targets", nil)
		req.Host = host
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	for _, host := range []string{"localhost:8080", "LOCALHOST", "localhost.", "app.localhost:8080", "127.0.0.1:8080", "127.0.0.2", "[::1]:8080", "[::1]", "smokeng.example.org"} {
		if code := get(host); code != http.StatusOK {
			t.Errorf("Host %q = %d, want 200", host, code)
		}
	}
	for _, host := range []string{"attacker.example", "attacker.example:8080", "localhost.attacker.example", "127.0.0.1.attacker.example", "[[::1]]", "notlocalhost", "10.0.0.1:8080", ""} {
		if code := get(host); code != http.StatusForbidden {
			t.Errorf("Host %q = %d, want 403", host, code)
		}
	}
	// Off, as when authentication is on, every name is served.
	open := New(st, Options{}, fstest.MapFS{})
	req := httptest.NewRequest("GET", "/api/v1/targets", nil)
	req.Host = "attacker.example"
	rec := httptest.NewRecorder()
	open.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("with the check off, a foreign Host = %d", rec.Code)
	}
}

// Decoding a batch is what costs memory, and only a signed agent gets that far.
// Past a handful at once the master says to come back, which an agent treats as
// "keep it buffered", instead of taking all of them on at once.
func TestIngestDecodesAFewBatchesAtATime(t *testing.T) {
	h, _, agentID, key, mine, _ := ingestFixture(t)
	srv := h.(*handler).srv
	if cap(srv.decodeSlots) == 0 {
		t.Fatal("no limit on concurrent decodes")
	}
	for i := 0; i < cap(srv.decodeSlots); i++ {
		srv.decodeSlots <- struct{}{}
	}
	rec := submit(t, h, agentID, key, []store.Measurement{measurement(mine, 1_756_400_000)}, time.Now())
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("with every slot taken: %d, Retry-After %q", rec.Code, rec.Header().Get("Retry-After"))
	}
	<-srv.decodeSlots // one frees
	if rec := submit(t, h, agentID, key, []store.Measurement{measurement(mine, 1_756_400_060)}, time.Now()); rec.Code != http.StatusOK {
		t.Fatalf("with a slot free: %d: %s", rec.Code, rec.Body)
	}
}

// slotProbe reports how many decode slots are taken at the moment a batch is
// written, which is after it was decoded: the slot is held for the handling,
// not just admitted at the door.
type slotProbe struct {
	*store.SQLite
	srv   **server
	inUse int
}

func (p *slotProbe) IngestMeasurements(ctx context.Context, ms []store.Measurement) (int, error) {
	p.inUse = len((*p.srv).decodeSlots)
	return p.SQLite.IngestMeasurements(ctx, ms)
}

func TestADecodeSlotIsHeldWhileBatchIsHandledAndThenReleased(t *testing.T) {
	_, st, agentID, key, mine, _ := ingestFixture(t)
	var srv *server
	probe := &slotProbe{SQLite: st, srv: &srv}
	h := New(probe, Options{}, fstest.MapFS{})
	srv = h.(*handler).srv
	for i := 0; i <= cap(srv.decodeSlots)+1; i++ { // more batches than slots: a leaked slot runs out
		rec := submit(t, h, agentID, key, []store.Measurement{measurement(mine, 1_756_400_000+int64(i)*60)}, time.Now())
		if rec.Code != http.StatusOK {
			t.Fatalf("batch %d = %d: %s", i, rec.Code, rec.Body)
		}
		if probe.inUse != 1 {
			t.Fatalf("batch %d: %d slot(s) held while it was written, want 1", i, probe.inUse)
		}
	}
	if n := len(srv.decodeSlots); n != 0 {
		t.Errorf("%d slot(s) still held after every batch finished", n)
	}
}

// Where the browser sends no Sec-Fetch-Site (plain http, an old browser), the
// Origin is all there is, and it names a scheme, a host and a port. The same
// host on another scheme is another origin, and the default port is the same
// port however it is written.
func TestTheOriginFallbackComparesSchemeHostAndPort(t *testing.T) {
	st := seededStore(t)
	trusted, err := ParseTrustedProxies("10.0.0.0/8")
	if err != nil {
		t.Fatal(err)
	}
	plain := New(st, Options{Auth: &fakeAuth{session: adminSession()}, TrustedProxies: trusted}, fstest.MapFS{})
	https := New(st, Options{Auth: &fakeAuth{session: adminSession()}, ExternalURL: "https://smokeng.example.org"}, fstest.MapFS{})
	http80 := New(st, Options{Auth: &fakeAuth{session: adminSession()}, ExternalURL: "http://smokeng.example.org:80"}, fstest.MapFS{})
	try := func(h http.Handler, host, origin, peer, proto string) bool {
		req := httptest.NewRequest("POST", "/api/v1/targets", strings.NewReader("{}"))
		req.Host = host
		req.Header.Set("Origin", origin)
		if peer != "" {
			req.RemoteAddr = peer
		}
		if proto != "" {
			req.Header.Set("X-Forwarded-Proto", proto)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code != http.StatusForbidden
	}
	for _, c := range []struct {
		name                      string
		h                         http.Handler
		host, origin, peer, proto string
		want                      bool
	}{
		{"same host, same scheme", plain, "example.com", "http://example.com", "", "", true},
		{"explicit default port", plain, "example.com", "http://example.com:80", "", "", true},
		{"default port on the host side", plain, "example.com:80", "http://example.com", "", "", true},
		{"another port", plain, "example.com", "http://example.com:8080", "", "", false},
		{"another port on the host side", plain, "example.com:8080", "http://example.com", "", "", false},
		{"not a web origin", plain, "example.com", "ftp://example.com", "", "", false},
		{"https origin where plain http is assumed", plain, "example.com", "https://example.com", "", "", true},
		// A proxy we trust says the browser is on https: an http page of the same
		// host is a downgrade, not this site.
		{"http origin, trusted proxy says https", plain, "example.com", "http://example.com", "10.1.1.1:4000", "https", false},
		{"https origin, trusted proxy says https", plain, "example.com", "https://example.com", "10.1.1.1:4000", "https", true},
		{"http origin, trusted proxy says http", plain, "example.com", "http://example.com", "10.1.1.1:4000", "http", true},
		{"http origin, untrusted peer claims https", plain, "example.com", "http://example.com", "203.0.113.5:4000", "https", true},
		// An https external URL: the host matches but the scheme is not the site's.
		{"external https, http origin on the same host", https, "smokeng.example.org", "http://smokeng.example.org", "", "", false},
		{"external https, explicit 443", https, "backend:8080", "https://smokeng.example.org:443", "", "", true},
		{"external https, plain https origin", https, "backend:8080", "https://smokeng.example.org", "", "", true},
		{"external https, http origin on another host name", https, "backend:8080", "http://smokeng.example.org", "", "", false},
		{"external https, another port", https, "backend:8080", "https://smokeng.example.org:8443", "", "", false},
		{"external http with :80, origin without it", http80, "backend:8080", "http://smokeng.example.org", "", "", true},
		{"external http, https origin", http80, "backend:8080", "https://smokeng.example.org", "", "", false},
	} {
		if got := try(c.h, c.host, c.origin, c.peer, c.proto); got != c.want {
			t.Errorf("%s: Host %q Origin %q: let through = %v, want %v", c.name, c.host, c.origin, got, c.want)
		}
	}
}
