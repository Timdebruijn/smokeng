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

// The probe modules a real SmokePing install actually names, mapped to the
// smokeng type that measures the same thing. IRTT is the one that matters most:
// smokeng probes with the same tool, so importing it as icmp would turn a UDP
// round-trip measurement into an ICMP ping wearing the same graph — found by
// running the importer against a production config that used it.
func TestSmokePingProbeModuleMapping(t *testing.T) {
	cases := map[string]string{
		"FPing":           "icmp",
		"FPing6":          "icmp",
		"FPingContinuous": "icmp", // a renamed fping probe still pings
		"IRTT":            "irtt",
		"DNS":             "dns",
		"EchoPingDNS":     "dns",
		"AnotherDNS":      "dns",
		"EchoPingHttp":    "http",
		"Curl":            "http",
		"EchoPingHttps":   "https",
		"TCPPing":         "tcp",
		"EchoPingTcp":     "tcp",
		"":                "icmp", // no probe named: SmokePing's own default
		"SomeUnknown":     "",     // unrecognised: warned about, never guessed
	}
	for module, want := range cases {
		if got := probeTypeFor(module); got != want {
			t.Errorf("probeTypeFor(%q) = %q, want %q", module, got, want)
		}
	}
}

// An IRTT target comes across as irtt, with the difference in what is graphed
// stated rather than left for the operator to discover.
func TestSmokePingImportsIRTT(t *testing.T) {
	const cfg = `*** Probes ***

+ FPing
binary = /usr/bin/fping

+ IRTT
binary = /usr/bin/irtt

*** Targets ***

probe = FPing

+ wag

++ Irtt
probe = IRTT
host = irtt.example.org
`
	f, warnings, err := ParseSmokePing([]byte(cfg), false)
	if err != nil {
		t.Fatal(err)
	}
	e, ok := f.Targets["wag/Irtt"]
	if !ok {
		t.Fatalf("target not imported; got %v", keys(f.Targets))
	}
	if e.ProbeType == nil || *e.ProbeType != "irtt" {
		t.Fatalf("probe_type = %v, want irtt", e.ProbeType)
	}
	var said bool
	for _, w := range warnings {
		if strings.Contains(w, "irtt") && strings.Contains(w, "round-trip") {
			said = true
		}
		if strings.Contains(w, "no smokeng equivalent") {
			t.Errorf("IRTT reported as unmappable: %s", w)
		}
	}
	if !said {
		t.Errorf("no warning that the graph differs; got %v", warnings)
	}
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// A secret in the SmokePing config is not imported — it would travel into the
// API, the export and version control — but its absence must be stated. Without
// the key the irtt server refuses the session, and the target reads as a send
// failure: a silent outage that looks like a network fault rather than a missing
// setting. Found on a real migration, where three targets went quiet for exactly
// this reason.
func TestSmokePingWarnsAboutIRTTSecret(t *testing.T) {
	const cfg = `*** Probes ***

+ IRTT
binary = /usr/bin/irtt

*** Targets ***

probe = IRTT

+ wag
host = irtt.example.org
hmac = deadbeefcafe
`
	f, warnings, err := ParseSmokePing([]byte(cfg), false)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range f.Targets {
		// Whatever else it carries, it must not carry the key.
		if e.Notes != nil && strings.Contains(*e.Notes, "deadbeefcafe") {
			t.Error("the HMAC key was imported into the target tree")
		}
	}
	var warned bool
	for _, w := range warnings {
		if strings.Contains(w, "HMAC") {
			warned = true
			if strings.Contains(w, "deadbeefcafe") {
				t.Errorf("the warning leaks the key: %s", w)
			}
		}
	}
	if !warned {
		t.Errorf("no warning that a key is needed; got %v", warnings)
	}
}

// SmokePing's IRTT probe graphs one figure per target, chosen with `metric`,
// all from the same session. Importing each as its own target opened a separate
// irtt session to the same server; they collided, and only one won per interval
// while the rest recorded as send failures. Found on a real migration.
func TestSmokePingSkipsDerivedIRTTMetrics(t *testing.T) {
	const cfg = `*** Probes ***

+ IRTT
binary = /usr/bin/irtt

*** Targets ***

probe = IRTT

+ wag

++ Rtt
host = irtt.example.org

++ SendJitter
host = irtt.example.org
metric = send_ipdv

++ RecvJitter
host = irtt.example.org
metric = receive_ipdv
`
	f, warnings, err := ParseSmokePing([]byte(cfg), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := f.Targets["wag/Rtt"]; !ok {
		t.Errorf("the plain irtt target should be imported; got %v", keys(f.Targets))
	}
	for _, skipped := range []string{"wag/SendJitter", "wag/RecvJitter"} {
		if _, ok := f.Targets[skipped]; ok {
			t.Errorf("%s is a derived view of the same session and must not become its own target", skipped)
		}
	}
	joined := strings.Join(warnings, "\n")
	for _, want := range []string{"send_ipdv", "receive_ipdv"} {
		if !strings.Contains(joined, want) {
			t.Errorf("no warning naming %q in:\n%s", want, joined)
		}
	}
}
