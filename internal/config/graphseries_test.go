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
