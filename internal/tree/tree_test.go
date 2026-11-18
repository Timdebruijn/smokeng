package tree

import (
	"slices"
	"strings"
	"testing"
)

func ptr[T any](v T) *T { return &v }

func testTargets() []Target {
	return []Target{
		{ID: 1, Name: "", Enabled: true, Settings: Settings{
			IntervalS:        ptr(60),
			PingsPerInterval: ptr(20),
			ProbeMode:        ptr("burst"),
			BurstGapMS:       ptr(10),
			TimeoutMS:        ptr(1000),
			PacketSize:       ptr(56),
			DSCP:             ptr(0),
			Agents:           ptr("local"),
			TraceIntervalS:   ptr(300),
			RetentionS:       ptr(0),
			GraphSeries:      ptr("all"),
			PositionalLoss:   ptr("exclude"),
			ProbeType:        ptr("icmp"),
			TLSSkipVerify:    ptr(false),
		}},
		{ID: 2, ParentID: ptr(int64(1)), Name: "Production", Enabled: true,
			Settings: Settings{IntervalS: ptr(30)}},
		{ID: 3, ParentID: ptr(int64(2)), Name: "cloudflare-v4", Enabled: true,
			Host: ptr("1.1.1.1"), AddressFamily: ptr("v4"),
			Settings: Settings{PingsPerInterval: ptr(40)}},
	}
}

func TestResolveProvenance(t *testing.T) {
	tr, err := New(testTargets())
	if err != nil {
		t.Fatal(err)
	}
	res, err := tr.Resolve(3)
	if err != nil {
		t.Fatal(err)
	}

	// Local override on the leaf itself.
	if res.PingsPerInterval.Effective != 40 || res.PingsPerInterval.Source.ID != 3 {
		t.Errorf("pings = %+v, want effective 40 from node 3", res.PingsPerInterval)
	}
	if res.PingsPerInterval.Local == nil || *res.PingsPerInterval.Local != 40 {
		t.Errorf("pings local = %v, want 40", res.PingsPerInterval.Local)
	}

	// Inherited from the intermediate group.
	iv := res.IntervalS
	if iv.Effective != 30 || iv.Source.ID != 2 || iv.Source.Path != "/Production" || iv.Local != nil {
		t.Errorf("interval = %+v, want effective 30 inherited from /Production", iv)
	}

	// Inherited from the root.
	if res.ProbeMode.Effective != "burst" || res.ProbeMode.Source.ID != 1 || res.ProbeMode.Source.Path != "/" {
		t.Errorf("probe_mode = %+v, want burst inherited from root", res.ProbeMode)
	}
}

func TestPath(t *testing.T) {
	tr, err := New(testTargets())
	if err != nil {
		t.Fatal(err)
	}
	for id, want := range map[int64]string{1: "/", 2: "/Production", 3: "/Production/cloudflare-v4"} {
		got, err := tr.Path(id)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("Path(%d) = %q, want %q", id, got, want)
		}
	}
}

func TestValidation(t *testing.T) {
	incompleteRoot := testTargets()
	incompleteRoot[0].Settings.DSCP = nil
	missingParent := testTargets()
	missingParent[2].ParentID = ptr(int64(99))
	twoRoots := testTargets()
	twoRoots[1].ParentID = nil

	for name, targets := range map[string][]Target{
		"incomplete root": incompleteRoot,
		"missing parent":  missingParent,
		"two roots":       twoRoots,
		"no nodes":        nil,
	} {
		if _, err := New(targets); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

// The wire format carries sent and received as UInt16, so a configuration that
// could exceed that would report a count which had silently wrapped — 70000
// pings arriving as 4464.
func TestPingsPerIntervalIsBoundedByTheWireFormat(t *testing.T) {
	for _, n := range []int{0, -1, 65536, 1 << 20} {
		n := n
		tg := Target{Name: "t", Settings: Settings{PingsPerInterval: &n}}
		if err := tg.Validate(); err == nil {
			t.Errorf("pings_per_interval = %d was accepted", n)
		}
	}
	for _, n := range []int{1, 20, 65535} {
		n := n
		tg := Target{Name: "t", Settings: Settings{PingsPerInterval: &n}}
		if err := tg.Validate(); err != nil {
			t.Errorf("pings_per_interval = %d was refused: %v", n, err)
		}
	}
}

// Protects the set of families a target is measured over, which the prober,
// alerting, ingest and the replay all read: v4 and v6 are themselves, "both"
// is v4 and v6, a group has none. Family stays the one family a caller gets
// when it names none, v4 for "both".
func TestFamiliesOfATarget(t *testing.T) {
	for _, c := range []struct {
		af    *string
		want  []Family
		deflt Family
	}{
		{ptr("v4"), []Family{FamilyV4}, FamilyV4},
		{ptr("v6"), []Family{FamilyV6}, FamilyV6},
		{ptr("both"), []Family{FamilyV4, FamilyV6}, FamilyV4},
		{nil, nil, FamilyUnknown},
	} {
		n := Target{AddressFamily: c.af}
		if got := n.Families(); !slices.Equal(got, c.want) {
			t.Errorf("Families(%v) = %v, want %v", c.af, got, c.want)
		}
		if got := n.Family(); got != c.deflt {
			t.Errorf("Family(%v) = %v, want %v", c.af, got, c.deflt)
		}
	}
}

// Protects the rule that "both" is measured by name: a literal address has one
// family, so the other series could only ever be no data. A hostname is
// accepted, and so is every literal under v4 or v6. The refusal says why and
// what to do, because it is what a person editing a target reads.
func TestBothNeedsAHostname(t *testing.T) {
	check := func(host, af string) error {
		n := Target{Name: "x", ParentID: ptr(int64(1)), Host: ptr(host), AddressFamily: ptr(af)}
		return n.Validate()
	}
	if err := check("web.example.net", "both"); err != nil {
		t.Errorf("both with a hostname: %v", err)
	}
	for _, host := range []string{"192.0.2.1", "2001:db8::1", "::ffff:192.0.2.1", "fe80::1%eth0"} {
		err := check(host, "both")
		if err == nil || !strings.Contains(err.Error(), "hostname") || !strings.Contains(err.Error(), "v4 or v6") {
			t.Errorf("both with literal %s: %v, want a refusal naming the hostname rule and the alternatives", host, err)
		}
	}
	for _, c := range [][2]string{{"192.0.2.1", "v4"}, {"2001:db8::1", "v6"}, {"web.example.net", "v4"}} {
		if err := check(c[0], c[1]); err != nil {
			t.Errorf("%s as %s: %v", c[0], c[1], err)
		}
	}
	if err := check("web.example.net", "any"); err == nil {
		t.Error("an unknown address_family was accepted")
	}
}
