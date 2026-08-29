package config

import (
	"context"
	"strings"
	"testing"
)

const smokepingTargets = `
*** Targets ***

probe = FPing
menu = Top
title = Network Latency Grapher
pings = 15

+ Datacenter
menu = DC
title = Datacenter hosts
remark = Racks 1 through 4,
         second floor

++ router1
menu = router1
title = Core Router 1
host = 10.0.0.1
pings = 20

++ router2
host = 10.0.0.2
alerts = someloss
hide = yes

++ v6gw
probe = FPing6
host = 2001:db8::1

+ Internet

++ google
host = google.com

++ dynamichost
host = DYNAMIC

++ overlay
host = /Datacenter/router1 /Datacenter/router2

++ remote
host = 10.1.0.1
slaves = ams rtm
`

func TestParseSmokePing(t *testing.T) {
	f, warnings, err := ParseSmokePing([]byte(smokepingTargets), false)
	if err != nil {
		t.Fatal(err)
	}

	// Hierarchy became paths; groups exist without hosts.
	for _, want := range []string{"Datacenter", "Datacenter/router1", "Datacenter/router2",
		"Datacenter/v6gw", "Internet", "Internet/google", "Internet/remote"} {
		if _, ok := f.Targets[want]; !ok {
			t.Errorf("missing target %q (got %v)", want, keys(f.Targets))
		}
	}
	if f.Defaults.PingsPerInterval == nil || *f.Defaults.PingsPerInterval != 15 {
		t.Errorf("top-level pings did not become a default: %v", f.Defaults.PingsPerInterval)
	}

	dc := f.Targets["Datacenter"]
	if dc.Host != nil {
		t.Errorf("Datacenter should be a group, got host %v", *dc.Host)
	}
	if dc.Title == nil || *dc.Title != "Datacenter hosts" {
		t.Errorf("title = %v", dc.Title)
	}
	// The continuation line is folded into the remark.
	if dc.Notes == nil || !strings.Contains(*dc.Notes, "second floor") {
		t.Errorf("remark continuation lost: %v", dc.Notes)
	}

	r1 := f.Targets["Datacenter/router1"]
	if r1.AddressFamily == nil || *r1.AddressFamily != "v4" {
		t.Errorf("router1 family = %v", r1.AddressFamily)
	}
	if r1.PingsPerInterval == nil || *r1.PingsPerInterval != 20 {
		t.Errorf("router1 pings = %v", r1.PingsPerInterval)
	}
	if !f.Targets["Datacenter/router2"].Hidden {
		t.Error("hide = yes did not map to hidden")
	}
	// A literal v6 address and the FPing6 probe both imply v6.
	if fam := f.Targets["Datacenter/v6gw"].AddressFamily; fam == nil || *fam != "v6" {
		t.Errorf("v6gw family = %v", fam)
	}
	// A hostname defaults to v4 rather than "whatever resolves".
	if fam := f.Targets["Internet/google"].AddressFamily; fam == nil || *fam != "v4" {
		t.Errorf("google family = %v", fam)
	}
	if ag := agentStringFrom(f.Targets["Internet/remote"].Agents); ag == nil || *ag != "local ams rtm" {
		t.Errorf("remote agents = %v", ag)
	}

	// Everything skipped is reported.
	if _, ok := f.Targets["Internet/dynamichost"]; ok {
		t.Error("DYNAMIC host should not be imported")
	}
	if _, ok := f.Targets["Internet/overlay"]; ok {
		t.Error("multi-host overlay should not be imported")
	}
	joined := strings.Join(warnings, "\n")
	for _, want := range []string{"DYNAMIC", "overlay", "alerts"} {
		if !strings.Contains(joined, want) {
			t.Errorf("no warning mentioning %q in:\n%s", want, joined)
		}
	}
}

func TestParseSmokePingAlsoIPv6(t *testing.T) {
	f, _, err := ParseSmokePing([]byte(smokepingTargets), true)
	if err != nil {
		t.Fatal(err)
	}
	v6, ok := f.Targets["Internet/google-v6"]
	if !ok {
		t.Fatalf("no v6 sibling created (got %v)", keys(f.Targets))
	}
	if *v6.AddressFamily != "v6" || *v6.Host != "google.com" {
		t.Errorf("v6 sibling = %+v", v6)
	}
	// A literal v4 address must not be duplicated: it cannot resolve to v6.
	if _, ok := f.Targets["Datacenter/router1-v6"]; ok {
		t.Error("literal v4 address should not get a v6 sibling")
	}
}

// The importer must produce a configuration that the normal sync accepts, so
// an imported tree obeys exactly the same rules as a hand-written one.
func TestSmokePingImportApplies(t *testing.T) {
	ctx := context.Background()
	st := open(t)
	f, _, err := ParseSmokePing([]byte(smokepingTargets), false)
	if err != nil {
		t.Fatal(err)
	}
	sum, err := Apply(ctx, st, f, false, AllowUnknownAgents())
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if sum.Created == 0 {
		t.Fatalf("nothing created: %+v", sum)
	}
	tr, byPath := mustTree(t, st)

	leaf := byPath["/Datacenter/router1"]
	res, err := tr.Resolve(leaf.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.PingsPerInterval.Effective != 20 || res.PingsPerInterval.Source.Path != "/Datacenter/router1" {
		t.Errorf("router1 pings = %+v", res.PingsPerInterval)
	}
	// router2 has no local pings, so it inherits the file-level default of 15.
	res2, err := tr.Resolve(byPath["/Datacenter/router2"].ID)
	if err != nil {
		t.Fatal(err)
	}
	if res2.PingsPerInterval.Effective != 15 || res2.PingsPerInterval.Source.Path != "/" {
		t.Errorf("router2 pings = %+v", res2.PingsPerInterval)
	}
}

func TestParseSmokePingErrors(t *testing.T) {
	if _, _, err := ParseSmokePing([]byte("*** Targets ***\n"), false); err == nil {
		t.Error("expected an error for a file with no targets")
	}
	if _, _, err := ParseSmokePing([]byte("*** Targets ***\n+++ deep\nhost = 1.1.1.1\n"), false); err == nil {
		t.Error("expected an error when a level is skipped")
	}
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
