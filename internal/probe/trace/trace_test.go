package trace

import (
	"net/netip"
	"testing"
)

func addr(t *testing.T, s string) netip.Addr {
	t.Helper()
	a, err := netip.ParseAddr(s)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestPathRoundTrip(t *testing.T) {
	p := Path{
		{TTL: 1, Addr: addr(t, "10.0.0.1")},
		{TTL: 2}, // silent router
		{TTL: 3, Addr: addr(t, "192.0.2.9")},
	}
	const want = "10.0.0.1,*,192.0.2.9"
	if got := p.String(); got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
	back := Parse(want)
	if !back.SameAs(p) {
		t.Errorf("Parse(String(p)) = %v, want %v", back, p)
	}
	if len(back) != 3 || back[1].Addr.IsValid() {
		t.Errorf("silent hop did not survive: %+v", back)
	}
}

func TestParseEmpty(t *testing.T) {
	if p := Parse(""); p != nil {
		t.Errorf("Parse(\"\") = %v, want nil", p)
	}
}

// Whether two paths count as the same decides when a change is recorded, so
// the comparison earns its own cases.
func TestSameAs(t *testing.T) {
	base := Parse("10.0.0.1,*,192.0.2.9")
	cases := map[string]struct {
		other string
		same  bool
	}{
		"identical": {"10.0.0.1,*,192.0.2.9", true},
		// A router that stayed silent twice is not evidence of a change;
		// treating it as one would bury the real changes in noise.
		"silent hop stays silent": {"10.0.0.1,*,192.0.2.9", true},
		"a hop changed":           {"10.0.0.1,*,198.51.100.4", false},
		"a hop started answering": {"10.0.0.1,10.0.0.2,192.0.2.9", false},
		"path got longer":         {"10.0.0.1,*,192.0.2.9,203.0.113.7", false},
		"path got shorter":        {"10.0.0.1,*", false},
		"reordered":               {"192.0.2.9,*,10.0.0.1", false},
	}
	for name, c := range cases {
		if got := base.SameAs(Parse(c.other)); got != c.same {
			t.Errorf("%s: SameAs(%q) = %v, want %v", name, c.other, got, c.same)
		}
	}
}

// A path made entirely of silence is still a path, and must round-trip: it is
// what an unresponsive route looks like, and it should compare equal to the
// next equally silent run rather than churning the change log.
func TestAllSilent(t *testing.T) {
	p := Parse("*,*,*")
	if len(p) != 3 {
		t.Fatalf("parsed %d hops, want 3", len(p))
	}
	if p.String() != "*,*,*" {
		t.Errorf("String() = %q", p.String())
	}
	if !p.SameAs(Parse("*,*,*")) {
		t.Error("two silent paths should compare equal")
	}
}
