package tree

import (
	"strings"
	"testing"
)

// positional_loss inherits like every other setting, and a value other than
// the two is refused with a message that names both: a typo stored as-is would
// read as neither, and the exclusion would quietly be off or on for a subtree
// that meant the opposite.
func TestPositionalLossInheritsAndValidates(t *testing.T) {
	ts := testTargets()
	ts[1].Settings.PositionalLoss = ptr("flag_only") // /Production
	tr, err := New(ts)
	if err != nil {
		t.Fatal(err)
	}
	res, err := tr.Resolve(3) // a leaf under /Production
	if err != nil {
		t.Fatal(err)
	}
	pl := res.PositionalLoss
	if pl.Effective != "flag_only" || pl.Source.ID != 2 || pl.Local != nil {
		t.Errorf("leaf = %+v, want flag_only inherited from /Production", pl)
	}
	root, _ := tr.Resolve(1)
	if root.PositionalLoss.Effective != "exclude" {
		t.Errorf("root = %+v, want exclude", root.PositionalLoss)
	}

	bad := testTargets()
	bad[1].Settings.PositionalLoss = ptr("both")
	_, err = New(bad)
	if err == nil {
		t.Fatal("an unknown positional_loss was accepted")
	}
	for _, want := range []string{`"exclude"`, `"flag_only"`, `"both"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}

	noDefault := testTargets()
	noDefault[0].Settings.PositionalLoss = nil
	if _, err := New(noDefault); err == nil {
		t.Error("a root without positional_loss was accepted")
	}
}
