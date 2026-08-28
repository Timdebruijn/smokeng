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
	Agents           *string // space-separated agent names
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
		s.DSCP == nil || s.Agents == nil {
		return nil, fmt.Errorf("tree: root %d must set every inheritable default", t.root.ID)
	}
	for _, n := range t.nodes {
		if _, err := t.ancestryOf(n); err != nil {
			return nil, err
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
