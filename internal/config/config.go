// Package config implements declarative TOML import/export of the target
// tree (DESIGN.md §7.3). The database is the source of truth; TOML exists for
// GitOps and bootstrapping. Paths are slash-separated and key the tables:
//
//	[defaults]                       # the root node's inheritable settings
//	interval_s = 30
//
//	[targets."Production/DNS/cloudflare-v4"]
//	host = "1.1.1.1"
//	address_family = "v4"
//	pings_per_interval = 40          # local override; unset keys mean inherit
//
// Import is a declarative sync: entries are upserted by path (missing
// ancestor groups are created), and targets present in the database but
// absent from the file are disabled — never silently deleted; prune deletes
// them explicitly. Export writes only local values, so export→import
// round-trips exactly.
package config

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2"

	"smokeng/internal/store"
	"smokeng/internal/tree"
)

// Values are the inheritable settings as they appear in TOML; nil = inherit.
type Values struct {
	IntervalS        *int    `toml:"interval_s,omitempty"`
	PingsPerInterval *int    `toml:"pings_per_interval,omitempty"`
	ProbeMode        *string `toml:"probe_mode,omitempty"`
	BurstGapMS       *int    `toml:"burst_gap_ms,omitempty"`
	TimeoutMS        *int    `toml:"timeout_ms,omitempty"`
	PacketSize       *int    `toml:"packet_size,omitempty"`
	DSCP             *int    `toml:"dscp,omitempty"`
	Agents           *string `toml:"agents,omitempty"`
}

// Entry is one target table, keyed by its path. A table without host is a
// group node.
type Entry struct {
	Host          *string `toml:"host,omitempty"`
	AddressFamily *string `toml:"address_family,omitempty"`
	Title         *string `toml:"title,omitempty"`
	Notes         *string `toml:"notes,omitempty"`
	Hidden        bool    `toml:"hidden,omitempty"`
	Disabled      bool    `toml:"disabled,omitempty"`
	SortOrder     int     `toml:"sort_order,omitempty"`
	Values
}

// File is the complete on-disk form.
type File struct {
	Defaults Values           `toml:"defaults,omitempty"`
	Targets  map[string]Entry `toml:"targets,omitempty"`
}

// Summary reports what an import changed.
type Summary struct {
	Created, Updated, Disabled, Deleted int
}

func (s Summary) String() string {
	return fmt.Sprintf("%d created, %d updated, %d disabled, %d deleted",
		s.Created, s.Updated, s.Disabled, s.Deleted)
}

// Import applies a TOML file to the store as a declarative sync. The
// resulting tree is fully validated before anything is written.
func Import(ctx context.Context, st store.Store, data []byte, prune bool) (Summary, error) {
	var sum Summary
	var f File
	if err := toml.Unmarshal(data, &f); err != nil {
		return sum, fmt.Errorf("config: parse: %w", err)
	}
	for p, e := range f.Targets {
		if err := validatePath(p); err != nil {
			return sum, err
		}
		if err := validateEntry(p, e); err != nil {
			return sum, err
		}
	}

	current, err := st.ListTargets(ctx)
	if err != nil {
		return sum, err
	}
	tr, err := tree.New(current)
	if err != nil {
		return sum, fmt.Errorf("config: existing tree invalid: %w", err)
	}

	// Working copies keyed by path ("" = root, no leading slash otherwise).
	nodes := map[string]*tree.Target{}
	var root *tree.Target
	for i := range current {
		n := current[i] // copy
		p, err := tr.Path(n.ID)
		if err != nil {
			return sum, err
		}
		key := strings.TrimPrefix(p, "/")
		nodes[key] = &n
		if n.ParentID == nil {
			root = &n
			nodes[""] = &n
		}
	}

	// 1. Defaults onto the root: only overwrite provided keys — the root's
	// inheritable settings can never become NULL.
	overlayValues(&root.Settings, f.Defaults)

	// 2. Upsert entries, creating missing ancestor groups, parents first.
	paths := make([]string, 0, len(f.Targets))
	for p := range f.Targets {
		paths = append(paths, p)
	}
	sort.Slice(paths, func(i, j int) bool {
		di, dj := strings.Count(paths[i], "/"), strings.Count(paths[j], "/")
		if di != dj {
			return di < dj
		}
		return paths[i] < paths[j]
	})
	created := map[string]bool{}
	ensure := func(path string) *tree.Target {
		if n, ok := nodes[path]; ok {
			return n
		}
		n := &tree.Target{Name: path[strings.LastIndex(path, "/")+1:], Enabled: true}
		nodes[path] = n
		created[path] = true
		return n
	}
	for _, p := range paths {
		for _, anc := range ancestors(p) {
			ensure(anc)
		}
		e := f.Targets[p]
		n := ensure(p)
		if !created[p] {
			sum.Updated++
		}
		n.Host = e.Host
		n.AddressFamily = e.AddressFamily
		n.Title = e.Title
		n.Notes = e.Notes
		n.Hidden = e.Hidden
		n.Enabled = !e.Disabled
		n.SortOrder = e.SortOrder
		n.Settings = tree.Settings{
			IntervalS:        e.IntervalS,
			PingsPerInterval: e.PingsPerInterval,
			ProbeMode:        e.ProbeMode,
			BurstGapMS:       e.BurstGapMS,
			TimeoutMS:        e.TimeoutMS,
			PacketSize:       e.PacketSize,
			DSCP:             e.DSCP,
			Agents:           e.Agents,
		}
	}
	sum.Created = len(created)

	// 3. Absence: a pre-existing target neither in the file nor an ancestor
	// of a file entry is disabled, or deleted with prune.
	keep := map[string]bool{"": true}
	for p := range f.Targets {
		keep[p] = true
		for _, anc := range ancestors(p) {
			keep[anc] = true
		}
	}
	var deletePaths []string
	for i := range current {
		if current[i].ParentID == nil {
			continue
		}
		p, _ := tr.Path(current[i].ID)
		key := strings.TrimPrefix(p, "/")
		if keep[key] {
			continue
		}
		if prune {
			deletePaths = append(deletePaths, key)
		} else if nodes[key].Enabled {
			nodes[key].Enabled = false
			sum.Disabled++
		}
	}
	// Children before parents.
	sort.Slice(deletePaths, func(i, j int) bool {
		return strings.Count(deletePaths[i], "/") > strings.Count(deletePaths[j], "/")
	})
	deleted := map[string]bool{}
	for _, p := range deletePaths {
		deleted[p] = true
	}

	// 4. Validate the planned tree before touching the database. New nodes
	// temporarily get synthetic negative ids for validation only; ParentID is
	// wired from the path structure.
	synth := map[string]bool{}
	nextSynth := int64(-1)
	for p, n := range nodes {
		if !deleted[p] && n.ID == 0 {
			n.ID = nextSynth
			nextSynth--
			synth[p] = true
		}
	}
	planned := make([]tree.Target, 0, len(nodes))
	for p, n := range nodes {
		if deleted[p] {
			continue
		}
		c := *n
		if p == "" {
			c.ParentID = nil
		} else {
			pid := nodes[parentPath(p)].ID
			c.ParentID = &pid
		}
		planned = append(planned, c)
	}
	if _, err := tree.New(planned); err != nil {
		return sum, fmt.Errorf("config: resulting tree invalid: %w", err)
	}

	// 5. Write, parents first so real ids exist before their children need
	// them; synthetic ids are cleared so the store assigns real ones.
	order := make([]string, 0, len(nodes))
	for p := range nodes {
		if !deleted[p] {
			order = append(order, p)
		}
	}
	sort.Slice(order, func(i, j int) bool {
		di, dj := depth(order[i]), depth(order[j])
		if di != dj {
			return di < dj
		}
		return order[i] < order[j]
	})
	for _, p := range order {
		n := nodes[p]
		if synth[p] {
			n.ID = 0
		}
		if p != "" {
			pid := nodes[parentPath(p)].ID
			n.ParentID = &pid
		}
		if err := st.UpsertTarget(ctx, n); err != nil {
			return sum, fmt.Errorf("config: upsert %q: %w", p, err)
		}
	}
	for _, p := range deletePaths {
		if err := st.DeleteTarget(ctx, nodes[p].ID); err != nil {
			return sum, fmt.Errorf("config: delete %q: %w", p, err)
		}
		sum.Deleted++
	}
	return sum, nil
}

// Export renders the current tree as TOML: the root's settings as [defaults],
// every other node as a path-keyed table carrying only its local values.
func Export(ctx context.Context, st store.Store) ([]byte, error) {
	current, err := st.ListTargets(ctx)
	if err != nil {
		return nil, err
	}
	tr, err := tree.New(current)
	if err != nil {
		return nil, err
	}
	f := File{Targets: map[string]Entry{}}
	for i := range current {
		n := &current[i]
		if n.ParentID == nil {
			f.Defaults = valuesFrom(n.Settings)
			continue
		}
		p, err := tr.Path(n.ID)
		if err != nil {
			return nil, err
		}
		f.Targets[strings.TrimPrefix(p, "/")] = Entry{
			Host:          n.Host,
			AddressFamily: n.AddressFamily,
			Title:         n.Title,
			Notes:         n.Notes,
			Hidden:        n.Hidden,
			Disabled:      !n.Enabled,
			SortOrder:     n.SortOrder,
			Values:        valuesFrom(n.Settings),
		}
	}
	return toml.Marshal(f)
}

func valuesFrom(s tree.Settings) Values {
	return Values{
		IntervalS:        s.IntervalS,
		PingsPerInterval: s.PingsPerInterval,
		ProbeMode:        s.ProbeMode,
		BurstGapMS:       s.BurstGapMS,
		TimeoutMS:        s.TimeoutMS,
		PacketSize:       s.PacketSize,
		DSCP:             s.DSCP,
		Agents:           s.Agents,
	}
}

func overlayValues(dst *tree.Settings, v Values) {
	if v.IntervalS != nil {
		dst.IntervalS = v.IntervalS
	}
	if v.PingsPerInterval != nil {
		dst.PingsPerInterval = v.PingsPerInterval
	}
	if v.ProbeMode != nil {
		dst.ProbeMode = v.ProbeMode
	}
	if v.BurstGapMS != nil {
		dst.BurstGapMS = v.BurstGapMS
	}
	if v.TimeoutMS != nil {
		dst.TimeoutMS = v.TimeoutMS
	}
	if v.PacketSize != nil {
		dst.PacketSize = v.PacketSize
	}
	if v.DSCP != nil {
		dst.DSCP = v.DSCP
	}
	if v.Agents != nil {
		dst.Agents = v.Agents
	}
}

func validatePath(p string) error {
	if p == "" || strings.HasPrefix(p, "/") || strings.HasSuffix(p, "/") ||
		strings.Contains(p, "//") {
		return fmt.Errorf("config: invalid target path %q", p)
	}
	return nil
}

func validateEntry(p string, e Entry) error {
	if (e.Host == nil) != (e.AddressFamily == nil) {
		return fmt.Errorf("config: %q: host and address_family must be set together", p)
	}
	if e.AddressFamily != nil && *e.AddressFamily != "v4" && *e.AddressFamily != "v6" {
		return fmt.Errorf("config: %q: address_family must be v4 or v6", p)
	}
	if e.ProbeMode != nil && *e.ProbeMode != "burst" && *e.ProbeMode != "spread" {
		return fmt.Errorf("config: %q: probe_mode must be burst or spread", p)
	}
	if e.IntervalS != nil && *e.IntervalS <= 0 {
		return fmt.Errorf("config: %q: interval_s must be positive", p)
	}
	if e.PingsPerInterval != nil && *e.PingsPerInterval <= 0 {
		return fmt.Errorf("config: %q: pings_per_interval must be positive", p)
	}
	if e.DSCP != nil && (*e.DSCP < 0 || *e.DSCP > 63) {
		return fmt.Errorf("config: %q: dscp must be 0..63", p)
	}
	return nil
}

func depth(p string) int {
	if p == "" {
		return 0
	}
	return strings.Count(p, "/") + 1
}

func parentPath(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[:i]
	}
	return ""
}

// ancestors returns the proper ancestor paths of p, excluding the root.
func ancestors(p string) []string {
	var out []string
	for cur := parentPath(p); cur != ""; cur = parentPath(cur) {
		out = append(out, cur)
	}
	return out
}
