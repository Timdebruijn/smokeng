package config

import (
	"context"
	"strings"
	"testing"
)

const smokepingTargets = `
*** Targets ***

probe = FPing
menu = Top
title = Network Latency Grapher
pings = 15

+ Datacenter
menu = DC
title = Datacenter hosts
remark = Racks 1 through 4, \
         second floor

++ router1
menu = router1
title = Core Router 1
host = 10.0.0.1
pings = 20

++ router2
host = 10.0.0.2
alerts = someloss
hide = yes

++ v6gw
probe = FPing6
host = 2001:db8::1

+ Internet

++ google
host = google.com

++ dynamichost
host = DYNAMIC

++ overlay
host = /Datacenter/router1 /Datacenter/router2

++ remote
host = 10.1.0.1
slaves = ams rtm
`

func TestParseSmokePing(t *testing.T) {
	f, warnings, err := ParseSmokePing([]byte(smokepingTargets), false)
	if err != nil {
		t.Fatal(err)
	}

	// Hierarchy became paths; groups exist without hosts.
	for _, want := range []string{"Datacenter", "Datacenter/router1", "Datacenter/router2",
		"Datacenter/v6gw", "Internet", "Internet/google", "Internet/remote"} {
		if _, ok := f.Targets[want]; !ok {
			t.Errorf("missing target %q (got %v)", want, keys(f.Targets))
		}
	}
	if f.Defaults.PingsPerInterval == nil || *f.Defaults.PingsPerInterval != 15 {
		t.Errorf("top-level pings did not become a default: %v", f.Defaults.PingsPerInterval)
	}

	dc := f.Targets["Datacenter"]
	if dc.Host != nil {
		t.Errorf("Datacenter should be a group, got host %v", *dc.Host)
	}
	if dc.Title == nil || *dc.Title != "Datacenter hosts" {
		t.Errorf("title = %v", dc.Title)
	}
	// The continuation line is folded into the remark. It is marked as one with
	// a trailing backslash, which is what SmokePing's own parser keys on; the
	// fixture used to rely on indentation alone, which SmokePing does not.
	if dc.Notes == nil || !strings.Contains(*dc.Notes, "second floor") {
		t.Errorf("remark continuation lost: %v", dc.Notes)
	}

	r1 := f.Targets["Datacenter/router1"]
	if r1.AddressFamily == nil || *r1.AddressFamily != "v4" {
		t.Errorf("router1 family = %v", r1.AddressFamily)
	}
	if r1.PingsPerInterval == nil || *r1.PingsPerInterval != 20 {
		t.Errorf("router1 pings = %v", r1.PingsPerInterval)
	}
	if !f.Targets["Datacenter/router2"].Hidden {
		t.Error("hide = yes did not map to hidden")
	}
	// A literal v6 address and the FPing6 probe both imply v6.
	if fam := f.Targets["Datacenter/v6gw"].AddressFamily; fam == nil || *fam != "v6" {
		t.Errorf("v6gw family = %v", fam)
	}
	// A hostname defaults to v4 rather than "whatever resolves".
	if fam := f.Targets["Internet/google"].AddressFamily; fam == nil || *fam != "v4" {
		t.Errorf("google family = %v", fam)
	}
	if ag := agentStringFrom(f.Targets["Internet/remote"].Agents); ag == nil || *ag != "local ams rtm" {
		t.Errorf("remote agents = %v", ag)
	}

	// Everything skipped is reported.
	if _, ok := f.Targets["Internet/dynamichost"]; ok {
		t.Error("DYNAMIC host should not be imported")
	}
	if _, ok := f.Targets["Internet/overlay"]; ok {
		t.Error("multi-host overlay should not be imported")
	}
	joined := strings.Join(warnings, "\n")
	for _, want := range []string{"DYNAMIC", "overlay", "alerts"} {
		if !strings.Contains(joined, want) {
			t.Errorf("no warning mentioning %q in:\n%s", want, joined)
		}
	}
}

func TestParseSmokePingAlsoIPv6(t *testing.T) {
	f, _, err := ParseSmokePing([]byte(smokepingTargets), true)
	if err != nil {
		t.Fatal(err)
	}
	v6, ok := f.Targets["Internet/google-v6"]
	if !ok {
		t.Fatalf("no v6 sibling created (got %v)", keys(f.Targets))
	}
	if *v6.AddressFamily != "v6" || *v6.Host != "google.com" {
		t.Errorf("v6 sibling = %+v", v6)
	}
	// A literal v4 address must not be duplicated: it cannot resolve to v6.
	if _, ok := f.Targets["Datacenter/router1-v6"]; ok {
		t.Error("literal v4 address should not get a v6 sibling")
	}
}

// The importer must produce a configuration that the normal sync accepts, so
// an imported tree obeys exactly the same rules as a hand-written one.
func TestSmokePingImportApplies(t *testing.T) {
	ctx := context.Background()
	st := open(t)
	f, _, err := ParseSmokePing([]byte(smokepingTargets), false)
	if err != nil {
		t.Fatal(err)
	}
	sum, err := Apply(ctx, st, f, false, AllowUnknownAgents())
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if sum.Created == 0 {
		t.Fatalf("nothing created: %+v", sum)
	}
	tr, byPath := mustTree(t, st)

	leaf := byPath["/Datacenter/router1"]
	res, err := tr.Resolve(leaf.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.PingsPerInterval.Effective != 20 || res.PingsPerInterval.Source.Path != "/Datacenter/router1" {
		t.Errorf("router1 pings = %+v", res.PingsPerInterval)
	}
	// router2 has no local pings, so it inherits the file-level default of 15.
	res2, err := tr.Resolve(byPath["/Datacenter/router2"].ID)
	if err != nil {
		t.Fatal(err)
	}
	if res2.PingsPerInterval.Effective != 15 || res2.PingsPerInterval.Source.Path != "/" {
		t.Errorf("router2 pings = %+v", res2.PingsPerInterval)
	}
}

func TestParseSmokePingErrors(t *testing.T) {
	if _, _, err := ParseSmokePing([]byte("*** Targets ***\n"), false); err == nil {
		t.Error("expected an error for a file with no targets")
	}
	if _, _, err := ParseSmokePing([]byte("*** Targets ***\n+++ deep\nhost = 1.1.1.1\n"), false); err == nil {
		t.Error("expected an error when a level is skipped")
	}
}

// The probe modules a real SmokePing install actually names, mapped to the
// smokeng type that measures the same thing. IRTT is the one that matters most:
// smokeng probes with the same tool, so importing it as icmp would turn a UDP
// round-trip measurement into an ICMP ping wearing the same graph — found by
// running the importer against a production config that used it.
func TestSmokePingProbeModuleMapping(t *testing.T) {
	cases := map[string]string{
		"FPing":           "icmp",
		"FPing6":          "icmp",
		"FPingContinuous": "icmp", // a renamed fping probe still pings
		"IRTT":            "irtt",
		"DNS":             "dns",
		"EchoPingDNS":     "dns",
		"AnotherDNS":      "dns",
		"EchoPingHttp":    "http",
		"Curl":            "http",
		"EchoPingHttps":   "https",
		"TCPPing":         "tcp",
		"EchoPingTcp":     "tcp",
		"":                "icmp", // no probe named: SmokePing's own default
		"SomeUnknown":     "",     // unrecognised: warned about, never guessed
	}
	for module, want := range cases {
		if got := probeTypeFor(module); got != want {
			t.Errorf("probeTypeFor(%q) = %q, want %q", module, got, want)
		}
	}
}

// An IRTT target comes across as irtt, with the difference in what is graphed
// stated rather than left for the operator to discover.
func TestSmokePingImportsIRTT(t *testing.T) {
	const cfg = `*** Probes ***

+ FPing
binary = /usr/bin/fping

+ IRTT
binary = /usr/bin/irtt

*** Targets ***

probe = FPing

+ wag

++ Irtt
probe = IRTT
host = irtt.example.org
`
	f, warnings, err := ParseSmokePing([]byte(cfg), false)
	if err != nil {
		t.Fatal(err)
	}
	e, ok := f.Targets["wag/Irtt"]
	if !ok {
		t.Fatalf("target not imported; got %v", keys(f.Targets))
	}
	if e.ProbeType == nil || *e.ProbeType != "irtt" {
		t.Fatalf("probe_type = %v, want irtt", e.ProbeType)
	}
	var said bool
	for _, w := range warnings {
		if strings.Contains(w, "irtt") && strings.Contains(w, "round-trip") {
			said = true
		}
		if strings.Contains(w, "no smokeng equivalent") {
			t.Errorf("IRTT reported as unmappable: %s", w)
		}
	}
	if !said {
		t.Errorf("no warning that the graph differs; got %v", warnings)
	}
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// A secret in the SmokePing config is not imported — it would travel into the
// API, the export and version control — but its absence must be stated. Without
// the key the irtt server refuses the session, and the target reads as a send
// failure: a silent outage that looks like a network fault rather than a missing
// setting. Found on a real migration, where three targets went quiet for exactly
// this reason.
func TestSmokePingWarnsAboutIRTTSecret(t *testing.T) {
	const cfg = `*** Probes ***

+ IRTT
binary = /usr/bin/irtt

*** Targets ***

probe = IRTT

+ wag
host = irtt.example.org
hmac = deadbeefcafe
`
	f, warnings, err := ParseSmokePing([]byte(cfg), false)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range f.Targets {
		// Whatever else it carries, it must not carry the key.
		if e.Notes != nil && strings.Contains(*e.Notes, "deadbeefcafe") {
			t.Error("the HMAC key was imported into the target tree")
		}
	}
	var warned bool
	for _, w := range warnings {
		if strings.Contains(w, "HMAC") {
			warned = true
			if strings.Contains(w, "deadbeefcafe") {
				t.Errorf("the warning leaks the key: %s", w)
			}
		}
	}
	if !warned {
		t.Errorf("no warning that a key is needed; got %v", warnings)
	}
}

// SmokePing's IRTT probe graphs one figure per target, chosen with `metric`,
// all from the same session. Importing each as its own target opened a separate
// irtt session to the same server; they collided, and only one won per interval
// while the rest recorded as send failures. Found on a real migration.
func TestSmokePingSkipsDerivedIRTTMetrics(t *testing.T) {
	const cfg = `*** Probes ***

+ IRTT
binary = /usr/bin/irtt

*** Targets ***

probe = IRTT

+ wag

++ Rtt
host = irtt.example.org

++ SendJitter
host = irtt.example.org
metric = send_ipdv

++ RecvJitter
host = irtt.example.org
metric = receive_ipdv
`
	f, warnings, err := ParseSmokePing([]byte(cfg), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := f.Targets["wag/Rtt"]; !ok {
		t.Errorf("the plain irtt target should be imported; got %v", keys(f.Targets))
	}
	for _, skipped := range []string{"wag/SendJitter", "wag/RecvJitter"} {
		if _, ok := f.Targets[skipped]; ok {
			t.Errorf("%s is a derived view of the same session and must not become its own target", skipped)
		}
	}
	joined := strings.Join(warnings, "\n")
	for _, want := range []string{"send_ipdv", "receive_ipdv"} {
		if !strings.Contains(joined, want) {
			t.Errorf("no warning naming %q in:\n%s", want, joined)
		}
	}
}

// SmokePing renders title, menu and remark as HTML and its configs use that:
// entities for punctuation, <b> for emphasis, <br> for a break. smokeng renders
// them as text, so an import that carried the markup through showed "&mdash;"
// where a dash belonged and "<b>" around a word — sixteen and ten of them
// respectively on a real migration, corrected by hand one title at a time.
//
// The trailing backslash is SmokePing's line-continuation marker, not text, and
// leaving it in put a stray "\" in the middle of every wrapped sentence.
func TestSmokePingImportsDisplayTextAsText(t *testing.T) {
	const cfg = `*** Targets ***

probe = FPing

+ wag
host = 10.0.0.1
title = Telfort(KPN) &mdash; IRTT round-trip
remark = Meet met UDP. <b>Burst</b> is de klassieke meting, \
         en <b>Continu</b> pingt door.<br><br>Wijken die af, \
         dan zit het in korte pieken.
`
	f, _, err := ParseSmokePing([]byte(cfg), false)
	if err != nil {
		t.Fatal(err)
	}
	e, ok := f.Targets["wag"]
	if !ok {
		t.Fatalf("target missing; got %v", keys(f.Targets))
	}
	if e.Title == nil || *e.Title != "Telfort(KPN) — IRTT round-trip" {
		t.Errorf("title = %q, want the entity decoded to a dash", derefOr(e.Title))
	}
	if e.Notes == nil {
		t.Fatal("no notes imported")
	}
	notes := *e.Notes
	for _, unwanted := range []string{"<b>", "</b>", "<br>", "&mdash;", "\\"} {
		if strings.Contains(notes, unwanted) {
			t.Errorf("notes still carry %q: %s", unwanted, notes)
		}
	}
	// The words survive, and so does the sentence break, as a space.
	for _, want := range []string{"Burst", "Continu", "korte pieken"} {
		if !strings.Contains(notes, want) {
			t.Errorf("notes lost %q: %s", want, notes)
		}
	}
	if strings.Contains(notes, "  ") {
		t.Errorf("notes carry a doubled space left by the stripping: %q", notes)
	}
}

// Markup a config deliberately escaped is text, and must survive as text.
func TestSmokePingKeepsEscapedMarkup(t *testing.T) {
	const cfg = `*** Targets ***

probe = FPing

+ a
host = 10.0.0.1
title = literally &lt;b&gt; and &amp; itself
`
	f, _, err := ParseSmokePing([]byte(cfg), false)
	if err != nil {
		t.Fatal(err)
	}
	if got := derefOr(f.Targets["a"].Title); got != "literally <b> and & itself" {
		t.Errorf("title = %q", got)
	}
}

func derefOr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// The cases the first version of plainText got wrong, each of which produced
// something worse than the markup it was removing.
func TestPlainTextEdges(t *testing.T) {
	cases := []struct{ name, in, want string }{
		// A tag must start with a letter, so ordinary prose survives. This is
		// the case that matters most: a remark is written by a person.
		{"less-than in prose", "latency < 5ms is fine", "latency < 5ms is fine"},
		{"less-than before a digit", "threshold <5ms", "threshold <5ms"},
		// A break carrying an attribute used to fall through to the generic
		// stripper, which removes without leaving a space — so two sentences
		// were glued into one word.
		{"break with an attribute", "One.<br class=\"spacer\">Two.", "One. Two."},
		{"break, plain and closed", "a<br>b<br/>c<BR>d", "a b c d"},
		// A ">" inside a quoted attribute ended the match early and left the
		// remainder of the attribute in the text: wreckage, not markup.
		{"quoted > in an attribute", `<a href="http://x?y=1>2">Link</a> tail`, "Link tail"},
		{"hyphenated tag", "<my-tag>hello</my-tag>", "hello"},
		// A non-breaking space is a space once this is text; left alone it
		// survives the collapse and reads as one wide gap.
		{"nbsp used for spacing", "word1&nbsp;&nbsp;&nbsp;word2", "word1 word2"},
		// A single one is the case that needs the normalisation: a run is
		// already caught by the collapse, so testing only runs left the
		// conversion unguarded.
		{"a single nbsp", "word1&nbsp;word2", "word1 word2"},
		{"entity and tag together", "Telfort &mdash; <b>burst</b>", "Telfort — burst"},
		{"escaped markup is text", "literally &lt;b&gt; and &amp; itself", "literally <b> and & itself"},
		{"nothing to do", "plain text", "plain text"},
	}
	for _, c := range cases {
		if got := plainText(c.in); got != c.want {
			t.Errorf("%s:\n  in   %q\n  got  %q\n  want %q", c.name, c.in, got, c.want)
		}
	}
}

// A value continues onto the next line when it ends with a backslash, and only
// then. That is SmokePing's own rule — Config::Grammar reads
// `while (/\\$/) { s/\\$//; ... $_ .= ' ' . $n }` — and it says nothing about
// indentation.
//
// Keying on indentation instead was wrong in both directions, and the second
// one is the dangerous half: an indented line that never asked to be joined was
// silently glued onto the value above it.
func TestSmokePingContinuationFollowsTheBackslash(t *testing.T) {
	const cfg = `*** Targets ***

probe = FPing

+ a
host = 10.0.0.1
title = joined \
        across two lines
remark = not continued
         this line is indented but was never asked for

+ b
host = 10.0.0.2
title = continued \
even without indentation
`
	f, warnings, err := ParseSmokePing([]byte(cfg), false)
	if err != nil {
		t.Fatal(err)
	}
	if got := derefOr(f.Targets["a"].Title); got != "joined across two lines" {
		t.Errorf("a.title = %q, want the two lines folded", got)
	}
	// The indented line is not part of the remark above it.
	if got := derefOr(f.Targets["a"].Notes); got != "not continued" {
		t.Errorf("a.notes = %q; an indented line was glued on without being asked for", got)
	}
	// And it is reported rather than dropped in silence.
	joined := strings.Join(warnings, "\n")
	if !strings.Contains(joined, "this line is indented") {
		t.Errorf("nothing warned about the stray line:\n%s", joined)
	}
	// A continued line joins whether or not it is indented, because the
	// backslash is what decides.
	if got := derefOr(f.Targets["b"].Title); got != "continued even without indentation" {
		t.Errorf("b.title = %q", got)
	}
}
