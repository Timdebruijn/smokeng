package config

import (
	"context"
	"strings"
	"testing"
)

// graph_series has to survive a round trip through TOML, in both directions.
//
// This is the same shape as the bug that disabled three shape rules on a real
// deployment: a setting the API and the UI understood but the file format did
// not, so the declarative import — where absence means "not set" — quietly
// reverted it on the next run. A setting that only half exists is worse than
// one that does not exist at all.
func TestGraphSeriesRoundTripsThroughTOML(t *testing.T) {
	ctx := context.Background()
	st := open(t)
	data := []byte(`
[defaults]
graph_series = "all"

[targets.'wag']
host = "10.0.0.1"
address_family = "v4"

[targets.'wag/Irtt']
host = "irtt.example.org"
address_family = "v4"
probe_type = "irtt"
graph_series = "ipdv_send ipdv_receive"
`)
	if _, err := Import(ctx, st, data, false, AllowUnknownAgents()); err != nil {
		t.Fatalf("import: %v", err)
	}
	tr, byPath := mustTree(t, st)
	res, err := tr.Resolve(byPath["/wag/Irtt"].ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.GraphSeries.Effective != "ipdv_send ipdv_receive" {
		t.Errorf("graph_series = %q, want the two directional series", res.GraphSeries.Effective)
	}
	// The sibling group never set it, so it inherits the root's default.
	resWag, err := tr.Resolve(byPath["/wag"].ID)
	if err != nil {
		t.Fatal(err)
	}
	if resWag.GraphSeries.Effective != "all" {
		t.Errorf("inherited graph_series = %q, want all", resWag.GraphSeries.Effective)
	}

	out, err := Export(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "ipdv_send ipdv_receive") {
		t.Errorf("export dropped graph_series:\n%s", out)
	}

	// Actually round-trip it. The earlier version of this test exported and
	// grepped, which proves the value appears somewhere — not that it comes
	// back on the same node, nor that the root's own default survives. A bug
	// that wrote it under the wrong key, or dropped it from [defaults] while
	// keeping it on target entries, passed.
	st2 := open(t)
	if _, err := Import(ctx, st2, out, false, AllowUnknownAgents()); err != nil {
		t.Fatalf("re-importing our own export: %v", err)
	}
	tr2, byPath2 := mustTree(t, st2)
	for path, want := range map[string]string{
		"/wag/Irtt": "ipdv_send ipdv_receive",
		"/wag":      "all",
		"/":         "all",
	} {
		n, ok := byPath2[path]
		if !ok {
			t.Errorf("%s missing after the round trip", path)
			continue
		}
		res, err := tr2.Resolve(n.ID)
		if err != nil {
			t.Fatal(err)
		}
		if res.GraphSeries.Effective != want {
			t.Errorf("%s: graph_series = %q after the round trip, want %q",
				path, res.GraphSeries.Effective, want)
		}
	}
	// And the root carries it as its own value, not by inheriting from nowhere.
	root := byPath2["/"]
	if root.Settings.GraphSeries == nil || *root.Settings.GraphSeries != "all" {
		t.Errorf("the root's own graph_series did not survive: %v", root.Settings.GraphSeries)
	}
}

// A typo must be refused at import rather than stored, where it would draw
// nothing and look exactly like a link with no jitter.
func TestGraphSeriesRejectsUnknownName(t *testing.T) {
	_, err := Import(context.Background(), open(t), []byte(`
[targets.'a']
host = "10.0.0.1"
address_family = "v4"
graph_series = "ipdv_sideways"
`), false, AllowUnknownAgents())
	if err == nil {
		t.Fatal("an unknown series name was accepted")
	}
	if !strings.Contains(err.Error(), "ipdv_sideways") {
		t.Errorf("error does not name the offending value: %v", err)
	}
}
