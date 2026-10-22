package trace

import (
	"net/netip"
	"testing"
)

func TestPathString(t *testing.T) {
	p := Path{
		{Addr: netip.MustParseAddr("10.0.0.1")},
		{}, // silent router
		{Addr: netip.MustParseAddr("192.0.2.9")},
	}
	if got, want := p.String(), "10.0.0.1,*,192.0.2.9"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
	if got := (Path{{}, {}, {}}).String(); got != "*,*,*" {
		t.Errorf("all-silent String() = %q, want *,*,*", got)
	}
	if got := (Path(nil)).String(); got != "" {
		t.Errorf("empty String() = %q, want empty", got)
	}
}
