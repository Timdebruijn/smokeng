// Package dnscache provides TTL-respecting hostname resolution
// (DESIGN.md §5.4): direct DNS queries against the system resolver (the
// stdlib resolver hides TTLs), a cache that serves the last-known address and
// refreshes asynchronously when the record's TTL expires, and strict A/AAAA
// selection per address family — never "whatever resolves". The ping loop
// blocks on DNS only for a host's very first lookup.
package dnscache

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"time"

	"codeberg.org/miekg/dns"
	"codeberg.org/miekg/dns/dnsconf"
)

const (
	minTTL       = 30 * time.Second
	maxTTL       = 24 * time.Hour
	queryTimeout = 5 * time.Second
)

// ErrNoRecord is a resolver's definitive answer that the name has no record of
// the wanted type: NOERROR or NXDOMAIN with nothing of that type in the answer.
// A resolver that failed, timed out or answered SERVFAIL or REFUSED says
// nothing about the name and is not this.
var ErrNoRecord = errors.New("no record")

type key struct {
	host   string
	family string
}

type entry struct {
	addr       netip.Addr
	expires    time.Time
	refreshing bool
}

// Cache resolves hostnames with TTL-based refresh. Safe for concurrent use.
type Cache struct {
	client  *dns.Client
	servers []string
	// exchange asks one server. It is client.Exchange, and a field so a test can
	// give the resolver an answer without a network.
	exchange func(ctx context.Context, m *dns.Msg, server string) (*dns.Msg, error)

	mu      sync.Mutex
	entries map[key]*entry
}

// New builds a cache using the resolvers from /etc/resolv.conf.
func New() (*Cache, error) {
	conf, err := dnsconf.FromFile("/etc/resolv.conf")
	if err != nil {
		return nil, fmt.Errorf("dnscache: read system resolver config: %w", err)
	}
	if len(conf.Servers) == 0 {
		return nil, fmt.Errorf("dnscache: no nameservers in /etc/resolv.conf")
	}
	port := conf.Port
	if port == "" {
		port = "53"
	}
	servers := make([]string, 0, len(conf.Servers))
	for _, s := range conf.Servers {
		servers = append(servers, net.JoinHostPort(s, port))
	}
	client := dns.NewClient()
	return &Cache{
		client:  client,
		servers: servers,
		exchange: func(ctx context.Context, m *dns.Msg, server string) (*dns.Msg, error) {
			r, _, err := client.Exchange(ctx, m, "udp", server)
			return r, err
		},
		entries: map[key]*entry{},
	}, nil
}

// Lookup returns the address for host in the given family ("v4"/"v6").
// Literal IPs bypass DNS entirely but are checked against the family. A
// cached address whose TTL has expired is returned as-is while a background
// refresh runs — the caller never waits on DNS after the first lookup.
func (c *Cache) Lookup(ctx context.Context, host, family string) (netip.Addr, error) {
	if ip, err := netip.ParseAddr(host); err == nil {
		if !familyMatches(ip, family) {
			return netip.Addr{}, fmt.Errorf("dnscache: literal %s is not an %s address", host, family)
		}
		return ip, nil
	}

	k := key{host, family}
	c.mu.Lock()
	e, ok := c.entries[k]
	if ok {
		addr := e.addr
		if time.Now().After(e.expires) && !e.refreshing {
			e.refreshing = true
			go c.refresh(k)
		}
		c.mu.Unlock()
		return addr, nil
	}
	c.mu.Unlock()

	// First-ever lookup for this host: resolve synchronously.
	addr, ttl, err := c.resolve(ctx, host, family)
	if err != nil {
		return netip.Addr{}, err
	}
	c.mu.Lock()
	c.entries[k] = &entry{addr: addr, expires: time.Now().Add(ttl)}
	c.mu.Unlock()
	return addr, nil
}

func (c *Cache) refresh(k key) {
	ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
	defer cancel()
	addr, ttl, err := c.resolve(ctx, k.host, k.family)
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.entries[k]
	e.refreshing = false
	if errors.Is(err, ErrNoRecord) {
		// The record is gone. Serving the old address would probe something the
		// name no longer points at, so forget it: the next Lookup resolves
		// again and returns the error.
		delete(c.entries, k)
		return
	}
	if err != nil {
		// No resolver gave an answer about the name: keep serving the stale
		// address and retry after the minimum TTL.
		e.expires = time.Now().Add(minTTL)
		return
	}
	e.addr = addr
	e.expires = time.Now().Add(ttl)
}

func (c *Cache) resolve(ctx context.Context, host, family string) (netip.Addr, time.Duration, error) {
	qtype := dns.TypeA
	if family == "v6" {
		qtype = dns.TypeAAAA
	}
	m := dns.NewMsg(host, qtype)
	m.ID = dns.ID()

	var lastErr error
	for _, server := range c.servers {
		qctx, cancel := context.WithTimeout(ctx, queryTimeout)
		r, err := c.exchange(qctx, m, server)
		cancel()
		if err != nil {
			lastErr = err
			continue
		}
		// Only NOERROR and NXDOMAIN answer the question. SERVFAIL, REFUSED and
		// the rest are this server failing, so the next one is tried.
		if r.Rcode != dns.RcodeSuccess && r.Rcode != dns.RcodeNameError {
			lastErr = fmt.Errorf("resolver %s answered rcode %d", server, r.Rcode)
			continue
		}
		for _, rr := range r.Answer {
			switch a := rr.(type) {
			case *dns.A:
				if family == "v4" {
					return a.Addr, clampTTL(a.Hdr.TTL), nil
				}
			case *dns.AAAA:
				if family == "v6" {
					return a.Addr, clampTTL(a.Hdr.TTL), nil
				}
			}
		}
		return netip.Addr{}, 0, fmt.Errorf("dnscache: %w: no %s record for %s", ErrNoRecord, recordName(family), host)
	}
	return netip.Addr{}, 0, fmt.Errorf("dnscache: all resolvers failed for %s: %w", host, lastErr)
}

func clampTTL(ttl uint32) time.Duration {
	d := time.Duration(ttl) * time.Second
	return min(max(d, minTTL), maxTTL)
}

func familyMatches(ip netip.Addr, family string) bool {
	if family == "v6" {
		return ip.Is6() && !ip.Is4In6()
	}
	return ip.Is4() || ip.Is4In6()
}

func recordName(family string) string {
	if family == "v6" {
		return "AAAA"
	}
	return "A"
}
