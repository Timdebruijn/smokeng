package api

import (
	"net"
	"net/http"
	"net/url"
	"strings"
)

// contentSecurityPolicy is the strictest policy the built frontend runs under:
// its own scripts, styles, fonts and workers, images it draws itself, and
// requests to its own origin. It is not a defence against a page that already
// escapes what it renders (there is no such sink; the build has no dangerous DOM
// writes), but it makes the next mistake one the browser refuses to act on, and
// it forbids framing, which is how a click is stolen.
const contentSecurityPolicy = "default-src 'self'; script-src 'self'; style-src 'self'; " +
	"img-src 'self' data:; font-src 'self'; connect-src 'self'; worker-src 'self'; " +
	"object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

// protect wraps the whole mux, so a route added later is covered without its
// author having to remember.
func (s *server) protect(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		if s.restrictHosts && !s.knownHost(r.Host) {
			writeJSON(w, http.StatusForbidden, map[string]string{
				"error": "this server answers only to localhost while it has no authentication; " +
					"set --external-url if it is reached through a proxy under another name",
			})
			return
		}
		if !s.sameOrigin(r) {
			writeJSON(w, http.StatusForbidden, map[string]string{
				"error": "a change must be made from this site, not from another; if this is the site and " +
					"a proxy rewrites the Host header, set --external-url to the address people use",
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// sameOrigin reports whether a state-changing request may proceed. Reads always
// may. For a change, the browser's own statement of where the request came from
// decides, and a request that makes none is not from a browser: an agent
// signs its requests and a command-line client holds no session to ride on.
func (s *server) sameOrigin(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" {
		return site == "same-origin" || site == "none"
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false // "null" included
	}
	if strings.EqualFold(u.Host, r.Host) {
		return true
	}
	ext, err := url.Parse(s.externalURL)
	return s.externalURL != "" && err == nil && strings.EqualFold(u.Host, ext.Host)
}

// knownHost reports whether a Host header names this server as a server with no
// login should be reached: loopback, or the address it was told it is served at.
func (s *server) knownHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.ToLower(host), "]")
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), ".")
	// Browsers resolve localhost and its subdomains to loopback themselves, so a
	// page cannot point one of them anywhere else.
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return true
	}
	if s.externalURL != "" {
		if ext, err := url.Parse(s.externalURL); err == nil {
			return strings.EqualFold(hostport, ext.Host) || strings.EqualFold(host, ext.Hostname())
		}
	}
	return false
}
