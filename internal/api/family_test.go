package api

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/apache/arrow-go/v18/arrow/array"

	"github.com/timdebruijn/smokeng/internal/alert"
	"github.com/timdebruijn/smokeng/internal/store"
	"github.com/timdebruijn/smokeng/internal/tree"
)

const famBase, famRows = int64(1_756_400_000), 6

// familyServer is a v4 target with, at the same famRows timestamps, rows under
// each family told apart by what they hold: v4 answered every probe (sent 1), v6
// lost them all (sent 2), unknown lost every other one (sent 3). With only is
// non-empty, just those families are written, which is what a target that never
// changed family looks like.
func familyServer(t *testing.T, only ...tree.Family) (h http.Handler, id int64) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "family.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	leaf := tree.Target{ParentID: new(int64(1)), Name: "web", Enabled: true, Host: new("192.0.2.1"), AddressFamily: new("v4")}
	if err := st.UpsertTarget(t.Context(), &leaf); err != nil {
		t.Fatal(err)
	}
	for _, f := range []tree.Family{tree.FamilyV4, tree.FamilyV6, tree.FamilyUnknown} {
		if len(only) > 0 && !contains(only, f) {
			continue
		}
		var ms []store.Measurement
		for i := range famRows {
			m := store.Measurement{TargetID: leaf.ID, Family: f, TS: famBase + int64(i)*60, Sent: 1, Received: 1, Samples: []uint32{100}}
			switch f {
			case tree.FamilyV6:
				m.Sent, m.Received, m.Samples = 2, 0, []uint32{}
			case tree.FamilyUnknown:
				m.Sent, m.Received, m.Samples = 3, i%2, []uint32{}
				if i%2 == 1 {
					m.Samples = []uint32{300}
				}
			}
			ms = append(ms, m)
		}
		if err := st.WriteMeasurements(t.Context(), ms); err != nil {
			t.Fatal(err)
		}
		if err := st.RecordPath(t.Context(), leaf.ID, 0, f, famBase, "10.0.0."+strconv.Itoa(int(f))); err != nil {
			t.Fatal(err)
		}
	}
	return New(st, Options{Alerts: alert.NewManager(st, nil)}, fstest.MapFS{}), leaf.ID
}

func contains(l []tree.Family, f tree.Family) bool {
	for _, x := range l {
		if x == f {
			return true
		}
	}
	return false
}

func famGet(h http.Handler, path string) (int, string) {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	return rec.Code, rec.Body.String()
}

func famQuery(id int64, extra string) string {
	return "target_id=" + strconv.FormatInt(id, 10) + "&from=" + strconv.FormatInt(famBase, 10) +
		"&to=" + strconv.FormatInt(famBase+3600, 10) + extra
}

// sentOf is the sent column of a raw measurements response.
func sentOf(t *testing.T, body string) []uint16 {
	t.Helper()
	col := column(readTable(t, []byte(body)), "sent").(*array.Uint16)
	return col.Uint16Values()
}

// Protects the raw measurements read: without family= it is the target's
// current family alone and byte-identical to what a target that never changed
// family returns (so existing clients see nothing new); family=v6 and unknown
// read their own rows at the same timestamps; a bad value is a 400 naming the
// accepted ones. A handler that ignored the parameter would return the v4 rows
// for all three.
func TestMeasurementsReadsTheFamilyAsked(t *testing.T) {
	h, id := familyServer(t)
	ref, refID := familyServer(t, tree.FamilyV4)

	code, def := famGet(h, "/api/v1/measurements?"+famQuery(id, ""))
	_, want := famGet(ref, "/api/v1/measurements?"+famQuery(refID, ""))
	if code != 200 || def != want {
		t.Errorf("default read (%d) differs from a single-family target's", code)
	}
	for _, tc := range []struct {
		family string
		sent   uint16
	}{{"v4", 1}, {"v6", 2}, {"unknown", 3}} {
		code, body := famGet(h, "/api/v1/measurements?"+famQuery(id, "&family="+tc.family))
		got := sentOf(t, body)
		if code != 200 || len(got) != famRows || got[0] != tc.sent || got[famRows-1] != tc.sent {
			t.Errorf("family=%s: %d, sent %v, want %d rows of %d", tc.family, code, got, famRows, tc.sent)
		}
	}
	for _, bad := range []string{"bogus", "V4", "4", ""} {
		code, body := famGet(h, "/api/v1/measurements?"+famQuery(id, "&family="+bad))
		if code != 400 || !strings.Contains(body, "v4, v6 or unknown") {
			t.Errorf("family=%q: %d %s, want 400 naming the values", bad, code, body)
		}
	}
}

// Protects the pooled read (columns=), which reaches seriesQuery the same way:
// the default is the current family's bytes, the other families are not.
func TestPooledMeasurementsReadTheFamilyAsked(t *testing.T) {
	h, id := familyServer(t)
	ref, refID := familyServer(t, tree.FamilyV4)
	_, want := famGet(ref, "/api/v1/measurements?"+famQuery(refID, "&columns=2"))
	_, def := famGet(h, "/api/v1/measurements?"+famQuery(id, "&columns=2"))
	if def != want {
		t.Error("default pooled read differs from a single-family target's")
	}
	seen := map[string]string{"": def}
	for _, f := range []string{"v6", "unknown"} {
		code, body := famGet(h, "/api/v1/measurements?"+famQuery(id, "&columns=2&family="+f))
		if code != 200 {
			t.Fatalf("family=%s: %d", f, code)
		}
		for other, b := range seen {
			if body == b {
				t.Errorf("pooled family=%s is the same bytes as family=%q", f, other)
			}
		}
		seen[f] = body
	}
	if code, _ := famGet(h, "/api/v1/measurements?"+famQuery(id, "&columns=2&family=bogus")); code != 400 {
		t.Errorf("pooled family=bogus: %d, want 400", code)
	}
}

// Protects the paths route: the default is the current family's route changes
// and says which family that is; family= reads the others; bad is 400.
func TestPathsReadTheFamilyAsked(t *testing.T) {
	h, id := familyServer(t)
	for _, tc := range []struct{ q, family, hops string }{
		{"", "v4", "10.0.0.4"}, {"&family=v6", "v6", "10.0.0.6"}, {"&family=unknown", "unknown", "10.0.0.0"},
	} {
		code, body := famGet(h, "/api/v1/paths?"+famQuery(id, tc.q))
		if code != 200 || !strings.Contains(body, `"family":"`+tc.family+`"`) || !strings.Contains(body, `"`+tc.hops+`"`) {
			t.Errorf("paths%s: %d %s, want family %s hops %s", tc.q, code, body, tc.family, tc.hops)
		}
	}
	if code, _ := famGet(h, "/api/v1/paths?"+famQuery(id, "&family=bogus")); code != 400 {
		t.Errorf("paths family=bogus: %d, want 400", code)
	}
}

// Protects availability, which parses its own parameters: the report is over the
// family asked (v4 never down, v6 always, unknown every other interval), the
// response says which, and a bad value is 400. Ignoring family= would report the
// v4 numbers each time.
func TestAvailabilityReadsTheFamilyAsked(t *testing.T) {
	h, id := familyServer(t)
	for _, tc := range []struct{ q, family, down string }{
		{"", "v4", `"down_intervals":0`}, {"&family=v6", "v6", `"down_intervals":6`}, {"&family=unknown", "unknown", `"down_intervals":3`},
	} {
		code, body := famGet(h, "/api/v1/availability?"+famQuery(id, tc.q+"&agent_id=0"))
		if code != 200 || !strings.Contains(body, `"family":"`+tc.family+`"`) || !strings.Contains(body, tc.down) {
			t.Errorf("availability%s: %d %s, want family %s and %s", tc.q, code, body, tc.family, tc.down)
		}
	}
	if code, body := famGet(h, "/api/v1/availability?"+famQuery(id, "&family=bogus")); code != 400 || !strings.Contains(body, "v4, v6 or unknown") {
		t.Errorf("availability family=bogus: %d %s, want 400", code, body)
	}
}

// Protects the scope check: family= does not open a target the caller cannot
// read. A foreign target answers the same 404 body with and without it, on every
// route that takes it, as one that does not exist.
func TestFamilyDoesNotOpenAForeignTarget(t *testing.T) {
	f := tenants(t)
	a := f.server(t, "a", f.groupA, "viewer")
	for _, route := range []string{"measurements", "paths", "availability"} {
		for _, extra := range []string{"&family=v4", "&family=unknown"} {
			base := "/api/v1/" + route + "?target_id=" + num(f.hostB) + "&from=0&to=100"
			c0, b0 := call(t, a, "GET", base, nil)
			c1, b1 := call(t, a, "GET", base+extra, nil)
			_, bm := call(t, a, "GET", "/api/v1/"+route+"?target_id=99999&from=0&to=100"+extra, nil)
			if c0 != 404 || c1 != 404 || b0 != b1 || b1 != bm {
				t.Errorf("%s%s: %d %q without, %d %q with, %q for a missing target", route, extra, c0, b0, c1, b1, bm)
			}
		}
		// A value that is refused is refused alike for a target outside the
		// caller's scope and one that does not exist. The routes check in a
		// different order (400 first on two, 404 first on the third), and in
		// neither order may the answer tell the two targets apart.
		cf, bf := call(t, a, "GET", "/api/v1/"+route+"?target_id="+num(f.hostB)+"&from=0&to=100&family=bogus", nil)
		cm, bm := call(t, a, "GET", "/api/v1/"+route+"?target_id=99999&from=0&to=100&family=bogus", nil)
		if cf != cm || bf != bm || (cf != 400 && cf != 404) {
			t.Errorf("%s with a bad family: %d %q for a foreign target, %d %q for a missing one", route, cf, bf, cm, bm)
		}
	}
}
