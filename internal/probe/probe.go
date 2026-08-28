// Package probe will contain the scheduler and the ICMP engine
// (DESIGN.md §5): datagram ICMP sockets per address family, kernel TX+RX
// timestamping with observable fallback, wall-clock-aligned interval buckets
// with per-target phase offsets, and the burst/spread probe modes.
//
// Status: contract only. TargetSpec fixes the boundary between inheritance
// resolution (internal/tree) and the engine: the engine receives flat,
// fully-resolved specs and knows nothing about the tree.
package probe

// TargetSpec is one leaf target with all inheritable settings resolved.
// Address is a literal IP; DNS resolution and TTL-based refresh are the
// dnscache package's job (DESIGN.md §5.4).
type TargetSpec struct {
	TargetID   int64
	Address    string
	Family     string // "v4" | "v6"
	IntervalS  int
	Pings      int
	Mode       string // "burst" | "spread"
	BurstGapMS int
	TimeoutMS  int
	PacketSize int
	DSCP       int
}
