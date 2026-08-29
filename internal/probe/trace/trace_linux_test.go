//go:build linux

package trace

import (
	"context"
	"net/netip"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// The topology this builds, so a traceroute has something real to discover:
//
//	main ns ── 10.100.0.1 ┈┈ 10.100.0.2 ── router ns ── 10.101.0.1 ┈┈ 10.101.0.2 ── dest ns
//
// A path to 10.101.0.2 must therefore be exactly two hops, the router first.
// Verifying against a real forwarding host is the only way to know the ICMP
// time-exceeded parsing is right: a plausible-looking wrong answer here would
// be indistinguishable from a correct one on a single-hop link.
const (
	routerNS   = "smokeng-r"
	destNS     = "smokeng-d"
	routerAddr = "10.100.0.2"
	destAddr   = "10.101.0.2"
)

func run(t *testing.T, args ...string) error {
	t.Helper()
	out, err := exec.Command(args[0], args[1:]...).CombinedOutput()
	if err != nil {
		return &execError{cmd: strings.Join(args, " "), out: string(out), err: err}
	}
	return nil
}

type execError struct {
	cmd, out string
	err      error
}

func (e *execError) Error() string { return e.cmd + ": " + e.err.Error() + ": " + e.out }

func buildTopology(t *testing.T) {
	t.Helper()
	teardown(t)
	steps := [][]string{
		{"ip", "netns", "add", routerNS},
		{"ip", "netns", "add", destNS},
		{"ip", "link", "add", "sm-a", "type", "veth", "peer", "name", "sm-b"},
		{"ip", "link", "set", "sm-b", "netns", routerNS},
		{"ip", "link", "add", "sm-c", "type", "veth", "peer", "name", "sm-d"},
		{"ip", "link", "set", "sm-c", "netns", routerNS},
		{"ip", "link", "set", "sm-d", "netns", destNS},

		{"ip", "addr", "add", "10.100.0.1/24", "dev", "sm-a"},
		{"ip", "link", "set", "sm-a", "up"},
		{"ip", "route", "add", "10.101.0.0/24", "via", routerAddr},

		{"ip", "netns", "exec", routerNS, "ip", "link", "set", "lo", "up"},
		{"ip", "netns", "exec", routerNS, "ip", "addr", "add", "10.100.0.2/24", "dev", "sm-b"},
		{"ip", "netns", "exec", routerNS, "ip", "addr", "add", "10.101.0.1/24", "dev", "sm-c"},
		{"ip", "netns", "exec", routerNS, "ip", "link", "set", "sm-b", "up"},
		{"ip", "netns", "exec", routerNS, "ip", "link", "set", "sm-c", "up"},
		{"ip", "netns", "exec", routerNS, "sysctl", "-qw", "net.ipv4.ip_forward=1"},

		{"ip", "netns", "exec", destNS, "ip", "link", "set", "lo", "up"},
		{"ip", "netns", "exec", destNS, "ip", "addr", "add", "10.101.0.2/24", "dev", "sm-d"},
		{"ip", "netns", "exec", destNS, "ip", "link", "set", "sm-d", "up"},
		{"ip", "netns", "exec", destNS, "ip", "route", "add", "default", "via", "10.101.0.1"},
	}
	for _, step := range steps {
		if err := run(t, step...); err != nil {
			teardown(t)
			t.Skipf("cannot build the test topology (needs root): %v", err)
		}
	}
	t.Cleanup(func() { teardown(t) })
}

func teardown(t *testing.T) {
	t.Helper()
	exec.Command("ip", "netns", "del", routerNS).Run()
	exec.Command("ip", "netns", "del", destNS).Run()
	exec.Command("ip", "link", "del", "sm-a").Run()
}

func TestTraceDiscoversHops(t *testing.T) {
	buildTopology(t)

	dest, err := netip.ParseAddr(destAddr)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	path, err := Trace(ctx, Options{Dest: dest, MaxHops: 5, Timeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("Trace: %v", err)
	}
	t.Logf("path: %s", path)

	if len(path) != 2 {
		t.Fatalf("path has %d hops, want 2 (%s)", len(path), path)
	}
	if got := path[0].Addr.String(); got != routerAddr {
		t.Errorf("first hop = %s, want the router at %s", got, routerAddr)
	}
	if got := path[1].Addr.String(); got != destAddr {
		t.Errorf("second hop = %s, want the destination at %s", got, destAddr)
	}

	// The stored form is what change detection compares, so it must survive.
	if want := routerAddr + "," + destAddr; path.String() != want {
		t.Errorf("String() = %q, want %q", path.String(), want)
	}
	// A second run of an unchanged path must compare equal, or every run
	// would be recorded as a change.
	again, err := Trace(ctx, Options{Dest: dest, MaxHops: 5, Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if !again.SameAs(path) {
		t.Errorf("an unchanged path did not compare equal: %s vs %s", again, path)
	}
}

// A route that changes must be seen to change, or the feature reports nothing
// when it matters most.
func TestTraceSeesARouteChange(t *testing.T) {
	buildTopology(t)

	dest := netip.MustParseAddr(destAddr)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	before, err := Trace(ctx, Options{Dest: dest, MaxHops: 5, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}

	// Renumber the router's near-side address: same topology, different hop.
	for _, step := range [][]string{
		{"ip", "netns", "exec", routerNS, "ip", "addr", "add", "10.100.0.9/24", "dev", "sm-b"},
		{"ip", "netns", "exec", routerNS, "ip", "addr", "del", "10.100.0.2/24", "dev", "sm-b"},
		{"ip", "route", "replace", "10.101.0.0/24", "via", "10.100.0.9"},
	} {
		if err := run(t, step...); err != nil {
			t.Skipf("cannot renumber the router: %v", err)
		}
	}

	after, err := Trace(ctx, Options{Dest: dest, MaxHops: 5, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("before: %s\nafter:  %s", before, after)
	if after.SameAs(before) {
		t.Fatal("the path changed but compared equal, so no change would be recorded")
	}
	if got := after[0].Addr.String(); got != "10.100.0.9" {
		t.Errorf("first hop after renumbering = %s, want 10.100.0.9", got)
	}
}
