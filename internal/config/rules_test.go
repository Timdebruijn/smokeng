package config

import (
	"context"
	"strings"
	"testing"

	"github.com/timdebruijn/smokeng/internal/alert"
)

// Parse is import's strict decode on its own: a misspelt key must still be an
// error naming it, or a replay would test a rule the import would refuse.
func TestParseIsStrict(t *testing.T) {
	if _, err := Parse([]byte("[defaults]\nintervall_s = 30\n")); err == nil ||
		!strings.Contains(err.Error(), "intervall_s") {
		t.Fatalf("Parse accepted an unknown key, or did not name it: %v", err)
	}
	f, err := Parse([]byte(sample))
	if err != nil || len(f.Targets) != 3 {
		t.Fatalf("Parse(sample) = %d targets, %v", len(f.Targets), err)
	}
}

// Rules from a file land where an import would put them (root for
// default_alerts, the named node otherwise), with import's defaults and
// validation, and nothing is written. An unknown path names itself.
func TestAlertRulesResolveAgainstTheTree(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	if _, err := Import(ctx, s, []byte(sample), false, false); err != nil {
		t.Fatal(err)
	}
	targets, err := s.ListTargets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, byPath := mustTree(t, s)

	f, err := Parse([]byte(`
[default_alerts.loss]
metric = "loss"
op = ">"
threshold = 5

[targets."Production".alerts.slow]
metric = "p95"
op = ">"
threshold = 80
for_intervals = 5
disabled = true

[targets."Production".alerts.smoke]
metric = "shape"
op = ">"
threshold = 3
mode = "auto"
baseline = "rolling"

[targets."Lab/gw-v6"]
host = "2001:db8::1"
`))
	if err != nil {
		t.Fatal(err)
	}
	rules, err := AlertRules(f, targets)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 3 {
		t.Fatalf("got %d rules, want 3: %+v", len(rules), rules)
	}
	by := map[string]alert.Rule{}
	for _, r := range rules {
		by[r.Name] = r
		if r.ID != 0 {
			t.Errorf("%s has id %d, want 0", r.Name, r.ID)
		}
	}
	if by["loss"].TargetID != 1 || by["loss"].For != 3 || by["loss"].ClearFor != 3 ||
		by["loss"].Severity != alert.SeverityWarning || !by["loss"].Enabled {
		t.Errorf("loss = %+v, want root node with import's defaults", by["loss"])
	}
	prod := byPath["/Production"].ID
	if by["slow"].TargetID != prod || by["slow"].For != 5 || by["slow"].Enabled {
		t.Errorf("slow = %+v", by["slow"])
	}
	if by["smoke"].TargetID != prod || by["smoke"].Mode != alert.ModeAuto {
		t.Errorf("smoke = %+v", by["smoke"])
	}
	if stored, _ := s.ListAlertRules(ctx); len(stored) != 0 {
		t.Errorf("AlertRules wrote %d rules", len(stored))
	}

	bad, err := Parse([]byte("[targets.\"Nowhere/here\".alerts.x]\nmetric = \"loss\"\nop = \">\"\nthreshold = 1\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AlertRules(bad, targets); err == nil || !strings.Contains(err.Error(), "/Nowhere/here") {
		t.Errorf("unknown path: err = %v, want one naming /Nowhere/here", err)
	}

	invalid, _ := Parse([]byte("[default_alerts.x]\nmetric = \"loss\"\nop = \"~\"\nthreshold = 1\n"))
	if _, err := AlertRules(invalid, targets); err == nil {
		t.Error("an invalid rule was accepted")
	}
}
