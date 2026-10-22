// Package trace discovers the network path to a target (DESIGN.md §9a).
//
// It exists to answer one question: when the smoke changed shape, did the path
// change? Everything a traceroute could otherwise offer — per-hop latency,
// path scoring, MTR — is a different product and deliberately absent.
//
// Hops are found with TTL-limited echo requests, reading the address of each
// router from the ICMP time-exceeded reply it sends back. That is the same
// error-queue machinery the prober already uses for ICMP errors: the queue
// returns the offending datagram alongside the error, and its sequence number
// says which probe was answered.
package trace

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
	"time"
)

// Hop is one step on the path. A zero Addr means the router did not answer,
// which is common and not an error: many routers rate-limit or suppress
// time-exceeded replies.
type Hop struct {
	Addr netip.Addr
}

// Path is the ordered hop list to a target.
type Path []Hop

// String renders a path for storage and comparison: comma-separated hops,
// with `*` for one that did not answer. Text, because this is diffed far more
// often than parsed, and being readable in a sqlite3 session is worth more
// than the bytes.
func (p Path) String() string {
	parts := make([]string, 0, len(p))
	for _, h := range p {
		if h.Addr.IsValid() {
			parts = append(parts, h.Addr.String())
		} else {
			parts = append(parts, "*")
		}
	}
	return strings.Join(parts, ",")
}

// Options configure one traceroute.
type Options struct {
	Dest    netip.Addr
	MaxHops int
	// Timeout is how long to wait for each hop before giving up on it.
	Timeout time.Duration
}

// ErrUnsupported means this platform cannot discover a path. The caller
// records no path rather than a wrong one, and the absence stays visible.
var ErrUnsupported = fmt.Errorf("trace: path discovery needs the socket error queue")

// Trace walks the path to a destination.
func Trace(ctx context.Context, opts Options) (Path, error) {
	if opts.MaxHops <= 0 {
		opts.MaxHops = 30
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 2 * time.Second
	}
	if !opts.Dest.IsValid() {
		return nil, fmt.Errorf("trace: no destination")
	}
	return traceroute(ctx, opts)
}
