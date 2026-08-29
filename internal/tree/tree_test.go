package tree

import "testing"

func ptr[T any](v T) *T { return &v }

func testTargets() []Target {
	return []Target{
		{ID: 1, Name: "", Enabled: true, Settings: Settings{
			IntervalS:        ptr(60),
			PingsPerInterval: ptr(20),
			ProbeMode:        ptr("burst"),
			BurstGapMS:       ptr(10),
			TimeoutMS:        ptr(1000),
			PacketSize:       ptr(56),
			DSCP:             ptr(0),
			Agents:           ptr("local"),
			TraceIntervalS:   ptr(300),
		}},
		{ID: 2, ParentID: ptr(int64(1)), Name: "Production", Enabled: true,
			Settings: Settings{IntervalS: ptr(30)}},
		{ID: 3, ParentID: ptr(int64(2)), Name: "cloudflare-v4", Enabled: true,
			Host: ptr("1.1.1.1"), AddressFamily: ptr("v4"),
			Settings: Settings{PingsPerInterval: ptr(40)}},
	}
}

func TestResolveProvenance(t *testing.T) {
	tr, err := New(testTargets())
	if err != nil {
		t.Fatal(err)
	}
	res, err := tr.Resolve(3)
	if err != nil {
		t.Fatal(err)
	}

	// Local override on the leaf itself.
	if res.PingsPerInterval.Effective != 40 || res.PingsPerInterval.Source.ID != 3 {
		t.Errorf("pings = %+v, want effective 40 from node 3", res.PingsPerInterval)
	}
	if res.PingsPerInterval.Local == nil || *res.PingsPerInterval.Local != 40 {
		t.Errorf("pings local = %v, want 40", res.PingsPerInterval.Local)
	}

	// Inherited from the intermediate group.
	iv := res.IntervalS
	if iv.Effective != 30 || iv.Source.ID != 2 || iv.Source.Path != "/Production" || iv.Local != nil {
		t.Errorf("interval = %+v, want effective 30 inherited from /Production", iv)
	}

	// Inherited from the root.
	if res.ProbeMode.Effective != "burst" || res.ProbeMode.Source.ID != 1 || res.ProbeMode.Source.Path != "/" {
		t.Errorf("probe_mode = %+v, want burst inherited from root", res.ProbeMode)
	}
}

func TestPath(t *testing.T) {
	tr, err := New(testTargets())
	if err != nil {
		t.Fatal(err)
	}
	for id, want := range map[int64]string{1: "/", 2: "/Production", 3: "/Production/cloudflare-v4"} {
		got, err := tr.Path(id)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("Path(%d) = %q, want %q", id, got, want)
		}
	}
}

func TestValidation(t *testing.T) {
	incompleteRoot := testTargets()
	incompleteRoot[0].Settings.DSCP = nil
	missingParent := testTargets()
	missingParent[2].ParentID = ptr(int64(99))
	twoRoots := testTargets()
	twoRoots[1].ParentID = nil

	for name, targets := range map[string][]Target{
		"incomplete root": incompleteRoot,
		"missing parent":  missingParent,
		"two roots":       twoRoots,
		"no nodes":        nil,
	} {
		if _, err := New(targets); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}
