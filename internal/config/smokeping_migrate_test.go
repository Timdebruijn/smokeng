package config

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
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

func writeFile(t *testing.T, p, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}

// An @include is a path the file chooses, and what it names is read into the
// import: a line shaped like "key = value" in any readable file becomes a note
// on a target in the database. So a file may include what sits under its own
// directory, which is how an install is split, and anything else is refused
// until the operator says where else to look.
func TestSmokePingIncludesStayUnderTheFilesDirectory(t *testing.T) {
	base := t.TempDir()
	conf := filepath.Join(base, "smokeping")
	outside := filepath.Join(base, "elsewhere", "secret.cfg")
	writeFile(t, outside, "++ leaked\nhost = 192.0.2.9\nnotes = hunter2\n")
	main := func(inc string) string {
		p := filepath.Join(conf, "Targets")
		writeFile(t, p, "*** Targets ***\nprobe = FPing\n+ A\nhost = 1.1.1.1\n@include "+inc+"\n")
		return p
	}
	for _, inc := range []string{outside, "../elsewhere/secret.cfg", "sub/../../elsewhere/secret.cfg"} {
		_, _, err := ParseSmokePingFile(main(inc), false)
		if err == nil || !strings.Contains(err.Error(), "--include-root") {
			t.Errorf("@include %s: err = %v, want a refusal that names --include-root", inc, err)
		}
	}
	// Control: the same file is read once it is allowed, so the refusal above was about where it is.
	f, _, err := ParseSmokePingFile(main(outside), false, filepath.Join(base, "elsewhere"))
	if err != nil {
		t.Fatalf("with the directory allowed: %v", err)
	}
	if _, ok := f.Targets["A/leaked"]; !ok {
		t.Errorf("the allowed include was not read; got %v", keys(f.Targets))
	}
	// And a file under the directory needs no flag.
	writeFile(t, filepath.Join(conf, "config.d", "more.cfg"), "++ fine\nhost = 1.0.0.1\n")
	f, _, err = ParseSmokePingFile(main("config.d/more.cfg"), false)
	if err != nil || f.Targets["A/fine"].Host == nil {
		t.Errorf("an include under the file's directory: %v", err)
	}
	// A sibling directory that merely shares a name prefix is not under it.
	writeFile(t, filepath.Join(base, "smokeping-other", "x.cfg"), "++ y\nhost = 1.1.1.2\n")
	if _, _, err := ParseSmokePingFile(main("../smokeping-other/x.cfg"), false); err == nil {
		t.Error("a directory sharing the name prefix was treated as inside")
	}
}

// A symlink inside the directory is a way out of it.
func TestSmokePingIncludesDoNotFollowSymlinksOut(t *testing.T) {
	base := t.TempDir()
	conf := filepath.Join(base, "conf")
	writeFile(t, filepath.Join(base, "outside.cfg"), "++ leaked\nhost = 192.0.2.9\n")
	writeFile(t, filepath.Join(conf, "Targets"), "*** Targets ***\nprobe = FPing\n+ A\nhost = 1.1.1.1\n@include link.cfg\n")
	if err := os.Symlink(filepath.Join(base, "outside.cfg"), filepath.Join(conf, "link.cfg")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ParseSmokePingFile(filepath.Join(conf, "Targets"), false); err == nil || !strings.Contains(err.Error(), "--include-root") {
		t.Errorf("an include through a symlink out of the directory: err = %v", err)
	}
	// A symlink that stays inside is fine.
	writeFile(t, filepath.Join(conf, "real.cfg"), "++ ok\nhost = 1.0.0.1\n")
	if err := os.Symlink(filepath.Join(conf, "real.cfg"), filepath.Join(conf, "inside.cfg")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(conf, "Targets"), "*** Targets ***\nprobe = FPing\n+ A\nhost = 1.1.1.1\n@include inside.cfg\n")
	if _, _, err := ParseSmokePingFile(filepath.Join(conf, "Targets"), false); err != nil {
		t.Errorf("a symlink within the directory: %v", err)
	}
}

// ReadFile on /dev/zero, or a FIFO nobody writes to, does not return.
func TestSmokePingIncludesReadOnlyRegularFiles(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "Targets"), "*** Targets ***\nprobe = FPing\n+ A\nhost = 1.1.1.1\n@include /dev/zero\n")
	_, _, err := ParseSmokePingFile(filepath.Join(dir, "Targets"), false, "/dev")
	if err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Errorf("@include /dev/zero: err = %v", err)
	}
}

// Two includes of the same file are fine (siblings may share one); a file that
// includes a small one a hundred thousand times is a way to spend memory with
// a few kilobytes of input.
func TestSmokePingIncludesAreBudgeted(t *testing.T) {
	defer func(f, b int) { maxIncludeFiles, maxIncludeBytes = f, b }(maxIncludeFiles, maxIncludeBytes)
	maxIncludeFiles, maxIncludeBytes = 50, 1<<20 // bytes are lowered below, for the one case that is about them
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "x.cfg"), "# x\n")
	list := func(n int, name string) string {
		return "*** Targets ***\nprobe = FPing\n+ A\nhost = 1.1.1.1\n" + strings.Repeat("@include "+name+"\n", n)
	}
	writeFile(t, filepath.Join(dir, "few"), list(10, "x.cfg"))
	if _, _, err := ParseSmokePingFile(filepath.Join(dir, "few"), false); err != nil {
		t.Errorf("ten includes of one file: %v", err)
	}
	writeFile(t, filepath.Join(dir, "many"), list(60, "x.cfg"))
	if _, _, err := ParseSmokePingFile(filepath.Join(dir, "many"), false); err == nil || !strings.Contains(err.Error(), "more than") {
		t.Errorf("sixty includes against a limit of fifty: err = %v", err)
	}
	maxIncludeBytes = 1 << 10
	writeFile(t, filepath.Join(dir, "big.cfg"), strings.Repeat("# padding padding padding\n", 20)) // ~520 bytes
	writeFile(t, filepath.Join(dir, "heavy"), list(3, "big.cfg"))
	if _, _, err := ParseSmokePingFile(filepath.Join(dir, "heavy"), false); err == nil || !strings.Contains(err.Error(), "more than") {
		t.Errorf("three reads of a 520-byte file against a limit of 1 KiB: err = %v", err)
	}
	// Nesting: a file that includes itself through a chain is the cycle guard's
	// business, but a fan-out at every level multiplies; the budget stops it.
	maxIncludeBytes = 1 << 20
	writeFile(t, filepath.Join(dir, "l2"), "# l2\n")
	writeFile(t, filepath.Join(dir, "l1"), strings.Repeat("@include l2\n", 8))
	writeFile(t, filepath.Join(dir, "top"), "*** Targets ***\nprobe = FPing\n+ A\nhost = 1.1.1.1\n"+strings.Repeat("@include l1\n", 8))
	if _, _, err := ParseSmokePingFile(filepath.Join(dir, "top"), false); err == nil {
		t.Error("8 x 8 includes (73 reads) against a limit of fifty was accepted")
	}
}

// The file the operator names is theirs to read, wherever it really is: a
// config that is a symlink into a deployment directory still imports.
func TestSmokePingTheNamedFileMayBeASymlink(t *testing.T) {
	base := t.TempDir()
	writeFile(t, filepath.Join(base, "deploy", "Targets"), "*** Targets ***\nprobe = FPing\n+ A\nhost = 1.1.1.1\n")
	if err := os.MkdirAll(filepath.Join(base, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "etc", "Targets")
	if err := os.Symlink(filepath.Join(base, "deploy", "Targets"), link); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ParseSmokePingFile(link, false); err != nil {
		t.Errorf("a named file that is a symlink to another directory: %v", err)
	}
}

// A FIFO nobody writes to blocks open(2) itself, before anything is read, so
// the check for a regular file has to come first.
func TestSmokePingIncludeOfAFIFODoesNotHang(t *testing.T) {
	dir := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(dir, "pipe"), 0o600); err != nil {
		t.Skipf("no FIFOs here: %v", err)
	}
	writeFile(t, filepath.Join(dir, "Targets"), "*** Targets ***\nprobe = FPing\n+ A\nhost = 1.1.1.1\n@include pipe\n")
	done := make(chan error, 1)
	go func() {
		_, _, err := ParseSmokePingFile(filepath.Join(dir, "Targets"), false)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "regular file") {
			t.Errorf("@include of a FIFO: err = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("@include of a FIFO hangs the import")
	}
}

// The byte budget is not a memory budget: every line becomes a record of about
// forty bytes, so blank lines multiply what they cost sixty-fold.
func TestSmokePingIncludesAreBudgetedInLines(t *testing.T) {
	defer func(l int) { maxIncludeLines = l }(maxIncludeLines)
	maxIncludeLines = 100
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "blank.cfg"), strings.Repeat("\n", 60))
	top := func(n int) string {
		p := filepath.Join(dir, "Targets")
		writeFile(t, p, "*** Targets ***\nprobe = FPing\n+ A\nhost = 1.1.1.1\n"+strings.Repeat("@include blank.cfg\n", n))
		return p
	}
	if _, _, err := ParseSmokePingFile(top(1), false); err != nil {
		t.Errorf("one include of sixty lines against a limit of a hundred: %v", err)
	}
	if _, _, err := ParseSmokePingFile(top(3), false); err == nil || !strings.Contains(err.Error(), "lines") {
		t.Errorf("three includes of sixty lines against a limit of a hundred: err = %v", err)
	}
}
