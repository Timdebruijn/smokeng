package agent

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"testing"
	"testing/fstest"

	"github.com/timdebruijn/smokeng/internal/api"
	"github.com/timdebruijn/smokeng/internal/probe"
	"github.com/timdebruijn/smokeng/internal/store"
	"github.com/timdebruijn/smokeng/internal/tree"
)

// The parity rule (DESIGN.md §9): an agent measures a target exactly as the
// master's own prober would. A setting the assignments do not carry is one the
// agent fills in from its own defaults, and the two then measure different
// things under one name — which is how trace_interval_s went missing: every
// agent traced at its own root's 300 s whatever the master said.
//
// So: targets with every measuring setting away from its default, assigned to
// both the master's prober and an agent; the agent pulls them through the real
// API; and what each side would measure is compared field by field. A field
// added to probe.TargetSpec must be given a value below, or the coverage check
// fails — a new setting cannot slip past this by being zero everywhere.
func TestAnAgentMeasuresATargetExactlyAsTheMasterWould(t *testing.T) {
	ctx := context.Background()
	master, err := store.Open(filepath.Join(t.TempDir(), "master.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { master.Close() })

	key, _, err := LoadOrCreateKey(filepath.Join(t.TempDir(), "probe.key"))
	if err != nil {
		t.Fatal(err)
	}
	rec, err := master.AddAgent(ctx, "ams-01", key.Public().(ed25519.PublicKey))
	if err != nil {
		t.Fatal(err)
	}

	// Some settings on a group, so inheritance has to be resolved on the way.
	root := int64(1)
	group := tree.Target{ParentID: &root, Name: "g", Enabled: true, Settings: tree.Settings{
		IntervalS: new(30), TraceIntervalS: new(120), Agents: new("local ams-01"),
	}}
	if err := master.UpsertTarget(ctx, &group); err != nil {
		t.Fatal(err)
	}
	leaf := func(name, host string, s tree.Settings) int64 {
		t.Helper()
		n := tree.Target{ParentID: &group.ID, Name: name, Host: new(host), AddressFamily: new("v4"), Enabled: true, Settings: s}
		if err := master.UpsertTarget(ctx, &n); err != nil {
			t.Fatal(err)
		}
		return n.ID
	}
	ids := []int64{
		leaf("icmp", "192.0.2.1", tree.Settings{
			PingsPerInterval: new(7), ProbeMode: new("spread"), BurstGapMS: new(25),
			TimeoutMS: new(900), PacketSize: new(120), DSCP: new(46),
		}),
		leaf("tcp", "192.0.2.2", tree.Settings{ProbeType: new("tcp"), ProbePort: new(8443)}),
		leaf("dns", "192.0.2.3", tree.Settings{ProbeType: new("dns"), DNSQuery: new("example.org"), DNSRRType: new("AAAA")}),
		leaf("https", "192.0.2.4", tree.Settings{ProbeType: new("https"), HTTPPath: new("/health"), TLSSkipVerify: new(true)}),
	}

	srv := httptest.NewServer(api.New(master, api.Options{}, fstest.MapFS{}))
	t.Cleanup(srv.Close)
	buf, err := store.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { buf.Close() })
	a, err := New(Config{Master: srv.URL, AgentID: rec.ID, Insecure: true}, key, buf)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.pull(ctx); err != nil {
		t.Fatalf("pull: %v", err)
	}

	want, err := probe.Specs(ctx, master)
	if err != nil {
		t.Fatal(err)
	}
	got, err := probe.Specs(ctx, buf)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		w, ok := want[id]
		if !ok {
			t.Fatalf("target %d: the master would not measure it", id)
		}
		g, ok := got[id]
		if !ok {
			t.Errorf("target %d: the agent would not measure it", id)
			continue
		}
		if !reflect.DeepEqual(g, w) {
			t.Errorf("target %d: the agent would measure\n%+v\nwhere the master would measure\n%+v", id, g, w)
		}
	}

	// Every field of a spec is away from its zero value on some target, so
	// each one was actually carried rather than agreeing by both being empty.
	typ := reflect.TypeFor[probe.TargetSpec]()
	for f := range typ.NumField() {
		set := false
		for _, id := range ids {
			if !reflect.ValueOf(want[id]).Field(f).IsZero() {
				set = true
			}
		}
		if !set {
			t.Errorf("TargetSpec.%s is zero on every target here; give it a value so parity covers it", typ.Field(f).Name)
		}
	}
}

// A master from before trace_interval_s was carried sends no such field. The
// agent then discovers no path at all, rather than falling back to its own
// root's default: a path it found would have nowhere to go.
func TestAnAssignmentWithoutTraceIntervalTracesNothing(t *testing.T) {
	a, st, _ := newTestAgent(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// As a v0.24 master writes it: no trace_interval_s.
		fmt.Fprint(w, `{"targets":[{"target_id":42,"path":"/a","host":"192.0.2.1","address_family":"v4",`+
			`"interval_s":60,"pings":20,"probe_mode":"burst","burst_gap_ms":10,"timeout_ms":1000,"packet_size":56,"dscp":0,`+
			`"probe_type":"icmp","probe_port":0,"dns_query":"","dns_rr_type":"","http_path":"","tls_skip_verify":false}]}`)
	}))
	ctx := context.Background()
	if err := a.pull(ctx); err != nil {
		t.Fatal(err)
	}
	specs, err := probe.Specs(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	if s, ok := specs[42]; !ok || s.TraceIntervalS != 0 {
		t.Errorf("target 42 = %+v (present %v), want measured with trace_interval_s 0", s, ok)
	}
}
