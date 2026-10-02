package tree

import (
	"fmt"
	"strings"
	"testing"
)

func leaf() Target {
	return Target{ID: 10, ParentID: ptr(int64(1)), Name: "gw", Enabled: true,
		Host: ptr("192.0.2.1"), AddressFamily: ptr("v4")}
}

// Each field limit, at the limit and one past it. A limit that is never
// reached by a test is a limit nobody knows is wrong.
func TestCheckLimitsFields(t *testing.T) {
	long := func(n int) string { return strings.Repeat("a", n) }
	cases := []struct {
		name   string
		mutate func(*Target)
		ok     bool
	}{
		{"name at the limit", func(t *Target) { t.Name = long(MaxNameRunes) }, true},
		{"name over the limit", func(t *Target) { t.Name = long(MaxNameRunes + 1) }, false},
		{"name with a newline", func(t *Target) { t.Name = "a\nb" }, false},
		{"name with a right-to-left override", func(t *Target) { t.Name = "a‮b" }, false},
		{"name with a zero-width joiner", func(t *Target) { t.Name = "a‍b" }, false},
		{"name with a line separator", func(t *Target) { t.Name = "a b" }, false},
		{"name with leading space", func(t *Target) { t.Name = " gw" }, false},
		{"name with trailing space", func(t *Target) { t.Name = "gw " }, false},
		{"name with inner space and accents", func(t *Target) { t.Name = "Gemeente Zoëterwoude (A)" }, true},
		{"name not UTF-8", func(t *Target) { t.Name = "a\xffb" }, false},

		{"IPv4", func(t *Target) { t.Host = ptr("198.51.100.7") }, true},
		{"IPv6", func(t *Target) { t.Host = ptr("2001:db8::1") }, true},
		{"IPv6 with a zone", func(t *Target) { t.Host = ptr("fe80::1%eth0") }, true},
		{"hostname", func(t *Target) { t.Host = ptr("resolver.example.org") }, true},
		{"hostname with trailing dot", func(t *Target) { t.Host = ptr("example.org.") }, true},
		{"hostname with underscore", func(t *Target) { t.Host = ptr("_sip._tcp.example.org") }, true},
		{"host with a slash", func(t *Target) { t.Host = ptr("a/b") }, false},
		{"host with userinfo", func(t *Target) { t.Host = ptr("x@internal.corp") }, false},
		{"host with a fragment", func(t *Target) { t.Host = ptr("intranet.corp#.example.net") }, false},
		{"host with a query", func(t *Target) { t.Host = ptr("a?b") }, false},
		{"host with a space", func(t *Target) { t.Host = ptr("a b") }, false},
		{"host with a newline", func(t *Target) { t.Host = ptr("a\nb") }, false},
		{"host with a port", func(t *Target) { t.Host = ptr("example.org:2112") }, false},
		{"host with an empty label", func(t *Target) { t.Host = ptr("a..b") }, false},
		{"host with a 63 byte label", func(t *Target) { t.Host = ptr(long(63) + ".org") }, true},
		{"host with a 64 byte label", func(t *Target) { t.Host = ptr(long(64) + ".org") }, false},
		{"host at 253 bytes", func(t *Target) { t.Host = ptr(strings.Join([]string{long(63), long(63), long(63), long(61)}, ".")) }, true},
		{"host over 253 bytes", func(t *Target) { t.Host = ptr(strings.Join([]string{long(63), long(63), long(63), long(62)}, ".")) }, false},
		{"empty host", func(t *Target) { t.Host = ptr("") }, false},

		{"title with a control character", func(t *Target) { t.Title = ptr("a\x1bb") }, false},
		{"title with a newline", func(t *Target) { t.Title = ptr("a\nb") }, false},
		{"title at the limit", func(t *Target) { t.Title = ptr(long(MaxTitleRunes)) }, true},
		{"title over the limit", func(t *Target) { t.Title = ptr(long(MaxTitleRunes + 1)) }, false},
		{"notes with newlines and tabs", func(t *Target) { t.Notes = ptr("line one\n\tline two\r\n") }, true},
		{"notes with an override", func(t *Target) { t.Notes = ptr("a‮b") }, false},
		{"notes with an escape", func(t *Target) { t.Notes = ptr("a\x1bb") }, false},
		{"notes at the limit", func(t *Target) { t.Notes = ptr(long(MaxNotesRunes)) }, true},
		{"notes over the limit", func(t *Target) { t.Notes = ptr(long(MaxNotesRunes + 1)) }, false},

		{"interval 1", func(t *Target) { t.Settings.IntervalS = ptr(1) }, true},
		{"interval 0", func(t *Target) { t.Settings.IntervalS = ptr(0) }, false},
		{"interval at the limit", func(t *Target) { t.Settings.IntervalS = ptr(MaxIntervalS) }, true},
		{"interval over the limit", func(t *Target) { t.Settings.IntervalS = ptr(MaxIntervalS + 1) }, false},
		{"interval that overflows a Duration", func(t *Target) { t.Settings.IntervalS = ptr(9_223_372_037) }, false},
		{"pings 1", func(t *Target) { t.Settings.PingsPerInterval = ptr(1) }, true},
		{"pings 0", func(t *Target) { t.Settings.PingsPerInterval = ptr(0) }, false},
		{"pings at the limit", func(t *Target) { t.Settings.PingsPerInterval = ptr(MaxPings) }, true},
		{"pings over the limit", func(t *Target) { t.Settings.PingsPerInterval = ptr(MaxPings + 1) }, false},
		{"pings at the old wire maximum", func(t *Target) { t.Settings.PingsPerInterval = ptr(65535) }, false},
		{"gap at the limit", func(t *Target) { t.Settings.BurstGapMS = ptr(MaxBurstGapMS) }, true},
		{"gap over the limit", func(t *Target) { t.Settings.BurstGapMS = ptr(MaxBurstGapMS + 1) }, false},
		{"gap 0", func(t *Target) { t.Settings.BurstGapMS = ptr(0) }, true},
		{"packet 12", func(t *Target) { t.Settings.PacketSize = ptr(12) }, true},
		{"packet 11", func(t *Target) { t.Settings.PacketSize = ptr(11) }, false},
		{"packet at the limit", func(t *Target) { t.Settings.PacketSize = ptr(MaxPacketSize) }, true},
		{"packet over the limit", func(t *Target) { t.Settings.PacketSize = ptr(MaxPacketSize + 1) }, false},
		{"packet at the old maximum", func(t *Target) { t.Settings.PacketSize = ptr(65000) }, false},
		{"trace off", func(t *Target) { t.Settings.TraceIntervalS = ptr(0) }, true},
		{"trace at the floor", func(t *Target) { t.Settings.TraceIntervalS = ptr(MinTraceS) }, true},
		{"trace under the floor", func(t *Target) { t.Settings.TraceIntervalS = ptr(MinTraceS - 1) }, false},
		{"trace at the ceiling", func(t *Target) { t.Settings.TraceIntervalS = ptr(MaxTraceS) }, true},
		{"trace over the ceiling", func(t *Target) { t.Settings.TraceIntervalS = ptr(MaxTraceS + 1) }, false},
		{"trace that overflows a Duration", func(t *Target) { t.Settings.TraceIntervalS = ptr(18_446_744_074) }, false},
		{"retention forever", func(t *Target) { t.Settings.RetentionS = ptr(0) }, true},
		{"retention at the floor", func(t *Target) { t.Settings.RetentionS = ptr(MinRetentionS) }, true},
		{"retention of one second, which deletes", func(t *Target) { t.Settings.RetentionS = ptr(1) }, false},
		{"retention at the ceiling", func(t *Target) { t.Settings.RetentionS = ptr(MaxRetentionS) }, true},
		{"retention over the ceiling", func(t *Target) { t.Settings.RetentionS = ptr(MaxRetentionS + 1) }, false},

		{"dns query", func(t *Target) { t.Settings.DNSQuery = ptr("_dmarc.example.org") }, true},
		{"dns query with a slash", func(t *Target) { t.Settings.DNSQuery = ptr("a/b.example.org") }, false},
		{"dns query root", func(t *Target) { t.Settings.DNSQuery = ptr(".") }, true},
		{"dns query over the limit", func(t *Target) { t.Settings.DNSQuery = ptr(long(MaxDNSQueryLen+1) + ".org") }, false},
		{"http path", func(t *Target) { t.Settings.HTTPPath = ptr("/healthz?full=1") }, true},
		{"http path with a space", func(t *Target) { t.Settings.HTTPPath = ptr("/a b") }, false},
		{"http path with a newline", func(t *Target) { t.Settings.HTTPPath = ptr("/a\r\nHost: x") }, false},
		{"http path over the limit", func(t *Target) { t.Settings.HTTPPath = ptr("/" + long(MaxHTTPPathLen)) }, false},
		{"agents over the limit", func(t *Target) { t.Settings.Agents = ptr(long(MaxAgentsLen + 1)) }, false},
		{"agents with a newline", func(t *Target) { t.Settings.Agents = ptr("a\nb") }, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n := leaf()
			c.mutate(&n)
			err := CheckLimits(nil, &n)
			if (err == nil) != c.ok {
				t.Errorf("CheckLimits = %v, want ok=%v", err, c.ok)
			}
		})
	}
}

// A limit applies to what a write changes. A node that predates one is not
// locked out of unrelated edits by it, which would turn a tightening into an
// outage for anyone who had configured past the new line.
func TestLegacyValuesDoNotBlockUnrelatedChanges(t *testing.T) {
	legacy := leaf()
	legacy.Name = strings.Repeat("n", MaxNameRunes+50)
	legacy.Host = ptr("a/b")
	legacy.Settings.PacketSize = ptr(20000)
	legacy.Settings.PingsPerInterval = ptr(5000)
	legacy.Settings.RetentionS = ptr(5)

	retitled := legacy
	retitled.Title = ptr("a new title")
	if err := CheckLimits(&legacy, &retitled); err != nil {
		t.Errorf("retitling a node with legacy values was refused: %v", err)
	}
	// Changing one of the legacy fields is checked, even to another value that
	// is still over the limit.
	for name, mutate := range map[string]func(*Target){
		"name":      func(t *Target) { t.Name += "x" },
		"host":      func(t *Target) { t.Host = ptr("a/c") },
		"packet":    func(t *Target) { t.Settings.PacketSize = ptr(30000) },
		"pings":     func(t *Target) { t.Settings.PingsPerInterval = ptr(6000) },
		"retention": func(t *Target) { t.Settings.RetentionS = ptr(6) },
	} {
		changed := legacy
		mutate(&changed)
		if err := CheckLimits(&legacy, &changed); err == nil {
			t.Errorf("changing the %s of a legacy node to another bad value was accepted", name)
		}
	}
	// And moving it back inside the limit is, of course, fine.
	fixed := legacy
	fixed.Settings.PacketSize = ptr(56)
	if err := CheckLimits(&legacy, &fixed); err != nil {
		t.Errorf("bringing a legacy value inside the limit was refused: %v", err)
	}
}

// chain builds root -> g1 -> g2 ... of the given depth, each a group.
func chain(depthWanted int) []Target {
	ts := testTargets()[:1]
	for i := 1; i <= depthWanted; i++ {
		ts = append(ts, Target{ID: int64(i + 1), ParentID: ptr(int64(i)), Name: fmt.Sprintf("g%d", i), Enabled: true})
	}
	return ts
}

func TestCheckTreeLimitsDepth(t *testing.T) {
	before := chain(MaxDepth - 1)
	// One more level, at the limit.
	at := append(append([]Target(nil), before...), Target{ID: 900, ParentID: ptr(before[len(before)-1].ID), Name: "leaf", Enabled: true})
	if err := CheckTreeLimits(before, at); err != nil {
		t.Errorf("a node at depth %d was refused: %v", MaxDepth, err)
	}
	over := append(append([]Target(nil), at...), Target{ID: 901, ParentID: ptr(int64(900)), Name: "too-deep", Enabled: true})
	if err := CheckTreeLimits(at, over); err == nil {
		t.Errorf("a node at depth %d was accepted", MaxDepth+1)
	}
	// A tree already past the limit can still be edited where it is not deepened.
	deep := chain(MaxDepth + 6)
	edited := append([]Target(nil), deep...)
	edited[len(edited)-1].Title = ptr("retitled")
	if err := CheckTreeLimits(deep, edited); err != nil {
		t.Errorf("editing a node in a tree that was already too deep was refused: %v", err)
	}
}

func TestCheckTreeLimitsCount(t *testing.T) {
	grow := func(n int) []Target {
		ts := testTargets()[:2]
		for i := 0; i < n; i++ {
			ts = append(ts, Target{ID: int64(1000 + i), ParentID: ptr(int64(2)), Name: fmt.Sprintf("t%d", i), Enabled: true,
				Host: ptr("192.0.2.1"), AddressFamily: ptr("v4"), Settings: Settings{PingsPerInterval: ptr(1), IntervalS: ptr(MaxIntervalS)}})
		}
		return ts
	}
	atLimit := grow(MaxTargets - 2)
	if err := CheckTreeLimits(nil, atLimit); err != nil {
		t.Errorf("%d targets was refused: %v", len(atLimit), err)
	}
	over := grow(MaxTargets - 1)
	if err := CheckTreeLimits(atLimit, over); err == nil {
		t.Errorf("%d targets was accepted", len(over))
	}
	// Removing targets from a tree that is over the limit is always allowed,
	// including when what is left is still over it.
	wayOver, stillOver := grow(MaxTargets+3), grow(MaxTargets)
	if len(stillOver) <= MaxTargets {
		t.Fatalf("the fixture is wrong: %d targets is not over the limit of %d", len(stillOver), MaxTargets)
	}
	if err := CheckTreeLimits(wayOver, stillOver); err != nil {
		t.Errorf("shrinking a tree that was over the limit, to a size still over it, was refused: %v", err)
	}
}

func withLeaf(root Target, l Target, group Settings) []Target {
	base := testTargets()[:2]
	base[1].Settings = group
	return append(base, l)
}

func TestCheckTreeLimitsLoad(t *testing.T) {
	mk := func(s Settings) []Target {
		l := Target{ID: 3, ParentID: ptr(int64(2)), Name: "l", Enabled: true, Host: ptr("192.0.2.1"), AddressFamily: ptr("v4"), Settings: s}
		return withLeaf(testTargets()[0], l, Settings{})
	}
	ok := func(name string, s Settings, want bool) {
		t.Helper()
		err := CheckTreeLimits(nil, mk(s))
		if (err == nil) != want {
			t.Errorf("%s: %v, want ok=%v", name, err, want)
		}
	}
	ok("100 pings in 1s", Settings{PingsPerInterval: ptr(100), IntervalS: ptr(1), TimeoutMS: ptr(1000), BurstGapMS: ptr(1)}, true)
	ok("101 pings in 1s", Settings{PingsPerInterval: ptr(101), IntervalS: ptr(1), TimeoutMS: ptr(1000), BurstGapMS: ptr(1)}, false)
	ok("1000 pings in 10s", Settings{PingsPerInterval: ptr(1000), IntervalS: ptr(10), TimeoutMS: ptr(1000), BurstGapMS: ptr(1)}, true)
	ok("1000 pings in 9s", Settings{PingsPerInterval: ptr(1000), IntervalS: ptr(9), TimeoutMS: ptr(1000), BurstGapMS: ptr(1)}, false)
	// 50 pings of 9000 bytes a second is 450,000 bytes a second; 60 is 540,000.
	ok("50 pings/s of 9000 bytes", Settings{PingsPerInterval: ptr(50), IntervalS: ptr(1), PacketSize: ptr(9000), TimeoutMS: ptr(1000), BurstGapMS: ptr(1)}, true)
	ok("60 pings/s of 9000 bytes", Settings{PingsPerInterval: ptr(60), IntervalS: ptr(1), PacketSize: ptr(9000), TimeoutMS: ptr(1000), BurstGapMS: ptr(1)}, false)
	ok("timeout equal to the interval", Settings{IntervalS: ptr(5), TimeoutMS: ptr(5000), PingsPerInterval: ptr(2), BurstGapMS: ptr(1)}, true)
	ok("timeout longer than the interval", Settings{IntervalS: ptr(5), TimeoutMS: ptr(5001), PingsPerInterval: ptr(2), BurstGapMS: ptr(1)}, false)
	ok("the documented defaults", Settings{}, true)
}

// The load of a leaf comes from values it inherits. A group's interval or
// timeout changes the load of every leaf beneath it.
func TestCheckTreeLimitsFollowsInheritance(t *testing.T) {
	before := testTargets()
	after := testTargets()
	// Production's interval drops to 1s, and its leaf sends 40 pings: 40 a
	// second is inside the rate limit, but the 1000 ms timeout is not longer than
	// the interval, so make the timeout the thing that is wrong.
	after[1].Settings.IntervalS = ptr(1)
	after[1].Settings.TimeoutMS = ptr(5000)
	if err := CheckTreeLimits(before, after); err == nil {
		t.Error("a group setting that pushes a leaf past a limit was accepted")
	}
}

// The tree can be past a limit before the change, and a change that does not
// make a target worse is not refused for it. Only what a change makes worse is.
func TestCheckTreeLimitsOnlyRefusesWhatGetsWorse(t *testing.T) {
	bad := testTargets()
	bad[2].Settings.PingsPerInterval = ptr(900)
	bad[2].Settings.IntervalS = ptr(2)
	bad[2].Settings.BurstGapMS = ptr(1) // 450 a second, over the limit, and already stored
	if _, err := New(bad); err != nil {
		t.Fatalf("the fixture is not a valid stored tree: %v", err)
	}
	with := func(f func(*Target)) []Target {
		out := append([]Target(nil), bad...)
		f(&out[2])
		return out
	}
	// An unrelated edit.
	if err := CheckTreeLimits(bad, with(func(t *Target) { t.Title = ptr("retitled") })); err != nil {
		t.Errorf("an unrelated edit to a leaf already over the limit was refused: %v", err)
	}
	// Reducing it step by step, still over the limit, is allowed.
	if err := CheckTreeLimits(bad, with(func(t *Target) { t.Settings.PingsPerInterval = ptr(800) })); err != nil {
		t.Errorf("a reduction was refused for still being over the limit: %v", err)
	}
	// Increasing it is not.
	if err := CheckTreeLimits(bad, with(func(t *Target) { t.Settings.PingsPerInterval = ptr(901) })); err == nil {
		t.Error("increasing a load that was already over the limit was accepted")
	}
	// A higher rate with smaller packets is worse in the dimension that matters
	// for the flood, even though the bytes a second go down.
	if err := CheckTreeLimits(bad, with(func(t *Target) {
		t.Settings.PingsPerInterval = ptr(990)
		t.Settings.PacketSize = ptr(12)
	})); err == nil {
		t.Error("raising the pings a second while shrinking the packets was accepted")
	}
	// Nor is making one dimension worse while another improves.
	if err := CheckTreeLimits(bad, with(func(t *Target) {
		t.Settings.PingsPerInterval = ptr(800)
		t.Settings.PacketSize = ptr(9000)
	})); err == nil {
		t.Error("a change that lowers the rate but raises the bytes a second was accepted")
	}
}

// Many modest targets are a flood too, so the sum is bounded.
func TestCheckTreeLimitsTotalRate(t *testing.T) {
	build := func(n int) []Target {
		ts := testTargets()[:2]
		for i := 0; i < n; i++ {
			ts = append(ts, Target{ID: int64(100 + i), ParentID: ptr(int64(2)), Name: fmt.Sprintf("t%d", i), Enabled: true,
				Host: ptr("192.0.2.1"), AddressFamily: ptr("v4"),
				Settings: Settings{PingsPerInterval: ptr(100), IntervalS: ptr(1), TimeoutMS: ptr(1000), BurstGapMS: ptr(1)}})
		}
		return ts
	}
	fifty := build(50) // 5000 a second exactly
	if err := CheckTreeLimits(nil, fifty); err != nil {
		t.Errorf("exactly the total limit was refused: %v", err)
	}
	if err := CheckTreeLimits(fifty, build(51)); err == nil {
		t.Error("one more target past the total limit was accepted")
	}
	// Disabled targets send nothing.
	off := build(51)
	off[len(off)-1].Enabled = false
	if err := CheckTreeLimits(fifty, off); err != nil {
		t.Errorf("a disabled target counted against the total: %v", err)
	}
	// An installation already past it can still shrink.
	if err := CheckTreeLimits(build(60), build(55)); err != nil {
		t.Errorf("reducing a tree that was over the total was refused: %v", err)
	}
}

// The tests above are written against the constants, so a constant with the
// wrong value moves both sides of its own test and nothing notices. These are
// the numbers the documentation states, pinned.
func TestTheLimitsAreTheDocumentedNumbers(t *testing.T) {
	for name, c := range map[string][2]int{
		"MaxNameRunes":        {MaxNameRunes, 128},
		"MaxTitleRunes":       {MaxTitleRunes, 500},
		"MaxNotesRunes":       {MaxNotesRunes, 16384},
		"MaxHostLen":          {MaxHostLen, 253},
		"MaxDNSQueryLen":      {MaxDNSQueryLen, 253},
		"MaxHTTPPathLen":      {MaxHTTPPathLen, 2048},
		"MaxAgentsLen":        {MaxAgentsLen, 1024},
		"MaxDepth":            {MaxDepth, 16},
		"MaxTargets":          {MaxTargets, 10_000},
		"MaxIntervalS":        {MaxIntervalS, 86_400},
		"MaxPings":            {MaxPings, 1000},
		"MaxBurstGapMS":       {MaxBurstGapMS, 600_000},
		"MaxPacketSize":       {MaxPacketSize, 9000},
		"MinTraceS":           {MinTraceS, 30},
		"MaxTraceS":           {MaxTraceS, 604_800},
		"MinRetentionS":       {MinRetentionS, 3600},
		"MaxRetentionS":       {MaxRetentionS, 315_360_000},
		"MaxProbeRate":        {MaxProbeRate, 100},
		"MaxProbeBytesPerSec": {MaxProbeBytesPerSec, 500_000},
		"MaxTotalProbeRate":   {MaxTotalProbeRate, 5000},
	} {
		if c[0] != c[1] {
			t.Errorf("%s = %d, the documentation says %d", name, c[0], c[1])
		}
	}
	// And neither of the two that guard a Duration may be able to overflow one.
	if int64(MaxTraceS)*1_000_000_000 <= 0 || int64(MaxRetentionS)*1_000_000_000 <= 0 || int64(MaxIntervalS)*1_000_000_000 <= 0 {
		t.Error("a limit lets time.Duration(seconds) * time.Second overflow")
	}
	// The bytes bound is only a bound if it can be reached.
	if MaxProbeBytesPerSec >= MaxProbeRate*MaxPacketSize {
		t.Errorf("MaxProbeBytesPerSec = %d can never be exceeded: %d pings a second of %d bytes is only %d",
			MaxProbeBytesPerSec, MaxProbeRate, MaxPacketSize, MaxProbeRate*MaxPacketSize)
	}
}

// A timeout longer than its interval is a load of its own: probes still waiting
// when the next interval's go out. One already stored can be shortened, or left
// alone, but not lengthened.
func TestCheckTreeLimitsOnlyRefusesALongerWait(t *testing.T) {
	legacy := testTargets()
	legacy[2].Settings.IntervalS = ptr(2)
	legacy[2].Settings.PingsPerInterval = ptr(2)
	legacy[2].Settings.BurstGapMS = ptr(1)
	legacy[2].Settings.TimeoutMS = ptr(3000) // longer than the 2s interval, and already stored
	if _, err := New(legacy); err != nil {
		t.Fatalf("the fixture is not a valid stored tree: %v", err)
	}
	with := func(timeout int) []Target {
		out := append([]Target(nil), legacy...)
		out[2].Settings.TimeoutMS = ptr(timeout)
		return out
	}
	if err := CheckTreeLimits(legacy, with(2500)); err != nil {
		t.Errorf("shortening a timeout that is still too long was refused: %v", err)
	}
	if err := CheckTreeLimits(legacy, with(3001)); err == nil {
		t.Error("lengthening a timeout that was already longer than its interval was accepted")
	}
	if err := CheckTreeLimits(legacy, with(2000)); err != nil {
		t.Errorf("bringing a timeout down to its interval was refused: %v", err)
	}
}

// branch returns a chain of n nodes under parent, ids from first.
func branch(first int64, n int, parent int64) []Target {
	var ts []Target
	for i := 0; i < n; i++ {
		ts = append(ts, Target{ID: first + int64(i), ParentID: ptr(parent), Name: fmt.Sprintf("b%d", first+int64(i)), Enabled: true})
		parent = first + int64(i)
	}
	return ts
}

func reparent(ts []Target, id, parent int64) []Target {
	out := append([]Target(nil), ts...)
	for i := range out {
		if out[i].ID == id {
			out[i].ParentID = ptr(parent)
		}
	}
	return out
}

// Moving a subtree deepens every node beneath it, not just the node that moved.
func TestMovingASubtreeIsHeldToTheDepthOfItsDeepestNode(t *testing.T) {
	ts := append(append(testTargets()[:1], branch(100, 10, 1)...), branch(200, 12, 1)...) // 109 is 10 deep; 211 is 12 deep
	if err := CheckTreeLimits(ts, reparent(ts, 200, 109)); err == nil {
		t.Errorf("a 12-level subtree moved under a 10-level node (22 deep) was accepted")
	}
	// Control: the same subtree moved somewhere it still fits is fine.
	if err := CheckTreeLimits(ts, reparent(ts, 200, 100)); err != nil {
		t.Errorf("a move that stays within %d levels was refused: %v", MaxDepth, err)
	}
	// A subtree already too deep may be moved shallower.
	deep := append(append(testTargets()[:1], branch(100, 3, 1)...), branch(200, MaxDepth+4, 100)...)
	if err := CheckTreeLimits(deep, reparent(deep, 200, 1)); err != nil {
		t.Errorf("moving an over-deep subtree closer to the root was refused: %v", err)
	}
}

// A group may hold settings that no probe uses. Giving it a host, or enabling
// it, is the write that starts the load, and is held to the load limits.
func TestStartingATargetIsHeldToTheLoadLimits(t *testing.T) {
	hot := Settings{IntervalS: ptr(1), PingsPerInterval: ptr(1000), TimeoutMS: ptr(1000), ProbeMode: ptr("spread")}
	group := func(enabled bool) []Target {
		return append(testTargets()[:1], Target{ID: 2, ParentID: ptr(int64(1)), Name: "g", Enabled: enabled, Settings: hot})
	}
	before := group(true)
	if err := CheckTreeLimits(nil, before); err != nil {
		t.Fatalf("a group holding settings was refused: %v", err)
	}
	withHost := group(true)
	withHost[1].Host, withHost[1].AddressFamily = ptr("192.0.2.1"), ptr("v4")
	if err := CheckTreeLimits(before, withHost); err == nil {
		t.Errorf("a group given a host started probing 1000 pings a second")
	}
	// Enabling a stored target that is over the limit starts it too.
	off := group(false)
	off[1].Host, off[1].AddressFamily = ptr("192.0.2.1"), ptr("v4")
	on := group(true)
	on[1].Host, on[1].AddressFamily = ptr("192.0.2.1"), ptr("v4")
	if err := CheckTreeLimits(off, on); err == nil {
		t.Errorf("enabling a target over the load limit was accepted")
	}
	// Control: disabling it, or editing it while it stays enabled and unchanged, is not refused.
	if err := CheckTreeLimits(on, off); err != nil {
		t.Errorf("disabling a target over the limit was refused: %v", err)
	}
	edited := group(true)
	edited[1].Host, edited[1].AddressFamily, edited[1].Title = ptr("192.0.2.1"), ptr("v4"), ptr("retitled")
	if err := CheckTreeLimits(on, edited); err != nil {
		t.Errorf("a retitle of a running over-limit target was refused: %v", err)
	}
}
