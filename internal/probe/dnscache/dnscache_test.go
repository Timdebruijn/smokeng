package dnscache

import (
	"context"
	"testing"
	"time"
)

// Literal-IP handling and family checks must work without any resolver.
func TestLiteralIPs(t *testing.T) {
	c := &Cache{entries: map[key]*entry{}}
	ctx := context.Background()

	addr, err := c.Lookup(ctx, "1.1.1.1", "v4")
	if err != nil || addr.String() != "1.1.1.1" {
		t.Fatalf("v4 literal: %v %v", addr, err)
	}
	addr, err = c.Lookup(ctx, "2001:db8::1", "v6")
	if err != nil || addr.String() != "2001:db8::1" {
		t.Fatalf("v6 literal: %v %v", addr, err)
	}
	if _, err := c.Lookup(ctx, "1.1.1.1", "v6"); err == nil {
		t.Fatal("v4 literal accepted for v6 target")
	}
	if _, err := c.Lookup(ctx, "2001:db8::1", "v4"); err == nil {
		t.Fatal("v6 literal accepted for v4 target")
	}
}

func TestClampTTL(t *testing.T) {
	for ttl, want := range map[uint32]time.Duration{
		0:      minTTL,
		5:      minTTL,
		300:    300 * time.Second,
		999999: maxTTL,
	} {
		if got := clampTTL(ttl); got != want {
			t.Errorf("clampTTL(%d) = %v, want %v", ttl, got, want)
		}
	}
}
