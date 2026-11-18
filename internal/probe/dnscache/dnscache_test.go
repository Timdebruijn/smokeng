package dnscache

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	"codeberg.org/miekg/dns"
	"codeberg.org/miekg/dns/rdata"
)

// fakeCache is a cache whose servers answer from a script: one reply (or error)
// per server, looked up by the server's name, changeable between lookups.
type fakeCache struct {
	*Cache
	reply map[string]func() (*dns.Msg, error)
	asked []string
}

func newFake(servers ...string) *fakeCache {
	f := &fakeCache{reply: map[string]func() (*dns.Msg, error){}}
	f.Cache = &Cache{servers: servers, entries: map[key]*entry{}}
	f.exchange = func(_ context.Context, _ *dns.Msg, server string) (*dns.Msg, error) {
		f.asked = append(f.asked, server)
		return f.reply[server]()
	}
	return f
}

func answerA(addr string) func() (*dns.Msg, error) {
	return func() (*dns.Msg, error) {
		m := &dns.Msg{}
		m.Rcode = dns.RcodeSuccess
		m.Answer = []dns.RR{&dns.A{Hdr: dns.Header{Name: "x.example.", Class: dns.ClassINET, TTL: 60},
			A: rdata.A{Addr: netip.MustParseAddr(addr)}}}
		return m, nil
	}
}

func rcode(code uint16) func() (*dns.Msg, error) {
	return func() (*dns.Msg, error) {
		m := &dns.Msg{}
		m.Rcode = code
		return m, nil
	}
}

func fail() (*dns.Msg, error) { return nil, errors.New("timeout") }

// refreshNow runs the refresh of the cached entry in the foreground, as Lookup
// would in the background once the TTL has expired.
func (f *fakeCache) refreshNow(t *testing.T) {
	t.Helper()
	k := key{"x.example", "v4"}
	f.mu.Lock()
	f.entries[k].refreshing = true
	f.mu.Unlock()
	f.refresh(k)
}

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

// Protects the end of a record: when a resolver answers NOERROR or NXDOMAIN with
// nothing of the wanted type on refresh, the cached address is forgotten, so the
// next Lookup resolves again and returns the error instead of the old address
// being probed until restart.
func TestRefreshForgetsADefinitivelyMissingRecord(t *testing.T) {
	for name, code := range map[string]uint16{"NOERROR": dns.RcodeSuccess, "NXDOMAIN": dns.RcodeNameError} {
		f := newFake("s1")
		f.reply["s1"] = answerA("192.0.2.1")
		if _, err := f.Lookup(t.Context(), "x.example", "v4"); err != nil {
			t.Fatal(err)
		}
		f.reply["s1"] = rcode(code)
		f.refreshNow(t)
		_, err := f.Lookup(t.Context(), "x.example", "v4")
		if !errors.Is(err, ErrNoRecord) {
			t.Errorf("%s: Lookup after the record was removed = %v, want ErrNoRecord", name, err)
		}
	}
}

// Protects the other half: a resolver that fails, times out, answers SERVFAIL or
// REFUSED, or sends a truncated answer says nothing about the name, so the
// stale address keeps being served.
func TestRefreshKeepsTheStaleAddressWhenResolversFail(t *testing.T) {
	for name, reply := range map[string]func() (*dns.Msg, error){
		"timeout":  fail,
		"SERVFAIL": rcode(dns.RcodeServerFailure),
		"REFUSED":  rcode(dns.RcodeRefused),
		// NOERROR with nothing in it, but cut off: not an answer about the name.
		"truncated": func() (*dns.Msg, error) {
			m := &dns.Msg{}
			m.Rcode, m.Truncated = dns.RcodeSuccess, true
			return m, nil
		},
	} {
		f := newFake("s1")
		f.reply["s1"] = answerA("192.0.2.1")
		if _, err := f.Lookup(t.Context(), "x.example", "v4"); err != nil {
			t.Fatal(err)
		}
		f.reply["s1"] = reply
		f.refreshNow(t)
		addr, err := f.Lookup(t.Context(), "x.example", "v4")
		if err != nil || addr.String() != "192.0.2.1" {
			t.Errorf("%s: Lookup = %v, %v, want the stale 192.0.2.1", name, addr, err)
		}
	}
}

// Protects the first-ever lookup: SERVFAIL from one server is that server
// failing, not "no record", so the next server is asked and its answer used; and
// when every server fails the error is not ErrNoRecord.
func TestResolveTriesTheNextServerAfterSERVFAIL(t *testing.T) {
	f := newFake("s1", "s2")
	f.reply["s1"] = rcode(dns.RcodeServerFailure)
	f.reply["s2"] = answerA("192.0.2.2")
	addr, err := f.Lookup(t.Context(), "x.example", "v4")
	if err != nil || addr.String() != "192.0.2.2" || len(f.asked) != 2 {
		t.Fatalf("Lookup = %v, %v after asking %v, want 192.0.2.2 from the second server", addr, err, f.asked)
	}

	f = newFake("s1", "s2")
	f.reply["s1"] = rcode(dns.RcodeServerFailure)
	f.reply["s2"] = rcode(dns.RcodeRefused)
	if _, err := f.Lookup(t.Context(), "y.example", "v4"); err == nil || errors.Is(err, ErrNoRecord) {
		t.Errorf("every server failing = %v, want an error that is not ErrNoRecord", err)
	}
}
