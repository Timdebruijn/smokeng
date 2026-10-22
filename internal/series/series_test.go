package series

import (
	"slices"
	"strings"
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
	_, _, err := ParseSelection("ipdv_sideways")
	if err == nil || !strings.Contains(err.Error(), "ipdv_sideways") {
		t.Fatalf("err = %v, want one naming the offending value", err)
	}
	// It also lists what is valid, so the message is actionable on its own.
	for _, want := range append(slices.Clone(All), SelectAll) {
		if !strings.Contains(err.Error(), want) {
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
	_, _, err := ParseSelection(IPDVSend + " " + IPDVSend)
	if err == nil || !strings.Contains(err.Error(), IPDVSend) {
		t.Fatalf("err = %v, want one naming %q", err, IPDVSend)
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
