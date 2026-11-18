package config

import (
	"context"
	"strings"
	"testing"
)

// positional_loss has to survive TOML in both directions, on [defaults] and on
// a target, and export has to write it only where it is set locally. Absence in
// a file means "inherit", so a field the file format drops is reverted on the
// next import. The root is set to the non-default so that [defaults] actually
// has to carry it.
func TestPositionalLossRoundTripsThroughTOML(t *testing.T) {
	ctx := context.Background()
	st := open(t)
	if _, err := Import(ctx, st, []byte(`
[defaults]
positional_loss = "flag_only"

[targets.'lab']
positional_loss = "exclude"

[targets.'lab/gw']
host = "10.0.0.1"
address_family = "v4"

[targets.'prod/gw']
host = "10.0.1.1"
address_family = "v4"
`), false, true); err != nil {
		t.Fatalf("import: %v", err)
	}
	out, err := Export(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	// Written where it is set: the root and /lab. Not on the leaves, which only
	// inherit it.
	if n := strings.Count(string(out), "positional_loss"); n != 2 {
		t.Errorf("export wrote positional_loss %d times, want 2 (root and lab):\n%s", n, out)
	}

	st2 := open(t)
	if _, err := Import(ctx, st2, out, false, true); err != nil {
		t.Fatalf("re-importing our own export: %v", err)
	}
	tr, byPath := mustTree(t, st2)
	for path, want := range map[string]string{
		"/": "flag_only", "/lab": "exclude", "/lab/gw": "exclude", "/prod/gw": "flag_only",
	} {
		n, ok := byPath[path]
		if !ok {
			t.Errorf("%s missing after the round trip", path)
			continue
		}
		res, err := tr.Resolve(n.ID)
		if err != nil {
			t.Fatal(err)
		}
		if res.PositionalLoss.Effective != want {
			t.Errorf("%s: positional_loss = %q, want %q", path, res.PositionalLoss.Effective, want)
		}
	}
	if byPath["/lab/gw"].Settings.PositionalLoss != nil {
		t.Error("a leaf that only inherits it gained a local value")
	}
}

// A typo is refused at import, with a message that names the value.
func TestPositionalLossRejectsUnknownValue(t *testing.T) {
	_, err := Import(context.Background(), open(t), []byte(`
[targets.'a']
host = "10.0.0.1"
address_family = "v4"
positional_loss = "ignore"
`), false, true)
	if err == nil || !strings.Contains(err.Error(), "ignore") {
		t.Fatalf("want an error naming the value, got %v", err)
	}
}
