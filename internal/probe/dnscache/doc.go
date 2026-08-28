// Package dnscache will provide TTL-respecting hostname resolution
// (DESIGN.md §5.4): direct queries via the system resolver using
// codeberg.org/miekg/dns (net.Resolver hides TTLs), a cache that re-resolves
// asynchronously when the TTL expires (clamped to [30s, 24h]), and a change
// log into the resolutions table. The ping loop never blocks on DNS; it keeps
// the previous address until a new resolution succeeds. address_family
// strictly selects A vs AAAA.
//
// Status: not implemented; lands with the ICMP engine.
package dnscache
