package config

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/pelletier/go-toml/v2"

	"github.com/timdebruijn/smokeng/internal/alert"
	"github.com/timdebruijn/smokeng/internal/store"
	"github.com/timdebruijn/smokeng/internal/tree"
)

const sample = `
[defaults]
interval_s = 30

[targets."Production"]
title = "Prod"

[targets."Production/cf-v4"]
host = "1.1.1.1"
address_family = "v4"
pings_per_interval = 40

[targets."Lab/gw-v6"]
host = "2001:db8::1"
address_family = "v6"
probe_mode = "spread"
`

func open(t *testing.T) *store.SQLite {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "cfg.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func mustTree(t *testing.T, s store.Store) (*tree.Tree, map[string]tree.Target) {
	t.Helper()
	targets, err := s.ListTargets(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	tr, err := tree.New(targets)
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]tree.Target{}
	for _, n := range targets {
		p, err := tr.Path(n.ID)
		if err != nil {
			t.Fatal(err)
		}
		byPath[p] = n
	}
	return tr, byPath
}

func TestImport(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	sum, err := Import(ctx, s, []byte(sample), false)
	if err != nil {
		t.Fatal(err)
	}
	// Lab is auto-created as a group: 4 created (Production, Production/cf-v4,
	// Lab, Lab/gw-v6), 0 updated.
	if sum.Created != 4 || sum.Updated != 0 || sum.Disabled != 0 || sum.Deleted != 0 {
		t.Fatalf("summary = %+v", sum)
	}

	tr, byPath := mustTree(t, s)

	// Defaults landed on the root; unspecified root defaults survive.
	root := byPath["/"]
	if *root.Settings.IntervalS != 30 || *root.Settings.PingsPerInterval != 20 {
		t.Fatalf("root settings = %+v", root.Settings)
	}

	// Auto-created group.
	lab, ok := byPath["/Lab"]
	if !ok || lab.Host != nil || !lab.Enabled {
		t.Fatalf("Lab group = %+v", lab)
	}

	// Inheritance provenance: cf-v4 inherits interval from root, overrides pings.
	cf := byPath["/Production/cf-v4"]
	res, err := tr.Resolve(cf.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.IntervalS.Effective != 30 || res.IntervalS.Source.Path != "/" {
		t.Fatalf("cf interval = %+v", res.IntervalS)
	}
	if res.PingsPerInterval.Effective != 40 || res.PingsPerInterval.Source.Path != "/Production/cf-v4" {
		t.Fatalf("cf pings = %+v", res.PingsPerInterval)
	}
}

func TestImportIdempotentAndRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	if _, err := Import(ctx, s, []byte(sample), false); err != nil {
		t.Fatal(err)
	}
	exported, err := Export(ctx, s)
	if err != nil {
		t.Fatal(err)
	}

	// Importing the export into a fresh database yields a semantically
	// identical export.
	s2 := open(t)
	if _, err := Import(ctx, s2, exported, false); err != nil {
		t.Fatalf("re-import of export: %v\n%s", err, exported)
	}
	exported2, err := Export(ctx, s2)
	if err != nil {
		t.Fatal(err)
	}
	var f1, f2 File
	if err := toml.Unmarshal(exported, &f1); err != nil {
		t.Fatal(err)
	}
	if err := toml.Unmarshal(exported2, &f2); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f1, f2) {
		t.Fatalf("export not stable across round trip:\n%s\n---\n%s", exported, exported2)
	}

	// Re-importing the same file into the same database changes nothing.
	sum, err := Import(ctx, s, []byte(sample), false)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Created != 0 || sum.Disabled != 0 || sum.Deleted != 0 {
		t.Fatalf("second import summary = %+v", sum)
	}
}

func TestAbsenceDisablesAndPruneDeletes(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	if _, err := Import(ctx, s, []byte(sample), false); err != nil {
		t.Fatal(err)
	}

	smaller := `
[targets."Production/cf-v4"]
host = "1.1.1.1"
address_family = "v4"
`
	sum, err := Import(ctx, s, []byte(smaller), false)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Disabled != 2 { // Lab and Lab/gw-v6
		t.Fatalf("summary = %+v, want 2 disabled", sum)
	}
	_, byPath := mustTree(t, s)
	if byPath["/Lab/gw-v6"].Enabled || byPath["/Lab"].Enabled {
		t.Fatal("absent targets not disabled")
	}
	if !byPath["/Production"].Enabled { // still an ancestor of a file entry
		t.Fatal("kept ancestor was disabled")
	}

	sum, err = Import(ctx, s, []byte(smaller), true)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Deleted != 2 {
		t.Fatalf("summary = %+v, want 2 deleted", sum)
	}
	_, byPath = mustTree(t, s)
	if _, ok := byPath["/Lab"]; ok {
		t.Fatal("pruned target still present")
	}
}

func TestImportRejectsInvalid(t *testing.T) {
	ctx := context.Background()
	for name, body := range map[string]string{
		"host without family": "[targets.\"x\"]\nhost = \"1.1.1.1\"\n",
		"bad family":          "[targets.\"x\"]\nhost = \"1.1.1.1\"\naddress_family = \"any\"\n",
		"bad probe mode":      "[targets.\"x\"]\nhost = \"1.1.1.1\"\naddress_family = \"v4\"\nprobe_mode = \"warp\"\n",
		"bad path":            "[targets.\"/x\"]\nhost = \"1.1.1.1\"\naddress_family = \"v4\"\n",
		"bad dscp":            "[targets.\"x\"]\nhost = \"1.1.1.1\"\naddress_family = \"v4\"\ndscp = 99\n",
	} {
		s := open(t)
		if _, err := Import(ctx, s, []byte(body), false); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

const withAlerts = `
[defaults]
interval_s = 30

[default_alerts."any loss"]
metric = "loss"
op = ">"
threshold = 5

[targets."Production/cf-v4"]
host = "1.1.1.1"
address_family = "v4"

[targets."Production/cf-v4".alerts."slow"]
metric = "p95"
op = ">"
threshold = 100
for_intervals = 5
clear_intervals = 2
`

// Alert rules are configuration too. A file that carried only half of it
// would be a trap for anyone managing this from a repository.
func TestAlertRulesImportAndRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	sum, err := Import(ctx, s, []byte(withAlerts), false)
	if err != nil {
		t.Fatal(err)
	}
	if sum.RulesCreated != 2 {
		t.Fatalf("created %d rules, want 2 (%+v)", sum.RulesCreated, sum)
	}

	rules, err := s.ListAlertRules(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]alert.Rule{}
	for _, r := range rules {
		byName[r.Name] = r
	}
	// The root rule landed on the root, so it covers everything.
	if r := byName["any loss"]; r.TargetID != 1 || r.Metric != alert.MetricLoss || r.Threshold != 5 {
		t.Errorf("root rule = %+v", r)
	}
	// Omitted hysteresis becomes the default rather than "fire immediately".
	if r := byName["any loss"]; r.For != 3 || r.ClearFor != 3 {
		t.Errorf("default hysteresis = for %d, clear %d; want 3 and 3", r.For, r.ClearFor)
	}
	if r := byName["slow"]; r.Metric != alert.MetricP95 || r.For != 5 || r.ClearFor != 2 {
		t.Errorf("node rule = %+v", r)
	}

	// Export must carry them back out, or the round trip silently loses them.
	exported, err := Export(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	s2 := open(t)
	if _, err := Import(ctx, s2, exported, false); err != nil {
		t.Fatalf("re-import: %v\n%s", err, exported)
	}
	again, err := s2.ListAlertRules(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 2 {
		t.Fatalf("round trip kept %d rules, want 2:\n%s", len(again), exported)
	}
	for _, r := range again {
		orig, ok := byName[r.Name]
		if !ok {
			t.Errorf("unexpected rule %q after round trip", r.Name)
			continue
		}
		if r.Metric != orig.Metric || r.Op != orig.Op || r.Threshold != orig.Threshold ||
			r.For != orig.For || r.ClearFor != orig.ClearFor || r.Enabled != orig.Enabled {
			t.Errorf("rule %q changed across the round trip: %+v vs %+v", r.Name, r, orig)
		}
	}
}

// Absence disables rather than deletes, exactly as it does for targets, so an
// import of a file that omits alerts is recoverable instead of destructive.
func TestAlertRuleAbsenceDisablesAndPruneDeletes(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	if _, err := Import(ctx, s, []byte(withAlerts), false); err != nil {
		t.Fatal(err)
	}

	onlyTargets := `
[targets."Production/cf-v4"]
host = "1.1.1.1"
address_family = "v4"
`
	sum, err := Import(ctx, s, []byte(onlyTargets), false)
	if err != nil {
		t.Fatal(err)
	}
	if sum.RulesDisabled != 2 {
		t.Fatalf("disabled %d rules, want 2 (%+v)", sum.RulesDisabled, sum)
	}
	rules, err := s.ListAlertRules(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 2 {
		t.Fatalf("rules were deleted without --prune: %d remain", len(rules))
	}
	for _, r := range rules {
		if r.Enabled {
			t.Errorf("rule %q is still enabled after being omitted", r.Name)
		}
	}

	sum, err = Import(ctx, s, []byte(onlyTargets), true)
	if err != nil {
		t.Fatal(err)
	}
	if sum.RulesDeleted != 2 {
		t.Fatalf("pruned %d rules, want 2 (%+v)", sum.RulesDeleted, sum)
	}
	if rules, err := s.ListAlertRules(ctx); err != nil || len(rules) != 0 {
		t.Fatalf("rules after prune = %v (err %v)", rules, err)
	}
}

func TestAlertRuleValidationRejects(t *testing.T) {
	ctx := context.Background()
	for name, body := range map[string]string{
		"unknown metric": "[default_alerts.\"x\"]\nmetric = \"jitter\"\nop = \">\"\nthreshold = 1\n",
		"unknown op":     "[default_alerts.\"x\"]\nmetric = \"loss\"\nop = \"~\"\nthreshold = 1\n",
		"loss over 100":  "[default_alerts.\"x\"]\nmetric = \"loss\"\nop = \">\"\nthreshold = 400\n",
	} {
		s := open(t)
		if _, err := Import(ctx, s, []byte(body), false); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}
