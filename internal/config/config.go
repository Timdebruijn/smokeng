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
	"reflect"
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2"

	"github.com/timdebruijn/smokeng/internal/alert"
	"github.com/timdebruijn/smokeng/internal/store"
	"github.com/timdebruijn/smokeng/internal/tree"
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
	Agents           any     `toml:"agents,omitempty"`
	TraceIntervalS   *int    `toml:"trace_interval_s,omitempty"`
}

// AlertRule is one alert condition in TOML form, keyed by its name within the
// node it applies to.
type AlertRule struct {
	Metric    string  `toml:"metric"`
	Op        string  `toml:"op"`
	Threshold float64 `toml:"threshold"`
	// Hysteresis. Zero means the default, not "fire immediately": a rule
	// without it would flap, and a config file should not be able to ask for
	// that by omission.
	For      int  `toml:"for_intervals,omitempty"`
	ClearFor int  `toml:"clear_intervals,omitempty"`
	Disabled bool `toml:"disabled,omitempty"`
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
	// Alerts are the rules defined on this node, keyed by name. They apply to
	// this node and everything below it, replacing any inherited set.
	Alerts map[string]AlertRule `toml:"alerts,omitempty"`
}

// File is the complete on-disk form.
type File struct {
	Defaults Values `toml:"defaults,omitempty"`
	// DefaultAlerts are the rules on the root, mirroring how Defaults holds
	// the root's settings.
	DefaultAlerts map[string]AlertRule `toml:"default_alerts,omitempty"`
	Targets       map[string]Entry     `toml:"targets,omitempty"`
}

// Store is the persistence a sync needs: the target tree plus alert rules,
// since a configuration that covered only half of them would be a trap for
// anyone managing this from a repository.
type Store interface {
	store.Store
	ListAlertRules(ctx context.Context) ([]alert.Rule, error)
	UpsertAlertRule(ctx context.Context, r *alert.Rule) error
	DeleteAlertRule(ctx context.Context, id int64) error
}

// Summary reports what an import changed.
type Summary struct {
	Created, Updated, Disabled, Deleted int
	// Rule counts are reported separately: seeing "3 alert rules disabled"
	// is the difference between noticing and discovering it during an
	// incident.
	RulesCreated, RulesUpdated, RulesDisabled, RulesDeleted int
	// Warnings are problems the import chose to carry rather than refuse.
	Warnings []string
}

func (s Summary) String() string {
	out := fmt.Sprintf("targets: %d created, %d updated, %d disabled, %d deleted",
		s.Created, s.Updated, s.Disabled, s.Deleted)
	if s.RulesCreated+s.RulesUpdated+s.RulesDisabled+s.RulesDeleted > 0 {
		out += fmt.Sprintf("\nalert rules: %d created, %d updated, %d disabled, %d deleted",
			s.RulesCreated, s.RulesUpdated, s.RulesDisabled, s.RulesDeleted)
	}
	return out
}

// Import applies a TOML file to the store as a declarative sync. The
// resulting tree is fully validated before anything is written.
// Option adjusts how an import behaves. Options are variadic so the common
// call stays a call and does not need an options struct built for it.
type Option func(*options)

type options struct{ allowUnknownAgents bool }

// AllowUnknownAgents downgrades an `agents` entry that names no enrolled agent
// from an error to a warning, for the case where the tree is meant to land
// before the agents that will serve it. It is opt-in because the common cause
// of an unknown name is a typo, and a typo means nobody measures that target
// (DESIGN.md §4.4).
func AllowUnknownAgents() Option { return func(o *options) { o.allowUnknownAgents = true } }

func Import(ctx context.Context, st Store, data []byte, prune bool, opts ...Option) (Summary, error) {
	var f File
	if err := toml.Unmarshal(data, &f); err != nil {
		return Summary{}, fmt.Errorf("config: parse: %w", err)
	}
	return Apply(ctx, st, f, prune, opts...)
}

// Apply syncs an already-parsed configuration into the store. Both the TOML
// importer and the SmokePing importer land here, so they cannot drift apart
// in how absence, inheritance or validation are handled.
func Apply(ctx context.Context, st Store, f File, prune bool, opts ...Option) (Summary, error) {
	var o options
	for _, fn := range opts {
		fn(&o)
	}
	var sum Summary
	if err := f.normaliseAgentLists(); err != nil {
		return sum, err
	}
	for p, e := range f.Targets {
		if err := validatePath(p); err != nil {
			return sum, err
		}
		if err := validateEntry(p, e); err != nil {
			return sum, err
		}
	}
	// Validate every rule before writing anything, so a typo in one does not
	// leave the tree half-synced.
	for name, r := range f.DefaultAlerts {
		if _, err := toAlertRule(name, r, 0); err != nil {
			return sum, err
		}
	}
	for p, e := range f.Targets {
		for name, r := range e.Alerts {
			if _, err := toAlertRule(name, r, 0); err != nil {
				return sum, fmt.Errorf("config: %q: %w", p, err)
			}
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
		// Snapshot before mutating: an import that changes nothing must say so,
		// or it cannot be run from CI or a config-management tool without
		// reporting a change on every single run. The fields below are all
		// replaced wholesale, so the copy keeps pointing at the old values.
		existed := !created[p]
		var before tree.Target
		if existed {
			before = *n
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
			Agents:           agentStringFrom(e.Agents),
		}
		if existed && !reflect.DeepEqual(before, *n) {
			sum.Updated++
		}
	}
	// Every `agents` entry must name something. A name that matches no enrolled
	// agent is not a smaller mistake than a bad hostname: the target is
	// measured by nobody, and the empty graph that results looks exactly like a
	// target that is measured and never answers.
	known, err := knownAgents(ctx, st)
	if err != nil {
		return sum, err
	}
	declared := map[string]bool{"": true}
	for p := range f.Targets {
		declared[p] = true
	}
	if problems := checkAgentRefs(nodes, declared, known); len(problems) > 0 {
		if !o.allowUnknownAgents {
			return sum, agentRefError(problems, known)
		}
		for _, p := range problems {
			sum.Warnings = append(sum.Warnings, p)
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

	// 6. Alert rules, once every node has a real id to hang them on.
	if err := applyRules(ctx, st, f, nodes, deleted, prune, &sum); err != nil {
		return sum, err
	}
	return sum, nil
}

// applyRules syncs alert rules the same way targets are synced: rules present
// in the file are upserted, and rules absent from it are disabled — or, with
// prune, deleted. Absence meaning "disabled" rather than "gone" mirrors the
// target behaviour exactly, and matters here because importing a file that
// happens to omit alerts should be recoverable, not a silent wipe. The
// summary reports the counts either way.
func applyRules(ctx context.Context, st Store, f File, nodes map[string]*tree.Target,
	deleted map[string]bool, prune bool, sum *Summary) error {
	existing, err := st.ListAlertRules(ctx)
	if err != nil {
		return err
	}
	type ruleKey struct {
		targetID int64
		name     string
	}
	current := map[ruleKey]*alert.Rule{}
	for i := range existing {
		r := &existing[i]
		current[ruleKey{r.TargetID, r.Name}] = r
	}

	// Collect what the file asks for, resolving paths to node ids.
	wanted := map[ruleKey]AlertRule{}
	for name, r := range f.DefaultAlerts {
		wanted[ruleKey{nodes[""].ID, name}] = r
	}
	for p, e := range f.Targets {
		node, ok := nodes[p]
		if !ok || deleted[p] {
			continue
		}
		for name, r := range e.Alerts {
			wanted[ruleKey{node.ID, name}] = r
		}
	}

	for key, spec := range wanted {
		rule, err := toAlertRule(key.name, spec, key.targetID)
		if err != nil {
			return err
		}
		if prev, ok := current[key]; ok {
			rule.ID = prev.ID
			if rule != *prev {
				sum.RulesUpdated++
			}
		} else {
			sum.RulesCreated++
		}
		if err := st.UpsertAlertRule(ctx, &rule); err != nil {
			return fmt.Errorf("config: alert rule %q: %w", key.name, err)
		}
	}

	for key, rule := range current {
		if _, ok := wanted[key]; ok {
			continue
		}
		if prune {
			if err := st.DeleteAlertRule(ctx, rule.ID); err != nil {
				return err
			}
			sum.RulesDeleted++
			continue
		}
		if rule.Enabled {
			rule.Enabled = false
			if err := st.UpsertAlertRule(ctx, rule); err != nil {
				return err
			}
			sum.RulesDisabled++
		}
	}
	return nil
}

// toAlertRule converts the TOML form, applying the hysteresis defaults and
// validating with exactly the rules the API applies.
func toAlertRule(name string, r AlertRule, targetID int64) (alert.Rule, error) {
	out := alert.Rule{
		TargetID:  targetID,
		Name:      name,
		Metric:    alert.Metric(r.Metric),
		Op:        alert.Op(r.Op),
		Threshold: r.Threshold,
		For:       r.For,
		ClearFor:  r.ClearFor,
		Enabled:   !r.Disabled,
	}
	if out.For == 0 {
		out.For = 3
	}
	if out.ClearFor == 0 {
		out.ClearFor = 3
	}
	if err := out.Validate(); err != nil {
		return out, fmt.Errorf("config: alert rule %q: %w", name, err)
	}
	return out, nil
}

// Export renders the current tree as TOML: the root's settings as [defaults],
// every other node as a path-keyed table carrying only its local values.
func Export(ctx context.Context, st Store) ([]byte, error) {
	current, err := st.ListTargets(ctx)
	if err != nil {
		return nil, err
	}
	tr, err := tree.New(current)
	if err != nil {
		return nil, err
	}
	rules, err := st.ListAlertRules(ctx)
	if err != nil {
		return nil, err
	}
	byNode := map[int64]map[string]AlertRule{}
	for _, r := range rules {
		if byNode[r.TargetID] == nil {
			byNode[r.TargetID] = map[string]AlertRule{}
		}
		byNode[r.TargetID][r.Name] = AlertRule{
			Metric: string(r.Metric), Op: string(r.Op), Threshold: r.Threshold,
			For: r.For, ClearFor: r.ClearFor, Disabled: !r.Enabled,
		}
	}

	f := File{Targets: map[string]Entry{}}
	for i := range current {
		n := &current[i]
		if n.ParentID == nil {
			f.Defaults = valuesFrom(n.Settings)
			f.DefaultAlerts = byNode[n.ID]
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
			Alerts:        byNode[n.ID],
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
		Agents:           agentListFrom(s.Agents),
		TraceIntervalS:   s.TraceIntervalS,
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
		dst.Agents = agentStringFrom(v.Agents)
	}
	if v.TraceIntervalS != nil {
		dst.TraceIntervalS = v.TraceIntervalS
	}
}

func validatePath(p string) error {
	if p == "" || strings.HasPrefix(p, "/") || strings.HasSuffix(p, "/") ||
		strings.Contains(p, "//") {
		return fmt.Errorf("config: invalid target path %q", p)
	}
	return nil
}

// validateEntry runs the same field rules the HTTP API applies, so a target
// cannot enter the database through the file that the API would reject.
func validateEntry(p string, e Entry) error {
	dummyParent := int64(1)
	t := tree.Target{
		ParentID:      &dummyParent,
		Name:          p[strings.LastIndex(p, "/")+1:],
		Host:          e.Host,
		AddressFamily: e.AddressFamily,
		Settings: tree.Settings{
			IntervalS:        e.IntervalS,
			PingsPerInterval: e.PingsPerInterval,
			ProbeMode:        e.ProbeMode,
			BurstGapMS:       e.BurstGapMS,
			TimeoutMS:        e.TimeoutMS,
			PacketSize:       e.PacketSize,
			DSCP:             e.DSCP,
			Agents:           agentStringFrom(e.Agents),
			TraceIntervalS:   e.TraceIntervalS,
		},
	}
	if err := t.Validate(); err != nil {
		return fmt.Errorf("config: %q: %w", p, err)
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
