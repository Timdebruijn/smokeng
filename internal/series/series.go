// Package series names the extra per-packet distributions a probe can measure
// beside the round trip.
//
// It is its own package because the names are shared vocabulary: the prober
// produces them, the store keeps them, the wire formats carry them, and the
// target tree decides which are drawn. The tree cannot import the store — the
// store already imports the tree — so the names live below both.
package series

import "strings"

const (
	// IPDVSend is inter-packet delay variation on the way out: how much later
	// or earlier each packet reached the far end than the one before it.
	//
	// It is a difference between consecutive packets on the monotonic clock, so
	// the offset between the two hosts' clocks cancels out and it stays
	// meaningful without them being synchronised. Absolute one-way delay, which
	// irtt also reports, does not have that property: it is a subtraction
	// between two machines' wall clocks and is wrong by however far apart they
	// have drifted. That is why smokeng keeps this and not that — one is a
	// measurement of the network, the other is partly a measurement of NTP.
	IPDVSend = "ipdv_send"
	// IPDVReceive is the same measure on the way back. Kept separately because
	// the two directions fail independently, and a round trip cannot say which
	// half of it got worse.
	IPDVReceive = "ipdv_receive"
	// ServerProcessing is how long the far end held each packet between
	// receiving it and replying. It separates a slow peer from a slow path.
	ServerProcessing = "server_processing"
)

// All lists every series name, in the order a UI should offer them.
var All = []string{IPDVSend, IPDVReceive, ServerProcessing}

// Valid reports whether name is a series smokeng knows how to record. An
// unknown name is refused at the door rather than stored and later rendered as
// an unlabelled curve.
func Valid(name string) bool {
	for _, s := range All {
		if s == name {
			return true
		}
	}
	return false
}

// SelectAll is the graph_series value meaning "draw every series that has
// data". It is the default: a measurement smokeng took and did not show is one
// nobody knows to look for.
const SelectAll = "all"

// ParseSelection reads a graph_series setting into the list of series to draw.
// The empty string selects none, leaving only the round trip; SelectAll
// selects everything that has data, which the caller signals by the returned
// bool rather than by a list, since what has data is not known here.
//
// A name that is not a series, or a name given twice, is an error: the setting
// is written by hand and by Ansible, and a typo that silently drew nothing
// would look exactly like a link with no jitter.
func ParseSelection(v string) (names []string, all bool, err error) {
	v = strings.TrimSpace(v)
	if v == SelectAll {
		return nil, true, nil
	}
	if v == "" {
		return nil, false, nil
	}
	seen := map[string]bool{}
	for _, f := range strings.Fields(v) {
		if !Valid(f) {
			return nil, false, &UnknownError{Name: f}
		}
		if seen[f] {
			return nil, false, &DuplicateError{Name: f}
		}
		seen[f] = true
		names = append(names, f)
	}
	return names, false, nil
}

// UnknownError names a series the setting asked for that does not exist.
type UnknownError struct{ Name string }

func (e *UnknownError) Error() string {
	return "unknown series " + e.Name + " (known: " + strings.Join(All, ", ") +
		", or " + SelectAll + ")"
}

// DuplicateError names a series listed more than once.
type DuplicateError struct{ Name string }

func (e *DuplicateError) Error() string { return "series " + e.Name + " listed twice" }
