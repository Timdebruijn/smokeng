package api

import (
	"bytes"
	"crypto/ed25519"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/ipc"

	"github.com/timdebruijn/smokeng/internal/ingest"
	"github.com/timdebruijn/smokeng/internal/store"
	"github.com/timdebruijn/smokeng/internal/tree"
)

// postSigned signs and sends a body to an agent route.
func postSigned(t *testing.T, h http.Handler, route string, id int64, key ed25519.PrivateKey, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", route, bytes.NewReader(body))
	if err := ingest.Sign(req, id, key, body, time.Now()); err != nil {
		t.Fatal(err)
	}
	req.Body = io.NopCloser(bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// batchWithoutFamily is a current batch with the family column cut out, as an
// agent built before it would send it.
func batchWithoutFamily(t *testing.T, ms []store.Measurement) []byte {
	t.Helper()
	body, err := ingest.EncodeBatch(ms)
	if err != nil {
		t.Fatal(err)
	}
	r, err := ipc.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Release()
	r.Next()
	rec := r.Record()
	var fields []arrow.Field
	var cols []arrow.Array
	for i, f := range rec.Schema().Fields() {
		if f.Name != "family" {
			fields = append(fields, f)
			cols = append(cols, rec.Column(i))
		}
	}
	schema := arrow.NewSchema(fields, nil)
	out := array.NewRecord(schema, cols, rec.NumRows())
	defer out.Release()
	var buf bytes.Buffer
	w := ipc.NewWriter(&buf, ipc.WithSchema(schema))
	if err := w.Write(out); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// Protects where a row an agent delivers is filed, and whether the rules see it.
// The target is v4. A row without a family (an older agent, or null) goes under
// v4 and is evaluated; one stating v4 does the same; one stating v6 is stored
// under v6, readable there and nowhere else, and fires nothing, though the same
// two lossy intervals fire the loss rule when they are v4. Each case is a fresh
// master, so what fired is what that batch alone did.
func TestIngestFilesRowsUnderTheFamilyTheAgentSays(t *testing.T) {
	const t0 = 1_756_400_000
	cases := []struct {
		name       string
		family     tree.Family
		noColumn   bool
		wantUnder  tree.Family
		wantFiring bool
	}{
		{"no column", tree.FamilyUnknown, true, tree.FamilyV4, true},
		{"null", tree.FamilyUnknown, false, tree.FamilyV4, true},
		{"the current family", tree.FamilyV4, false, tree.FamilyV4, true},
		{"the other family", tree.FamilyV6, false, tree.FamilyV6, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, st, mgr, agentID, key, mine, _ := ingestFixtureAlerts(t)
			lossRule(t, st, mgr)
			ms := []store.Measurement{measurement(mine, t0), measurement(mine, t0+60)}
			for i := range ms {
				ms[i].Family = c.family
			}
			var rec *httptest.ResponseRecorder
			if c.noColumn {
				rec = postSigned(t, h, "/api/v1/ingest", agentID, key, batchWithoutFamily(t, ms))
			} else {
				rec = submit(t, h, agentID, key, ms, time.Now())
			}
			if rec.Code != http.StatusOK {
				t.Fatalf("ingest = %d: %s", rec.Code, rec.Body)
			}
			for _, fam := range []tree.Family{tree.FamilyV4, tree.FamilyV6} {
				got, err := st.QueryRange(t.Context(), mine, agentID, fam, 0, 1<<40)
				if err != nil {
					t.Fatal(err)
				}
				want := 0
				if fam == c.wantUnder {
					want = 2
				}
				if len(got) != want {
					t.Errorf("%v holds %d rows, want %d", fam, len(got), want)
				}
			}
			if firing := len(mgr.Firing()) > 0; firing != c.wantFiring {
				t.Errorf("rule firing = %v, want %v: %+v", firing, c.wantFiring, mgr.Firing())
			}
		})
	}
}

// Protects the same for path changes: the family an agent states is the one the
// change is stored under, and absent means the target's current one. So does a
// family that is not "v4" or "v6" (a misspelling, "unknown", a number): the
// change is kept and filed as if it stated none, not dropped for its label.
func TestAgentPathsAreFiledUnderTheFamilyTheAgentSays(t *testing.T) {
	h, st, agentID, key, mine, _ := ingestFixture(t)
	now := time.Now().Unix()
	body := `{"paths":[
		{"target_id":` + itoa(mine) + `,"ts":` + itoa(now-900) + `,"hops":["10.0.0.1","192.0.2.9"]},
		{"target_id":` + itoa(mine) + `,"ts":` + itoa(now-800) + `,"hops":["10.0.0.2","192.0.2.9"],"family":"v4"},
		{"target_id":` + itoa(mine) + `,"ts":` + itoa(now-700) + `,"hops":["10.0.0.3","192.0.2.9"],"family":"v6"},
		{"target_id":` + itoa(mine) + `,"ts":` + itoa(now-600) + `,"hops":["10.0.0.4","192.0.2.9"],"family":"v5"},
		{"target_id":` + itoa(mine) + `,"ts":` + itoa(now-500) + `,"hops":["10.0.0.5","192.0.2.9"],"family":"unknown"},
		{"target_id":` + itoa(mine) + `,"ts":` + itoa(now-400) + `,"hops":["10.0.0.6","192.0.2.9"],"family":6}
	]}`
	rec := postSigned(t, h, "/api/v1/agent/paths", agentID, key, []byte(body))
	if rec.Code != http.StatusOK {
		t.Fatalf("paths = %d: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"accepted":6`) {
		t.Errorf("response %s, want all 6 accepted: a family that cannot be read does not drop the change", rec.Body)
	}
	hopsOf := func(fam tree.Family) (out []string) {
		got, err := st.PathChanges(t.Context(), mine, agentID, fam, 0, now+60)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range got {
			out = append(out, c.Hops)
		}
		return out
	}
	want := "10.0.0.1,192.0.2.9 10.0.0.2,192.0.2.9 10.0.0.4,192.0.2.9 10.0.0.5,192.0.2.9 10.0.0.6,192.0.2.9"
	if got := strings.Join(hopsOf(tree.FamilyV4), " "); got != want {
		t.Errorf("v4, the target's own, holds %q, want every change but the one stating v6", got)
	}
	if got := strings.Join(hopsOf(tree.FamilyV6), " "); got != "10.0.0.3,192.0.2.9" {
		t.Errorf("v6 holds %q, want only the change stating v6", got)
	}
}

// Protects the ingest of a target measured over both families: a row stating
// v4 or v6 is stored under it and evaluated (each family has its own state, so
// the same two lossy intervals fire the rule once per family), and a row stating
// none is stored under v4, the default, and evaluated there. Read against a
// single family this would store or evaluate only one of the two series.
func TestIngestOfABothTargetStoresAndEvaluatesEachFamily(t *testing.T) {
	const t0 = 1_756_400_000
	h, st, mgr, agentID, key, mine, _ := ingestFixtureAlerts(t)
	tg := tree.Target{ID: mine, ParentID: new(int64(1)), Name: "mine", Enabled: true,
		Host: new("mine.example.net"), AddressFamily: new("both"), Settings: tree.Settings{Agents: new("ams")}}
	if err := st.UpsertTarget(t.Context(), &tg); err != nil {
		t.Fatal(err)
	}
	lossRule(t, st, mgr)

	var ms []store.Measurement
	for i, fam := range []tree.Family{tree.FamilyV4, tree.FamilyV4, tree.FamilyV6, tree.FamilyV6, tree.FamilyUnknown} {
		m := measurement(mine, t0+int64(i%2)*60)
		m.Family = fam
		if fam == tree.FamilyUnknown {
			m.TS = t0 + 120 // a row stating none: after v4's two, so it is not a duplicate
		}
		ms = append(ms, m)
	}
	if rec := submit(t, h, agentID, key, ms, time.Now()); rec.Code != http.StatusOK {
		t.Fatalf("ingest = %d: %s", rec.Code, rec.Body)
	}
	for fam, want := range map[tree.Family]int{tree.FamilyV4: 3, tree.FamilyV6: 2} {
		got, err := st.QueryRange(t.Context(), mine, agentID, fam, 0, 1<<40)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != want {
			t.Errorf("%v holds %d rows, want %d (the row stating none belongs under v4)", fam, len(got), want)
		}
	}
	fired := map[tree.Family]bool{}
	for _, a := range mgr.Firing() {
		fired[a.Family] = true
	}
	if !fired[tree.FamilyV4] || !fired[tree.FamilyV6] {
		t.Errorf("firing by family = %v, want the loss rule firing for v4 and for v6", fired)
	}
}
