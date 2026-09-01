package probe

import (
	"context"
	"log"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/timdebruijn/smokeng/internal/store"
)

const (
	defaultHTTPPort  = 80
	defaultHTTPSPort = 443
)

// userAgent identifies the prober in the access logs of whatever it measures.
// Someone reading those logs should be able to tell at a glance that the
// traffic is monitoring rather than a client, and where it came from.
const userAgent = "smokeng (+https://github.com/timdebruijn/smokeng)"

// probeHTTP times one request against the target and records the time to
// response headers.
//
// This is the type that measures a service rather than a path. It builds a
// fresh connection every probe — TCP handshake, TLS handshake where the scheme
// asks for it, then the request — because reusing a connection would make the
// first sample of a session include a handshake the other nineteen skipped,
// and a distribution split between two unrelated populations is worse than a
// slower one. What you see is what a client arriving cold would experience.
//
// The address comes from smokeng's own TTL-aware resolver, not from the HTTP
// client: the URL names the host so virtual hosting and certificate
// verification behave, while the dial goes to the address this target
// resolved. Letting net/http resolve again would mean the graph and the
// recorded resolution could disagree about what was measured.
func probeHTTP(ctx context.Context, col *collector, idx int, addr netip.Addr, spec TargetSpec) {
	scheme := spec.ProbeType // "http" or "https"
	port := spec.ProbePort
	if port == 0 {
		port = defaultHTTPPort
		if scheme == "https" {
			port = defaultHTTPSPort
		}
	}
	path := strings.TrimSpace(spec.HTTPPath)
	if path == "" {
		path = "/"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	url := scheme + "://" + net.JoinHostPort(spec.Host, strconv.Itoa(port)) + path
	dest := net.JoinHostPort(addr.String(), strconv.Itoa(port))

	tr := &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return probeDialer(spec).DialContext(ctx, tcpNetwork(spec.Family), dest)
		},
		// The certificate is checked against the target's host, which is what
		// the URL names — not against the address we dialled. Verifying the
		// address would fail every correctly-configured virtual host.
		TLSClientConfig: tlsConfigFor(spec),
	}
	defer tr.CloseIdleConnections()
	// No redirect following: a 302 would add a second round trip, to a second
	// host, and quietly report the pair as one measurement of the first.
	client := &http.Client{
		Transport:     tr,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	rctx, cancel := context.WithTimeout(ctx, time.Duration(spec.TimeoutMS)*time.Millisecond)
	defer cancel()

	req, err := http.NewRequestWithContext(rctx, http.MethodGet, url, nil)
	if err != nil {
		// A malformed URL never reached the network, so this is a send
		// failure, not loss: nothing was asked of the target.
		col.markSendFailed(idx, store.SendReasonSocket)
		if idx == 0 {
			log.Printf("probe: target %d: build request %q: %v", spec.TargetID, url, err)
		}
		return
	}
	req.Header.Set("User-Agent", userAgent)

	col.markSent(idx, 0, time.Now())
	resp, err := client.Do(req)
	if err != nil {
		// fd or ephemeral-port exhaustion on the prober is ours, not the
		// service failing — the same distinction the tcp probe draws. Every
		// other error (refused, timed out, TLS rejected) is a real measurement
		// of the endpoint and stays loss.
		if isLocalResourceError(err) {
			col.markSendFailed(idx, sendReasonFor(err))
			if idx == 0 {
				log.Printf("probe: target %d (%s): request to %s failed locally: %v; "+
					"recorded as a send failure, not loss", spec.TargetID, spec.Host, url, err)
			}
			return
		}
		return // no response inside the timeout, or a refused/reset connection: loss
	}
	// Do returns once the headers are in hand; the body is still streaming.
	// Stamp here, which is time-to-first-byte, and do not read the body: how
	// fast a page transfers is a bandwidth question, and mixing it into a
	// latency distribution would make a large page look like a slow network.
	rx := time.Now()
	resp.Body.Close()

	if resp.StatusCode >= 400 {
		// The transport worked and the server answered, but it did not serve
		// what was asked for. Counting that as a good measurement would draw a
		// healthy green band over an outage, so it counts as loss — and is
		// named once per interval, because "100% loss" alone would send you
		// looking at the network for a fault that is in the application.
		if idx == 0 {
			log.Printf("probe: target %d (%s): %s returned HTTP %d; counted as loss",
				spec.TargetID, spec.Host, url, resp.StatusCode)
		}
		return
	}
	col.onRX(idx, rx, false)
}
