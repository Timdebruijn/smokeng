package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// #1: ParseSmokePingFile follows @include, resolving each relative to the
// including file, so a multi-file install imports in one command.
func TestSmokePingFollowsIncludes(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	// main includes a sibling and a child-dir file; paths are relative to each
	// including file's own directory.
	write := func(p, s string) {
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(dir, "Targets"), `*** Targets ***
probe = FPing

+ Internet
@include internet.cfg
@include sub/more.cfg
`)
	write(filepath.Join(dir, "internet.cfg"), `++ cloudflare
host = 1.1.1.1
`)
	write(filepath.Join(sub, "more.cfg"), `++ quad9
host = 9.9.9.9
`)

	f, warnings, err := ParseSmokePingFile(filepath.Join(dir, "Targets"), false)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range warnings {
		if strings.Contains(w, "@include is not followed") {
			t.Errorf("the file parser should follow includes, but warned: %s", w)
		}
	}
	if _, ok := f.Targets["Internet/cloudflare"]; !ok {
		t.Fatalf("target from an included file is missing; got %v", keys(f.Targets))
	}
	if _, ok := f.Targets["Internet/quad9"]; !ok {
		t.Fatalf("target from a nested-dir include is missing; got %v", keys(f.Targets))
	}
}

// An include cycle is broken rather than looped forever.
func TestSmokePingIncludeCycle(t *testing.T) {
	dir := t.TempDir()
	write := func(p, s string) {
		if err := os.WriteFile(filepath.Join(dir, p), []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("a.cfg", "*** Targets ***\n+ X\nhost = 1.1.1.1\n@include b.cfg\n")
	write("b.cfg", "@include a.cfg\n")
	_, warnings, err := ParseSmokePingFile(filepath.Join(dir, "a.cfg"), false)
	if err != nil {
		t.Fatalf("a cycle should be warned about, not fatal: %v", err)
	}
	found := false
	for _, w := range warnings {
		if strings.Contains(w, "cycle") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no cycle warning; got %v", warnings)
	}
}

// #2: a target's SmokePing probe maps to a smokeng probe type, and the obvious
// parameters carry across.
func TestSmokePingMapsProbeTypes(t *testing.T) {
	cfg := `*** Probes ***
+ FPing
binary = /usr/bin/fping
+ EchoPingDNS
+ WebCheck
+ EchoPingHttps

*** Targets ***
probe = FPing

+ svc

++ resolver
probe = EchoPingDNS
host = 10.0.0.53
lookup = example.com
recordtype = A

++ site
probe = EchoPingHttps
host = portal.example.org

++ handshake
probe = TCPPing
host = 10.0.0.9
port = 8443

++ plainping
host = 1.1.1.1
`
	f, _, err := ParseSmokePing([]byte(cfg), false)
	if err != nil {
		t.Fatal(err)
	}

	check := func(path, wantType string, extra func(Entry)) {
		e, ok := f.Targets[path]
		if !ok {
			t.Fatalf("missing target %s", path)
		}
		got := ""
		if e.ProbeType != nil {
			got = *e.ProbeType
		}
		if got != wantType {
			t.Errorf("%s: probe_type = %q, want %q", path, got, wantType)
		}
		if extra != nil {
			extra(e)
		}
	}

	check("svc/resolver", "dns", func(e Entry) {
		if e.DNSQuery == nil || *e.DNSQuery != "example.com" {
			t.Errorf("dns query not carried across: %v", e.DNSQuery)
		}
		if e.DNSRRType == nil || *e.DNSRRType != "A" {
			t.Errorf("dns record type not carried across: %v", e.DNSRRType)
		}
	})
	check("svc/site", "https", nil)
	check("svc/handshake", "tcp", func(e Entry) {
		if e.ProbePort == nil || *e.ProbePort != 8443 {
			t.Errorf("tcp port not carried across: %v", e.ProbePort)
		}
	})
	// A plain fping target is left implicit (icmp is the root default), so no
	// probe_type is written.
	check("svc/plainping", "", nil)
}
