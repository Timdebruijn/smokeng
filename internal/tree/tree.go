// Package tree models the target tree and inheritance resolution with
// provenance (DESIGN.md §4). The tree is an in-memory snapshot of all targets;
// it is rebuilt on any target write (the tree is small, resolution must never
// hit the database on the hot path).
package tree

import (
	"fmt"
	"slices"
	"strings"
)

// Target is one node of the tree. Nil pointer fields in Settings mean the
// node inherits that value from its parent.
type Target struct {
	ID            int64
	ParentID      *int64 // nil = root
	Name          string
	Host          *string // nil for group nodes
	AddressFamily *string // "v4" or "v6"; nil for group nodes
	Title         *string
	Notes         *string
	Hidden        bool
	Enabled       bool
	SortOrder     int
	Settings      Settings
}

// Settings holds the inheritable settings (DESIGN.md §3.1). The root node
// must have every field set; New enforces this.
type Settings struct {
	IntervalS        *int
	PingsPerInterval *int
	ProbeMode        *string // "burst" | "spread"
	BurstGapMS       *int
	TimeoutMS        *int
	PacketSize       *int
	DSCP             *int
	Agents           *string
	// ProbeType is what the N probes of an interval are: icmp, dns, tcp,
	// http, https or irtt (DESIGN.md §3.2b). probe_mode says when they go
	// out; this says what they are.
	ProbeType *string
	// ProbePort is the port the probe talks to, where the type has one.
	ProbePort *int
	// DNSQuery and DNSRRType are what a dns probe asks for; the target's host
	// is the server being asked.
	DNSQuery  *string
	DNSRRType *string
	// HTTPPath is what an http or https probe requests.
	HTTPPath *string
	// TLSSkipVerify turns off certificate verification for an https probe.
	//
	// It exists because the alternative was worse: an internal service on a
	// private PKI read as 100% loss with no way to say otherwise, which is a
	// graph that reports an outage where there is none. Prefer adding the
	// issuing CA with --tls-ca-file, which keeps verification on; this is the
	// escape hatch for when that is not possible.
	TLSSkipVerify *bool
	// TraceIntervalS is how often to discover the path; 0 disables it.
	// Separate from IntervalS because a traceroute costs a round trip per
	// hop, and a path changes on a scale of days rather than seconds.
	TraceIntervalS *int
}

// Source identifies the node an effective value came from.
type Source struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	Path string `json:"path"`
}

// Value is one resolved setting with provenance (DESIGN.md §4.2): the node's
// own value (nil if inherited), the effective value, and where it came from.
type Value[T any] struct {
	Local     *T
	Effective T
	Source    Source
}

// Resolved is the full effective configuration of one node.
type Resolved struct {
	IntervalS        Value[int]
	PingsPerInterval Value[int]
	ProbeMode        Value[string]
	BurstGapMS       Value[int]
	TimeoutMS        Value[int]
	PacketSize       Value[int]
	DSCP             Value[int]
	Agents           Value[string]
	TraceIntervalS   Value[int]
	ProbeType        Value[string]
	ProbePort        Value[int]
	DNSQuery         Value[string]
	DNSRRType        Value[string]
	HTTPPath         Value[string]
	TLSSkipVerify    Value[bool]
}

// Validate checks one node's field-level invariants — the rules that hold
// regardless of where the node sits in the tree. Structural rules (a single
// complete root, no cycles, unique sibling names) are New's job. Both the
// HTTP API and the TOML importer run this, so a target cannot enter the
// database through one door under rules the other would reject.
func (t *Target) Validate() error {
	if strings.TrimSpace(t.Name) == "" && t.ParentID != nil {
		return fmt.Errorf("tree: name is required")
	}
	if strings.Contains(t.Name, "/") {
		return fmt.Errorf("tree: name %q must not contain a slash", t.Name)
	}
	if (t.Host == nil) != (t.AddressFamily == nil) {
		return fmt.Errorf("tree: host and address_family must be set together (a node with neither is a group)")
	}
	if t.AddressFamily != nil && *t.AddressFamily != "v4" && *t.AddressFamily != "v6" {
		return fmt.Errorf("tree: address_family must be v4 or v6, got %q", *t.AddressFamily)
	}
	s := &t.Settings
	if s.ProbeMode != nil && *s.ProbeMode != "burst" && *s.ProbeMode != "spread" {
		return fmt.Errorf("tree: probe_mode must be burst or spread, got %q", *s.ProbeMode)
	}
	if s.IntervalS != nil && *s.IntervalS <= 0 {
		return fmt.Errorf("tree: interval_s must be positive")
	}
	// Bounded above because the measurement wire format carries sent and
	// received as UInt16 (DESIGN.md §7.2). Without this a target could be
	// configured past 65535 and report a count that had silently wrapped.
	if s.PingsPerInterval != nil && (*s.PingsPerInterval <= 0 || *s.PingsPerInterval > 65535) {
		return fmt.Errorf("tree: pings_per_interval must be between 1 and 65535")
	}
	if s.BurstGapMS != nil && *s.BurstGapMS < 0 {
		return fmt.Errorf("tree: burst_gap_ms must not be negative")
	}
	if s.TimeoutMS != nil && *s.TimeoutMS <= 0 {
		return fmt.Errorf("tree: timeout_ms must be positive")
	}
	// The ICMP payload carries a 4-byte magic and an 8-byte token.
	if s.PacketSize != nil && (*s.PacketSize < 12 || *s.PacketSize > 65_000) {
		return fmt.Errorf("tree: packet_size must be between 12 and 65000 bytes")
	}
	if s.DSCP != nil && (*s.DSCP < 0 || *s.DSCP > 63) {
		return fmt.Errorf("tree: dscp must be between 0 and 63")
	}
	// Every type must produce a distribution of N round-trip times per
	// interval (DESIGN.md §3.2b). Anything yielding a single number or an
	// up/down verdict is deliberately not here.
	if s.ProbeType != nil {
		switch *s.ProbeType {
		case "icmp", "dns", "tcp", "http", "https", "irtt":
		default:
			return fmt.Errorf("tree: probe_type must be icmp, dns, tcp, http, https or irtt, got %q", *s.ProbeType)
		}
	}
	if s.ProbePort != nil && (*s.ProbePort < 1 || *s.ProbePort > 65535) {
		return fmt.Errorf("tree: probe_port must be between 1 and 65535")
	}
	if s.DNSRRType != nil {
		switch strings.ToUpper(*s.DNSRRType) {
		case "A", "AAAA", "CNAME", "MX", "NS", "PTR", "SOA", "SRV", "TXT":
		default:
			return fmt.Errorf("tree: dns_rr_type %q is not a record type smokeng asks for", *s.DNSRRType)
		}
	}
	if s.Agents != nil && strings.TrimSpace(*s.Agents) == "" {
		return fmt.Errorf("tree: agents must name at least one agent, or be unset to inherit")
	}
	// 0 disables path discovery, so only a negative value is wrong.
	if s.TraceIntervalS != nil && *s.TraceIntervalS < 0 {
		return fmt.Errorf("tree: trace_interval_s must not be negative (0 disables it)")
	}
	return nil
}

// A burst must fit inside its interval, or bursts overlap the next bucket.
// This needs resolved values, so it is checked after inheritance.
func (r *Resolved) validateTiming() error {
	if r.ProbeMode.Effective == "burst" {
		burstMS := r.PingsPerInterval.Effective * r.BurstGapMS.Effective
		if burstMS >= r.IntervalS.Effective*1000 {
			return fmt.Errorf("tree: %d pings %dms apart (%.1fs) does not fit in a %ds interval",
				r.PingsPerInterval.Effective, r.BurstGapMS.Effective,
				float64(burstMS)/1000, r.IntervalS.Effective)
		}
	}
	return nil
}

// Tree is a validated, indexed snapshot of all targets.
type Tree struct {
	nodes map[int64]*Target
	root  *Target
}

// New builds and validates a Tree: exactly one root, all parents present, no
// cycles, and a root with a complete set of inheritable defaults — the
// invariants that make Resolve total.
func New(targets []Target) (*Tree, error) {
	t := &Tree{nodes: make(map[int64]*Target, len(targets))}
	for i := range targets {
		n := &targets[i]
		if _, dup := t.nodes[n.ID]; dup {
			return nil, fmt.Errorf("tree: duplicate target id %d", n.ID)
		}
		t.nodes[n.ID] = n
		if n.ParentID == nil {
			if t.root != nil {
				return nil, fmt.Errorf("tree: multiple roots (%d and %d)", t.root.ID, n.ID)
			}
			t.root = n
		}
	}
	if t.root == nil {
		return nil, fmt.Errorf("tree: no root node")
	}
	s := t.root.Settings
	if s.IntervalS == nil || s.PingsPerInterval == nil || s.ProbeMode == nil ||
		s.BurstGapMS == nil || s.TimeoutMS == nil || s.PacketSize == nil ||
		s.DSCP == nil || s.Agents == nil || s.TraceIntervalS == nil ||
		s.ProbeType == nil || s.TLSSkipVerify == nil {
		return nil, fmt.Errorf("tree: root %d must set every inheritable default", t.root.ID)
	}
	for _, n := range t.nodes {
		if _, err := t.ancestryOf(n); err != nil {
			return nil, err
		}
		if err := n.Validate(); err != nil {
			return nil, fmt.Errorf("target %d (%s): %w", n.ID, n.Name, err)
		}
	}
	// Sibling names must be unique: the path is the identity used by TOML
	// import/export and by the UI.
	seen := map[string]bool{}
	for _, n := range t.nodes {
		if n.ParentID == nil {
			continue
		}
		key := fmt.Sprintf("%d/%s", *n.ParentID, n.Name)
		if seen[key] {
			return nil, fmt.Errorf("tree: duplicate sibling name %q under target %d", n.Name, *n.ParentID)
		}
		seen[key] = true
	}
	// Timing has to hold for every leaf, using resolved values.
	for _, n := range t.nodes {
		if n.Host == nil {
			continue
		}
		res, err := t.Resolve(n.ID)
		if err != nil {
			return nil, err
		}
		if err := res.validateTiming(); err != nil {
			return nil, fmt.Errorf("target %d (%s): %w", n.ID, n.Name, err)
		}
	}
	return t, nil
}

// ancestryOf walks n up to the root, detecting broken links and cycles.
func (t *Tree) ancestryOf(n *Target) ([]*Target, error) {
	chain := []*Target{n}
	for cur := n; cur.ParentID != nil; {
		next, ok := t.nodes[*cur.ParentID]
		if !ok {
			return nil, fmt.Errorf("tree: target %d has missing parent %d", cur.ID, *cur.ParentID)
		}
		if len(chain) > len(t.nodes) {
			return nil, fmt.Errorf("tree: cycle involving target %d", n.ID)
		}
		chain = append(chain, next)
		cur = next
	}
	return chain, nil
}

// Path returns the node's path, e.g. "/Production/DNS/cloudflare-v4". The
// root's own (empty) name is not a path segment; the root's path is "/".
func (t *Tree) Path(id int64) (string, error) {
	n, ok := t.nodes[id]
	if !ok {
		return "", fmt.Errorf("tree: unknown target id %d", id)
	}
	chain, err := t.ancestryOf(n)
	if err != nil {
		return "", err
	}
	parts := make([]string, 0, len(chain)-1)
	for _, node := range chain[:len(chain)-1] { // drop the root
		parts = append(parts, node.Name)
	}
	slices.Reverse(parts)
	return "/" + strings.Join(parts, "/"), nil
}

// Get returns the node with the given id.
func (t *Tree) Get(id int64) (*Target, bool) {
	n, ok := t.nodes[id]
	return n, ok
}

// Resolve returns the node's full effective configuration with provenance.
func (t *Tree) Resolve(id int64) (Resolved, error) {
	n, ok := t.nodes[id]
	if !ok {
		return Resolved{}, fmt.Errorf("tree: unknown target id %d", id)
	}
	chain, err := t.ancestryOf(n)
	if err != nil {
		return Resolved{}, err
	}
	return Resolved{
		IntervalS:        resolve(t, chain, func(s *Settings) *int { return s.IntervalS }),
		PingsPerInterval: resolve(t, chain, func(s *Settings) *int { return s.PingsPerInterval }),
		ProbeMode:        resolve(t, chain, func(s *Settings) *string { return s.ProbeMode }),
		BurstGapMS:       resolve(t, chain, func(s *Settings) *int { return s.BurstGapMS }),
		TimeoutMS:        resolve(t, chain, func(s *Settings) *int { return s.TimeoutMS }),
		PacketSize:       resolve(t, chain, func(s *Settings) *int { return s.PacketSize }),
		DSCP:             resolve(t, chain, func(s *Settings) *int { return s.DSCP }),
		Agents:           resolve(t, chain, func(s *Settings) *string { return s.Agents }),
		ProbeType:        resolve(t, chain, func(s *Settings) *string { return s.ProbeType }),
		ProbePort:        resolve(t, chain, func(s *Settings) *int { return s.ProbePort }),
		DNSQuery:         resolve(t, chain, func(s *Settings) *string { return s.DNSQuery }),
		DNSRRType:        resolve(t, chain, func(s *Settings) *string { return s.DNSRRType }),
		HTTPPath:         resolve(t, chain, func(s *Settings) *string { return s.HTTPPath }),
		TLSSkipVerify:    resolve(t, chain, func(s *Settings) *bool { return s.TLSSkipVerify }),
		TraceIntervalS:   resolve(t, chain, func(s *Settings) *int { return s.TraceIntervalS }),
	}, nil
}

// resolve finds the first node in the ancestry chain (self first, root last)
// that sets the value. New's root-completeness check guarantees a hit.
func resolve[T any](t *Tree, chain []*Target, get func(*Settings) *T) Value[T] {
	local := get(&chain[0].Settings)
	for _, node := range chain {
		if v := get(&node.Settings); v != nil {
			path, _ := t.Path(node.ID)
			return Value[T]{
				Local:     local,
				Effective: *v,
				Source:    Source{ID: node.ID, Name: node.Name, Path: path},
			}
		}
	}
	// Unreachable after New's validation; return the zero value defensively.
	return Value[T]{Local: local}
}
