package config

import (
	"context"
	"crypto/ed25519"
	"path/filepath"
	"reflect"
	"strings"
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

// An import that changes nothing must report that it changed nothing.
// Anything driving this from CI or config management — Ansible, a GitOps
// pipeline — decides whether it made a change from these counters, and a
// summary that claims six updates on every run makes the whole play a lie.
func TestReimportReportsNoChange(t *testing.T) {
	ctx := context.Background()
	s := open(t)

	first, err := Import(ctx, s, []byte(sample), false)
	if err != nil {
		t.Fatal(err)
	}
	if first.Created == 0 {
		t.Fatalf("the first import created nothing: %s", first)
	}

	again, err := Import(ctx, s, []byte(sample), false)
	if err != nil {
		t.Fatal(err)
	}
	if again.Created != 0 || again.Updated != 0 || again.Disabled != 0 || again.Deleted != 0 ||
		again.RulesCreated != 0 || again.RulesUpdated != 0 ||
		again.RulesDisabled != 0 || again.RulesDeleted != 0 {
		t.Fatalf("re-importing an unchanged file reported work:\n%s", again)
	}
}

// …but a real edit must still be reported, or the counters would be useless
// in the other direction.
func TestReimportReportsAnActualChange(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	if _, err := Import(ctx, s, []byte(sample), false); err != nil {
		t.Fatal(err)
	}

	edited := strings.Replace(sample, "pings_per_interval = 40", "pings_per_interval = 41", 1)
	if edited == sample {
		t.Fatal("the fixture no longer contains the value this test edits")
	}
	got, err := Import(ctx, s, []byte(edited), false)
	if err != nil {
		t.Fatal(err)
	}
	if got.Updated != 1 {
		t.Fatalf("changing one target reported %d updates:\n%s", got.Updated, got)
	}
}

const agentSample = `
[targets."Edge"]
agents = ["local", "ams-01"]

[targets."Edge/cf-v4"]
host = "1.1.1.1"
address_family = "v4"
`

// An `agents` entry that names nothing must be refused. The failure it would
// otherwise cause is silent — nobody measures the target, and the empty graph
// is indistinguishable from one that is measured and never answers.
func TestImportRejectsUnknownAgent(t *testing.T) {
	ctx := context.Background()
	s := open(t)

	_, err := Import(ctx, s, []byte(agentSample), false)
	if err == nil {
		t.Fatal("an agents list naming an unenrolled agent was accepted")
	}
	// The rejection has to be actionable: which name, and what does exist.
	for _, want := range []string{"ams-01", "enrolled agents are", "local"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q:\n%v", want, err)
		}
	}
}

func TestImportAcceptsEnrolledAgent(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddAgent(ctx, "ams-01", pub); err != nil {
		t.Fatal(err)
	}
	if _, err := Import(ctx, s, []byte(agentSample), false); err != nil {
		t.Fatalf("an enrolled agent was refused: %v", err)
	}
}

// The bootstrap case: the tree lands before the agents that will serve it.
func TestAllowUnknownAgentsDowngradesToAWarning(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	sum, err := Import(ctx, s, []byte(agentSample), false, AllowUnknownAgents())
	if err != nil {
		t.Fatalf("with AllowUnknownAgents: %v", err)
	}
	if len(sum.Warnings) == 0 {
		t.Fatal("the unknown agent passed without even a warning")
	}
	if !strings.Contains(strings.Join(sum.Warnings, "\n"), "ams-01") {
		t.Errorf("warning does not name the agent: %v", sum.Warnings)
	}
}

// Both spellings must mean the same thing: configurations written before the
// array form existed are still out there.
func TestAgentListAcceptsBothSpellings(t *testing.T) {
	ctx := context.Background()
	arrayForm := `
[defaults]
agents = ["local"]

[targets."a"]
host = "1.1.1.1"
address_family = "v4"
`
	stringForm := strings.Replace(arrayForm, `agents = ["local"]`, `agents = "local"`, 1)

	s1, s2 := open(t), open(t)
	if _, err := Import(ctx, s1, []byte(arrayForm), false); err != nil {
		t.Fatalf("array form: %v", err)
	}
	if _, err := Import(ctx, s2, []byte(stringForm), false); err != nil {
		t.Fatalf("string form: %v", err)
	}
	e1, err := Export(ctx, s1)
	if err != nil {
		t.Fatal(err)
	}
	e2, err := Export(ctx, s2)
	if err != nil {
		t.Fatal(err)
	}
	if string(e1) != string(e2) {
		t.Errorf("the two spellings exported differently:\n--- array ---\n%s\n--- string ---\n%s", e1, e2)
	}
	// And an export writes the array form, which is what the format wants.
	// The quoting style is go-toml's business; the brackets are the point.
	if !strings.Contains(string(e1), "agents = [") {
		t.Errorf("export did not use the array form:\n%s", e1)
	}
}

const grantSample = `
[targets."Klanten"]
title = "Klanten"

[targets."Klanten/GemeenteX"]
host = "198.51.100.1"
address_family = "v4"

[[grants]]
group = "gemeente-x"
path = "Klanten/GemeenteX"
role = "viewer"
`

func TestGrantsImportAndRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := open(t)

	sum, err := Import(ctx, s, []byte(grantSample), false)
	if err != nil {
		t.Fatal(err)
	}
	if sum.GrantsCreated != 1 {
		t.Fatalf("created %d grants, want 1: %s", sum.GrantsCreated, sum)
	}
	got, err := s.ListGrants(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Group != "gemeente-x" || got[0].Role != "viewer" {
		t.Fatalf("stored grants = %+v", got)
	}

	// Re-importing the same file changes nothing, and the export re-imports
	// into an identical state.
	again, err := Import(ctx, s, []byte(grantSample), false)
	if err != nil {
		t.Fatal(err)
	}
	if again.GrantsCreated+again.GrantsUpdated+again.GrantsRemoved != 0 {
		t.Errorf("re-import reported grant work: %s", again)
	}
	exported, err := Export(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(exported), "gemeente-x") {
		t.Errorf("export lost the grants:\n%s", exported)
	}
	s2 := open(t)
	if _, err := Import(ctx, s2, exported, false); err != nil {
		t.Fatalf("re-import of export: %v\n%s", err, exported)
	}
	round, err := s2.ListGrants(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(round) != 1 || round[0].Group != got[0].Group || round[0].Role != got[0].Role {
		t.Errorf("round trip changed the grants: %+v -> %+v", got, round)
	}
}

// Absence removes a grant, unlike a target, which is disabled. A target's
// measurements are the product; a grant holds no data, and the dangerous
// direction for authorisation is the one that leaves stale access in place.
func TestAbsentGrantIsRemoved(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	if _, err := Import(ctx, s, []byte(grantSample), false); err != nil {
		t.Fatal(err)
	}
	withoutGrant := strings.Split(grantSample, "[[grants]]")[0] + "\n[[grants]]\ngroup = \"other\"\npath = \"Klanten\"\nrole = \"editor\"\n"
	sum, err := Import(ctx, s, []byte(withoutGrant), false)
	if err != nil {
		t.Fatal(err)
	}
	if sum.GrantsRemoved != 1 {
		t.Errorf("removed %d grants, want 1: %s", sum.GrantsRemoved, sum)
	}
	got, err := s.ListGrants(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Group != "other" {
		t.Errorf("grants after the second import = %+v", got)
	}
}

// A file that never mentions grants must leave them alone: importing a
// targets-only file is not a request to revoke everyone's access.
func TestFileWithoutGrantsLeavesThemAlone(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	if _, err := Import(ctx, s, []byte(grantSample), false); err != nil {
		t.Fatal(err)
	}
	targetsOnly := strings.Split(grantSample, "[[grants]]")[0]
	if _, err := Import(ctx, s, []byte(targetsOnly), false); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListGrants(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("a targets-only import revoked access: %d grants left", len(got))
	}
}

func TestGrantRejectsUnknownPathAndRole(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	bad := strings.Replace(grantSample, `path = "Klanten/GemeenteX"`, `path = "Klanten/Nope"`, 1)
	if _, err := Import(ctx, s, []byte(bad), false); err == nil {
		t.Error("a grant on a nonexistent path was accepted")
	}
	bad = strings.Replace(grantSample, `role = "viewer"`, `role = "admin"`, 1)
	if _, err := Import(ctx, s, []byte(bad), false); err == nil {
		t.Error("a grant of the global admin role was accepted")
	}
}

// A key smokeng does not know is a mistake. Accepting it quietly is how
// `probe_type = "dns"` ends up in a file, changes nothing, and leaves the
// operator looking at ICMP measurements wondering why the resolver looks fine.
func TestUnknownSettingIsRefusedAndNamed(t *testing.T) {
	ctx := context.Background()
	s := open(t)

	bad := `
[targets."a"]
host = "1.1.1.1"
address_family = "v4"
dns_rrtype = "A"
`
	_, err := Import(ctx, s, []byte(bad), false)
	if err == nil {
		t.Fatal("a misspelled setting was accepted")
	}
	// The error has to say which key, or it is a puzzle rather than a report.
	if !strings.Contains(err.Error(), "dns_rrtype") {
		t.Errorf("error does not name the offending key:\n%v", err)
	}
}

// The probe type and its settings have to survive a round trip, or a tree
// exported and re-imported quietly reverts to ICMP.
func TestProbeSettingsRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := open(t)

	src := `
[targets."DNS/quad9"]
host = "9.9.9.9"
address_family = "v4"
probe_type = "dns"
probe_port = 5353
dns_query = "example.org"
dns_rr_type = "AAAA"
`
	if _, err := Import(ctx, s, []byte(src), false); err != nil {
		t.Fatal(err)
	}
	exported, err := Export(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"probe_type = 'dns'", "probe_port = 5353",
		"dns_query = 'example.org'", "dns_rr_type = 'AAAA'"} {
		if !strings.Contains(string(exported), want) {
			t.Errorf("export lost %q:\n%s", want, exported)
		}
	}
	s2 := open(t)
	if _, err := Import(ctx, s2, exported, false); err != nil {
		t.Fatalf("re-import of export: %v", err)
	}
	targets, err := s2.ListTargets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, tg := range targets {
		if tg.Host == nil {
			continue
		}
		if tg.Settings.ProbeType == nil || *tg.Settings.ProbeType != "dns" {
			t.Errorf("probe_type did not survive the round trip: %v", tg.Settings.ProbeType)
		}
	}
}

// H1 regression: a boolean default in [defaults] must survive an import.
//
// overlayValues copies every inheritable default from the file onto the root
// row; tls_skip_verify was the one field it omitted, so setting it in
// [defaults] and importing silently changed nothing — a security-relevant
// setting the operator believed they had turned on. This also exercises the
// full round-trip, since an exported-then-reimported file must be stable.
func TestImportDefaultsTLSSkipVerify(t *testing.T) {
	ctx := context.Background()
	s := open(t)

	const cfg = `
[defaults]
interval_s = 60
pings_per_interval = 20
probe_mode = "burst"
burst_gap_ms = 10
tls_skip_verify = true

[targets."Svc"]
host = "192.0.2.9"
address_family = "v4"
probe_type = "https"
`
	if _, err := Import(ctx, s, []byte(cfg), false); err != nil {
		t.Fatal(err)
	}

	_, byPath := mustTree(t, s)
	root := byPath["/"]
	if root.Settings.TLSSkipVerify == nil || !*root.Settings.TLSSkipVerify {
		t.Fatal("[defaults] tls_skip_verify = true was dropped on import; the root still verifies")
	}

	// And the leaf inherits it, which is the whole point of setting it on the
	// root — the operator's intent was every https target under it.
	tr, _ := mustTree(t, s)
	res, err := tr.Resolve(byPath["/Svc"].ID)
	if err != nil {
		t.Fatal(err)
	}
	if !res.TLSSkipVerify.Effective {
		t.Fatal("the https leaf did not inherit tls_skip_verify from the root")
	}

	// Round-trip: export and re-import must preserve it.
	data, err := Export(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	s2 := open(t)
	if _, err := Import(ctx, s2, data, false); err != nil {
		t.Fatalf("re-import of exported config: %v", err)
	}
	_, byPath2 := mustTree(t, s2)
	if v := byPath2["/"].Settings.TLSSkipVerify; v == nil || !*v {
		t.Fatal("tls_skip_verify did not survive an export/import round-trip")
	}
}
