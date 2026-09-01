package store

import (
	"os"
	"regexp"
	"strconv"
	"testing"
)

// Every send reason Go can record must have a name in the browser.
//
// The browser falls back to "reason N" for a code it does not know, which is
// the right behaviour for data written by a *newer* prober than the page. It
// is the wrong behaviour for a code the same release defines, and that is
// exactly what happened: two reasons were added here and the map on the other
// side was not updated, so the UI would have printed "reason 8" for a case
// this project added a code to describe.
//
// Reading the TypeScript from a Go test is unusual. It is also the only thing
// that fails when the two drift, and they have drifted once already.
func TestEveryReasonHasABrowserName(t *testing.T) {
	src, err := os.ReadFile("../../web/src/api.ts")
	if err != nil {
		t.Skipf("no frontend source to check against: %v", err)
	}
	block := regexp.MustCompile(`(?s)const SEND_REASONS: Record<number, string> = \{(.*?)\n\}`).
		FindSubmatch(src)
	if block == nil {
		t.Fatal("SEND_REASONS not found in web/src/api.ts; if it was renamed, rename it here too")
	}
	known := map[int]bool{}
	for _, m := range regexp.MustCompile(`(?m)^\s*(\d+):`).FindAllSubmatch(block[1], -1) {
		n, _ := strconv.Atoi(string(m[1]))
		known[n] = true
	}
	for r := int(SendReasonSocket); r <= int(SendReasonDeadline); r++ {
		if !known[r] {
			t.Errorf("reason %d (%q) has no entry in SEND_REASONS, so the graph would call it %q",
				r, SendReasonName(uint8(r)), "reason "+strconv.Itoa(r))
		}
	}
	// And nothing in the browser that Go cannot produce, which would be a name
	// for a code that never arrives.
	for r := range known {
		if r < int(SendReasonSocket) || r > int(SendReasonDeadline) {
			t.Errorf("SEND_REASONS names reason %d, which this prober never records", r)
		}
	}
}
