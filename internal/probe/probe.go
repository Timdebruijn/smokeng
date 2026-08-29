// Package probe contains the scheduler and the ICMP engine (DESIGN.md §5):
// unprivileged datagram ICMP sockets per (address family, DSCP) with a
// flagged raw-socket fallback, kernel TX+RX timestamping on Linux with a
// flagged userspace fallback, wall-clock-aligned interval buckets with
// deterministic per-target phase offsets, burst and spread probe modes, and
// TTL-based DNS refresh via dnscache.
package probe

// TargetSpec is one leaf target with all inheritable settings resolved flat:
// the boundary between inheritance resolution (internal/tree) and the engine,
// which knows nothing about the tree. Host may be a hostname or a literal IP.
type TargetSpec struct {
	TargetID   int64
	Host       string
	Family     string // "v4" | "v6"
	IntervalS  int
	Pings      int
	Mode       string // "burst" | "spread"
	BurstGapMS int
	TimeoutMS  int
	PacketSize int
	DSCP       int
	// TraceIntervalS is how often to discover the path; 0 disables it.
	TraceIntervalS int
	// ProbeType is what the N probes of an interval are (DESIGN.md §3.2b).
	ProbeType string
	ProbePort int
	DNSQuery  string
	DNSRRType string
	HTTPPath  string
}
