package probe

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/timdebruijn/smokeng/internal/store"
	"github.com/timdebruijn/smokeng/internal/tree"
)

// bothStore holds one leaf, "dual", measured by the local prober with one
// 500 ms tcp probe per second against the listener of tcpSpec. Path discovery is
// switched off: the tracer needs the engine's resolver, which a test does not
// have.
func bothStore(t *testing.T, family string) (*store.SQLite, tree.Target, TargetSpec) {
	t.Helper()
	spec := tcpSpec(t)
	st, err := store.Open(filepath.Join(t.TempDir(), "both.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	n := tree.Target{ParentID: new(int64(1)), Name: "dual", Enabled: true, Host: new("dual.example.net"),
		AddressFamily: new(family), Settings: tree.Settings{
			IntervalS: new(1), PingsPerInterval: new(1), TimeoutMS: new(500), TraceIntervalS: new(0),
			ProbeType: new("tcp"), ProbePort: new(spec.ProbePort),
		}}
	if err := st.UpsertTarget(t.Context(), &n); err != nil {
		t.Fatal(err)
	}
	return st, n, spec
}

func setFamily(t *testing.T, st *store.SQLite, n tree.Target, family string) {
	t.Helper()
	n.AddressFamily = new(family)
	if err := st.UpsertTarget(t.Context(), &n); err != nil {
		t.Fatal(err)
	}
}

// bothEngine answers a lookup for v4 only, with loopback, as a host with no
// IPv6 DNS answer would. Its rows are read from results.
func bothEngine(st store.Store) *Engine {
	return &Engine{
		st: st,
		lookup: func(_ context.Context, _, family string) (netip.Addr, error) {
			if family == "v4" {
				return netip.MustParseAddr("127.0.0.1"), nil
			}
			return netip.Addr{}, errors.New("no AAAA record")
		},
		noRoute: func(netip.Addr, string) bool { return false },
		results: make(chan store.Measurement, 256),
		conns:   map[connKey]*conn{},
		running: map[SpecKey]*runningTarget{},
		warned:  map[int64]string{},
	}
}

// rowsFor collects what the loops emit for d.
func rowsFor(e *Engine, d time.Duration) (out []store.Measurement) {
	end := time.After(d)
	for {
		select {
		case m := <-e.results:
			out = append(out, m)
		case <-end:
			return out
		}
	}
}

func familiesOf(rows []store.Measurement) map[tree.Family]int {
	out := map[tree.Family]int{}
	for _, m := range rows {
		out[m.Family]++
	}
	return out
}

// Protects the fan-out of a "both" target into series: it is two specs with
// the same target id, one per family, and otherwise identical. A target of one
// family stays one spec, so nothing existing changes.
func TestABothTargetIsTwoSpecs(t *testing.T) {
	st, n, _ := bothStore(t, "both")
	specs, err := Specs(t.Context(), st)
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 2 {
		t.Fatalf("got %d specs, want one per family: %+v", len(specs), specs)
	}
	v4, v6 := specs[SpecKey{n.ID, "v4"}], specs[SpecKey{n.ID, "v6"}]
	if v4.Family != "v4" || v6.Family != "v6" || v4.TargetID != n.ID || v6.TargetID != n.ID {
		t.Fatalf("specs = %+v / %+v", v4, v6)
	}
	v6.Family = "v4"
	if v4 != v6 {
		t.Errorf("the two specs differ in more than the family:\n%+v\n%+v", v4, v6)
	}
	setFamily(t, st, n, "v6")
	if specs, _ := Specs(t.Context(), st); len(specs) != 1 || specs[SpecKey{n.ID, "v6"}].Family != "v6" {
		t.Errorf("a v6 target is not one v6 spec: %+v", specs)
	}
}

// Protects the intended result on a host that can reach one family only: with
// the lookup answering for v4 alone, the v4 series is measured and the v6 series
// is no data for "no address", in the same intervals, one row per family each.
// A shared loop, or a v6 failure that stopped the v4 loop, would show here as a
// missing or a wrong row.
func TestABothTargetWithoutAV6AddressIsMeasuredOnV4AndNoDataOnV6(t *testing.T) {
	quietLog(t)
	st, _, _ := bothStore(t, "both")
	e := bothEngine(st)
	ctx, cancel := context.WithCancel(t.Context())
	defer func() { cancel(); e.targetWG.Wait() }()
	e.reload(ctx)
	if len(e.running) != 2 {
		t.Fatalf("%d loops running, want 2", len(e.running))
	}

	rows := rowsFor(e, 3600*time.Millisecond)
	byTS := map[int64]map[tree.Family]store.Measurement{}
	for _, m := range rows {
		if byTS[m.TS] == nil {
			byTS[m.TS] = map[tree.Family]store.Measurement{}
		}
		if _, dup := byTS[m.TS][m.Family]; dup {
			t.Errorf("two rows for %v at %d", m.Family, m.TS)
		}
		byTS[m.TS][m.Family] = m
	}
	both := 0
	for ts, fams := range byTS {
		v4, v6 := fams[tree.FamilyV4], fams[tree.FamilyV6]
		if len(fams) != 2 {
			continue // the first or last interval may be only half delivered
		}
		both++
		if v4.Sent != 1 || v4.Received != 1 {
			t.Errorf("v4 at %d = %+v, want one probe sent and answered", ts, v4)
		}
		if v6.Sent != 0 || v6.Flags != store.FlagSendFailed || v6.SendErr == nil || *v6.SendErr != store.SendReasonNoAddress {
			t.Errorf("v6 at %d = %+v, want no data, no address", ts, v6)
		}
	}
	if both < 2 {
		t.Errorf("only %d intervals have a row for each family: %v", both, familiesOf(rows))
	}
}

// Protects the reload diff being per series. Changing a target from both to v4
// stops the v6 loop and leaves the v4 loop running, the same one (not stopped
// and started again, which would drop an interval); changing it back starts a v6
// loop next to the untouched v4 one. Keyed by target, the v4 change would have
// cancelled or replaced the v4 loop, and the v6 loop would have outlived it.
func TestChangingBothToV4AndBackTouchesOnlyTheV6Loop(t *testing.T) {
	quietLog(t)
	st, n, _ := bothStore(t, "both")
	e := bothEngine(st)
	ctx, cancel := context.WithCancel(t.Context())
	defer func() { cancel(); e.targetWG.Wait() }()
	v4Key, v6Key := SpecKey{n.ID, "v4"}, SpecKey{n.ID, "v6"}
	e.reload(ctx)
	v4Loop := e.running[v4Key]
	if v4Loop == nil || e.running[v6Key] == nil {
		t.Fatalf("running = %v, want both loops", e.running)
	}

	setFamily(t, st, n, "v4")
	e.reload(ctx)
	if e.running[v4Key] != v4Loop || e.running[v6Key] != nil || len(e.running) != 1 {
		t.Fatalf("after both -> v4 running = %v, want the same v4 loop alone", e.running)
	}
	// A v6 interval already under way when the loop was cancelled may still
	// arrive; a second after that, only v4 does, and it does keep arriving.
	rowsFor(e, 1700*time.Millisecond)
	got := familiesOf(rowsFor(e, 2200*time.Millisecond))
	if got[tree.FamilyV6] != 0 || got[tree.FamilyV4] == 0 {
		t.Errorf("rows after both -> v4 by family = %v, want v4 only", got)
	}

	setFamily(t, st, n, "both")
	e.reload(ctx)
	if e.running[v4Key] != v4Loop || e.running[v6Key] == nil {
		t.Fatalf("after v4 -> both running = %v, want the v4 loop untouched and a v6 loop", e.running)
	}
	if got := familiesOf(rowsFor(e, 2600*time.Millisecond)); got[tree.FamilyV6] == 0 || got[tree.FamilyV4] == 0 {
		t.Errorf("rows after v4 -> both by family = %v, want both", got)
	}
}

// lockedBuffer is a log destination the loops may write to concurrently.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// Protects the resolution log of a target measured over both: each loop
// compares what it resolves with the last address of its own family. The log
// already holds one address per family, as after any earlier run; with the last
// address read as that of any family, the loop of the family logged first would
// start from the other's address and log a change that never happened.
func TestTwoLoopsOfOneTargetDoNotLogEachOthersAddressAsAChange(t *testing.T) {
	st, n, spec := bothStore(t, "both")
	ctx := t.Context()
	if err := st.RecordResolution(ctx, n.ID, tree.FamilyV4, 100, "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordResolution(ctx, n.ID, tree.FamilyV6, 200, "::1"); err != nil {
		t.Fatal(err)
	}
	var out lockedBuffer
	log.SetOutput(&out)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	e := bothEngine(st)
	e.lookup = func(_ context.Context, _, family string) (netip.Addr, error) {
		if family == "v4" {
			return netip.MustParseAddr("127.0.0.1"), nil
		}
		return netip.MustParseAddr("::1"), nil
	}
	e.noRoute = func(netip.Addr, string) bool { return true } // nothing is sent; the intervals are no data
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	for _, fam := range []string{"v4", "v6"} {
		s := spec
		s.TargetID, s.Family = n.ID, fam
		wg.Go(func() { e.runTarget(ctx, s) })
	}
	rowsFor(e, 2500*time.Millisecond)
	cancel()
	wg.Wait()
	if strings.Contains(out.String(), "address changed") {
		t.Errorf("an address change was logged for addresses that did not change:\n%s", out.String())
	}
}
