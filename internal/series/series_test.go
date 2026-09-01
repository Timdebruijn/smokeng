package series

import (
	"errors"
	"slices"
	"testing"
)

func TestParseSelection(t *testing.T) {
	cases := []struct {
		in    string
		names []string
		all   bool
	}{
		{"all", nil, true},
		{"  all  ", nil, true},
		{"", nil, false},
		{"   ", nil, false},
		{IPDVSend, []string{IPDVSend}, false},
		{IPDVReceive + "  " + IPDVSend, []string{IPDVReceive, IPDVSend}, false},
		{"\tipdv_send\n ipdv_receive ", []string{IPDVSend, IPDVReceive}, false},
	}
	for _, c := range cases {
		names, all, err := ParseSelection(c.in)
		if err != nil {
			t.Errorf("ParseSelection(%q): %v", c.in, err)
			continue
		}
		if all != c.all || !slices.Equal(names, c.names) {
			t.Errorf("ParseSelection(%q) = %v, all=%v; want %v, all=%v", c.in, names, all, c.names, c.all)
		}
	}
}

// A name is refused rather than dropped. The setting is written by hand and by
// Ansible, and a typo that silently drew nothing would look exactly like a link
// with no jitter — which is the reading this project exists to prevent.
func TestParseSelectionRefusesTypos(t *testing.T) {
	var unknown *UnknownError
	_, _, err := ParseSelection("ipdv_sideways")
	if !errors.As(err, &unknown) {
		t.Fatalf("err = %v, want *UnknownError", err)
	}
	if unknown.Name != "ipdv_sideways" {
		t.Errorf("the error does not name the offending value: %q", unknown.Name)
	}
	// It also lists what is valid, so the message is actionable on its own.
	for _, want := range append(slices.Clone(All), SelectAll) {
		if !contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}
	// "all" is a whole-value keyword, not a name that can appear in a list.
	if _, _, err := ParseSelection("all ipdv_send"); err == nil {
		t.Error("\"all\" was accepted as a list element")
	}
	// Case matters: the Go validator and the TypeScript reader both compare
	// exactly, and accepting "All" here would make them disagree.
	if _, _, err := ParseSelection("All"); err == nil {
		t.Error("\"All\" was accepted; the browser's reader would not accept it")
	}
}

// A name given twice is a mistake, not a request for two graphs. Nothing
// exercised this branch, so it could have been deleted unnoticed.
func TestParseSelectionRefusesDuplicates(t *testing.T) {
	var dup *DuplicateError
	_, _, err := ParseSelection(IPDVSend + " " + IPDVSend)
	if !errors.As(err, &dup) {
		t.Fatalf("err = %v, want *DuplicateError", err)
	}
	if dup.Name != IPDVSend {
		t.Errorf("DuplicateError names %q", dup.Name)
	}
}

func TestValid(t *testing.T) {
	for _, n := range All {
		if !Valid(n) {
			t.Errorf("Valid(%q) = false", n)
		}
	}
	for _, n := range []string{"", "all", "IPDV_SEND", "ipdv", "server_processing "} {
		if Valid(n) {
			t.Errorf("Valid(%q) = true", n)
		}
	}
}

func contains(hay, needle string) bool {
	return len(needle) > 0 && len(hay) >= len(needle) &&
		(hay == needle || indexOf(hay, needle) >= 0)
}

func indexOf(hay, needle string) int {
	for i := 0; i+len(needle) <= len(hay); i++ {
		if hay[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
