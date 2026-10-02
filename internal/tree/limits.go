package tree

import (
	"fmt"
	"net/netip"
	"strings"
	"unicode"
	"unicode/utf8"
)

// What smokeng will run, as opposed to what it will merely store.
//
// These are checked when a target is written, through the API or an import, and
// never when the tree is loaded: New runs on every request and in the prober, so
// a limit in it would break everything the moment a database held one value
// above it. Each check also applies only to what a write changes. Someone
// retitling a node whose packet_size predates a limit is not told about the
// packet size.
//
// The numbers are generous next to any real use (smokeng's defaults are 20
// pings of 56 bytes a minute) and far below what lets one editor turn the
// prober into a flood source or exhaust the host: pings_per_interval,
// interval_s, packet_size and timeout_ms had a floor and no useful ceiling, so
// a single request could schedule tens of thousands of probes a second at a
// third party, or hold millions of sockets open waiting on a timeout.
const (
	MaxNameRunes   = 128
	MaxTitleRunes  = 500
	MaxNotesRunes  = 16384
	MaxHostLen     = 253
	MaxDNSQueryLen = 253
	MaxHTTPPathLen = 2048
	MaxAgentsLen   = 1024

	// MaxDepth and MaxTargets bound the tree. Resolving a node walks its whole
	// ancestry once per setting, so cost grows with depth squared; a chain of
	// a few thousand nodes took seconds per request.
	MaxDepth   = 16
	MaxTargets = 10_000

	MaxIntervalS  = 86_400
	MaxPings      = 1000
	MaxBurstGapMS = 600_000
	MaxPacketSize = 9000
	MinTraceS     = 30
	MaxTraceS     = 604_800     // a week; and keeps time.Duration(s)*time.Second from overflowing
	MinRetentionS = 3600        // 0 keeps everything; a positive value deletes, so not 1
	MaxRetentionS = 315_360_000 // ten years

	// MaxProbeRate and MaxProbeBytesPerSec bound one target's average load on
	// whatever it probes. MaxTotalProbeRate bounds the sum over all targets, so
	// that many modest targets are not a flood either.
	MaxProbeRate        = 100     // pings per second
	MaxProbeBytesPerSec = 500_000 // 4 Mbit/s; below MaxProbeRate times MaxPacketSize, or it could never be reached
	MaxTotalProbeRate   = 5000
)

// CheckLimits refuses a node whose changed fields exceed what smokeng will run.
// before is nil for a node that is being created.
func CheckLimits(before, after *Target) error {
	changed := func(f func(*Target) any) bool { return before == nil || f(before) != f(after) }
	str := func(p *string) any {
		if p == nil {
			return nil
		}
		return *p
	}
	num := func(p *int) any {
		if p == nil {
			return nil
		}
		return *p
	}

	if changed(func(t *Target) any { return t.Name }) {
		if err := checkText("name", after.Name, MaxNameRunes, false); err != nil {
			return err
		}
		if after.Name != strings.TrimSpace(after.Name) {
			return fmt.Errorf("tree: name must not start or end with whitespace")
		}
	}
	if after.Host != nil && changed(func(t *Target) any { return str(t.Host) }) {
		if err := checkHost(*after.Host); err != nil {
			return err
		}
	}
	if after.Title != nil && changed(func(t *Target) any { return str(t.Title) }) {
		if err := checkText("title", *after.Title, MaxTitleRunes, false); err != nil {
			return err
		}
	}
	if after.Notes != nil && changed(func(t *Target) any { return str(t.Notes) }) {
		if err := checkText("notes", *after.Notes, MaxNotesRunes, true); err != nil {
			return err
		}
	}

	s := &after.Settings
	bound := func(name string, get func(*Target) *int, lo, hi int) error {
		v := get(after)
		if v == nil || !changed(func(t *Target) any { return num(get(t)) }) {
			return nil
		}
		if *v < lo || *v > hi {
			return fmt.Errorf("tree: %s must be between %d and %d, got %d", name, lo, hi, *v)
		}
		return nil
	}
	for _, c := range []struct {
		name   string
		get    func(*Target) *int
		lo, hi int
	}{
		{"interval_s", func(t *Target) *int { return t.Settings.IntervalS }, 1, MaxIntervalS},
		{"pings_per_interval", func(t *Target) *int { return t.Settings.PingsPerInterval }, 1, MaxPings},
		{"burst_gap_ms", func(t *Target) *int { return t.Settings.BurstGapMS }, 0, MaxBurstGapMS},
		{"packet_size", func(t *Target) *int { return t.Settings.PacketSize }, 12, MaxPacketSize},
	} {
		if err := bound(c.name, c.get, c.lo, c.hi); err != nil {
			return err
		}
	}
	// 0 turns these off, so the floor applies to anything else.
	zeroOr := func(name string, get func(*Target) *int, lo, hi int) error {
		v := get(after)
		if v == nil || *v == 0 || !changed(func(t *Target) any { return num(get(t)) }) {
			return nil
		}
		if *v < lo || *v > hi {
			return fmt.Errorf("tree: %s must be 0 or between %d and %d, got %d", name, lo, hi, *v)
		}
		return nil
	}
	if err := zeroOr("trace_interval_s", func(t *Target) *int { return t.Settings.TraceIntervalS }, MinTraceS, MaxTraceS); err != nil {
		return err
	}
	if err := zeroOr("retention_s", func(t *Target) *int { return t.Settings.RetentionS }, MinRetentionS, MaxRetentionS); err != nil {
		return err
	}

	if s.DNSQuery != nil && changed(func(t *Target) any { return str(t.Settings.DNSQuery) }) {
		if err := checkDNSName("dns_query", *s.DNSQuery, MaxDNSQueryLen); err != nil {
			return err
		}
	}
	if s.HTTPPath != nil && changed(func(t *Target) any { return str(t.Settings.HTTPPath) }) {
		if err := checkHTTPPath(*s.HTTPPath); err != nil {
			return err
		}
	}
	if s.Agents != nil && changed(func(t *Target) any { return str(t.Settings.Agents) }) {
		if err := checkText("agents", *s.Agents, MaxAgentsLen, false); err != nil {
			return err
		}
	}
	return nil
}

// checkText refuses text that is too long, is not valid UTF-8, or carries
// characters that are not text: control characters (a newline in a name forges
// a log line), the format characters that reorder or hide what is displayed
// (right-to-left overrides, zero-width joiners), and line separators. Newlines
// and tabs are allowed where the field is prose.
func checkText(field, s string, maxRunes int, prose bool) error {
	if !utf8.ValidString(s) {
		return fmt.Errorf("tree: %s is not valid UTF-8", field)
	}
	if n := utf8.RuneCountInString(s); n > maxRunes {
		return fmt.Errorf("tree: %s is %d characters, more than the %d allowed", field, n, maxRunes)
	}
	for _, r := range s {
		switch {
		case prose && (r == '\n' || r == '\t' || r == '\r'):
		case unicode.IsControl(r):
			return fmt.Errorf("tree: %s contains a control character (U+%04X)", field, r)
		case unicode.Is(unicode.Cf, r), r == ' ', r == ' ':
			return fmt.Errorf("tree: %s contains an invisible formatting character (U+%04X)", field, r)
		}
	}
	return nil
}

// checkHost accepts an IP address or a DNS name, and nothing that could be read
// as anything else. A host is joined into URLs and handed to resolvers, so a
// value with a slash, an at-sign or a hash is not just odd: the HTTP probe built
// its URL from it, and "intranet.corp#.example.net" became a request for
// intranet.corp while the dial went to the address resolved for the whole
// string.
func checkHost(h string) error {
	if h == "" {
		return fmt.Errorf("tree: host must not be empty")
	}
	if len(h) > MaxHostLen {
		return fmt.Errorf("tree: host is longer than %d bytes", MaxHostLen)
	}
	if _, err := netip.ParseAddr(h); err == nil {
		return nil
	}
	return checkDNSName("host", h, MaxHostLen)
}

// checkDNSName accepts letters, digits, hyphens and underscores in dot-separated
// labels of at most 63 bytes, with an optional trailing dot. Underscores because
// real names have them (_dmarc, _sip._tcp).
func checkDNSName(field, name string, maxLen int) error {
	if len(name) > maxLen {
		return fmt.Errorf("tree: %s is longer than %d bytes", field, maxLen)
	}
	if !utf8.ValidString(name) {
		return fmt.Errorf("tree: %s is not valid UTF-8", field)
	}
	trimmed := strings.TrimSuffix(name, ".")
	if trimmed == "" && name != "." {
		return fmt.Errorf("tree: %s must not be empty", field)
	}
	if name == "." { // the root, a valid query
		return nil
	}
	for _, label := range strings.Split(trimmed, ".") {
		if label == "" {
			return fmt.Errorf("tree: %s %q has an empty label", field, name)
		}
		if len(label) > 63 {
			return fmt.Errorf("tree: %s %q has a label longer than 63 bytes", field, name)
		}
		for _, r := range label {
			if !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_') {
				return fmt.Errorf("tree: %s %q contains %q, which is not part of a DNS name", field, name, r)
			}
		}
	}
	return nil
}

func checkHTTPPath(p string) error {
	if len(p) > MaxHTTPPathLen {
		return fmt.Errorf("tree: http_path is longer than %d bytes", MaxHTTPPathLen)
	}
	if !utf8.ValidString(p) {
		return fmt.Errorf("tree: http_path is not valid UTF-8")
	}
	for _, r := range p {
		if unicode.IsControl(r) || r == ' ' || unicode.Is(unicode.Cf, r) {
			return fmt.Errorf("tree: http_path contains a space or control character (U+%04X)", r)
		}
	}
	return nil
}

// CheckTreeLimits applies the limits that depend on the whole tree and on
// inherited values: depth, the number of targets, and how much load one target
// and all of them together put on the network. before is the tree as stored and
// after the one being proposed; only what the change makes worse is refused, so
// an installation already past a limit can still be edited, and can still
// reduce what it runs.
func CheckTreeLimits(before, after []Target) error {
	afterT, err := New(after)
	if err != nil {
		return err
	}
	beforeT, _ := New(before) // nil when the stored tree is itself invalid; then everything is checked

	if len(after) > MaxTargets && len(after) > len(before) {
		return fmt.Errorf("tree: %d targets is more than the %d smokeng will hold", len(after), MaxTargets)
	}

	prior := map[int64]*Target{}
	for i := range before {
		prior[before[i].ID] = &before[i]
	}
	for i := range after {
		n := &after[i]
		old, existed := prior[n.ID]
		moved := !existed || (old.ParentID == nil) != (n.ParentID == nil) ||
			(old.ParentID != nil && n.ParentID != nil && *old.ParentID != *n.ParentID)
		if moved {
			if d := depth(afterT, n); d > MaxDepth {
				return fmt.Errorf("tree: %q would sit %d levels deep, more than the %d allowed", n.Name, d, MaxDepth)
			}
		}
	}

	var rateBefore, rateAfter float64
	for i := range after {
		n := &after[i]
		if n.Host == nil {
			continue
		}
		res, err := afterT.Resolve(n.ID)
		if err != nil {
			return err
		}
		if n.Enabled {
			rateAfter += probeRate(res)
		}
		if beforeT != nil {
			if old, existed := prior[n.ID]; existed {
				if rb, err := beforeT.Resolve(old.ID); err == nil && notWorse(rb, res) {
					continue
				}
			}
		}
		if err := resolvedLimits(res); err != nil {
			return fmt.Errorf("target %d (%s): %w", n.ID, n.Name, err)
		}
	}
	if beforeT != nil {
		for i := range before {
			n := &before[i]
			if n.Host == nil || !n.Enabled {
				continue
			}
			if rb, err := beforeT.Resolve(n.ID); err == nil {
				rateBefore += probeRate(rb)
			}
		}
	}
	if rateAfter > MaxTotalProbeRate && rateAfter > rateBefore {
		return fmt.Errorf("tree: all targets together would send %.0f probes a second, more than the %d allowed",
			rateAfter, MaxTotalProbeRate)
	}
	return nil
}

func probeRate(r Resolved) float64 {
	return float64(r.PingsPerInterval.Effective) / float64(r.IntervalS.Effective)
}

// notWorse reports whether a target's load after a change is no greater than
// before it: no more pings a second, no more bytes a second, and no larger a
// share of its interval spent waiting on a timeout. A target over a limit can
// then be brought down in steps, or left alone while something else about it
// changes, without being refused for what it already was.
func notWorse(before, after Resolved) bool {
	waiting := func(r Resolved) float64 { return float64(r.TimeoutMS.Effective) / float64(r.IntervalS.Effective*1000) }
	bytes := func(r Resolved) float64 { return probeRate(r) * float64(r.PacketSize.Effective) }
	return probeRate(after) <= probeRate(before) && bytes(after) <= bytes(before) && waiting(after) <= waiting(before)
}

// resolvedLimits checks the load one target puts on what it probes, from its
// effective values after inheritance.
func resolvedLimits(r Resolved) error {
	interval, pings := r.IntervalS.Effective, r.PingsPerInterval.Effective
	// A probe that waits longer than the interval is still waiting when the next
	// interval's probes go out, so the sockets held open grow with pings times
	// timeout over interval instead of being bounded by pings.
	if r.TimeoutMS.Effective > interval*1000 {
		return fmt.Errorf("tree: timeout_ms %d is longer than the %ds interval", r.TimeoutMS.Effective, interval)
	}
	rate := probeRate(r)
	if rate > MaxProbeRate {
		return fmt.Errorf("tree: %d pings in %ds is %.0f a second, more than the %d allowed for one target",
			pings, interval, rate, MaxProbeRate)
	}
	if bps := rate * float64(r.PacketSize.Effective); bps > MaxProbeBytesPerSec {
		return fmt.Errorf("tree: %d pings of %d bytes in %ds is %.0f bytes a second, more than the %d allowed for one target",
			pings, r.PacketSize.Effective, interval, bps, MaxProbeBytesPerSec)
	}
	return nil
}

func depth(t *Tree, n *Target) int {
	d := 0
	for cur := n; cur.ParentID != nil; d++ {
		next, ok := t.nodes[*cur.ParentID]
		if !ok {
			break
		}
		cur = next
	}
	return d
}
