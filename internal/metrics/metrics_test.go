package metrics

import (
	"strings"
	"testing"
)

func render(t *testing.T, ms []Metric) string {
	t.Helper()
	var b strings.Builder
	if err := Write(&b, ms); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestFormat(t *testing.T) {
	got := render(t, []Metric{
		Simple("smokeng_late_replies_total", "Replies that arrived too late.", Counter, 42),
	})
	want := "# HELP smokeng_late_replies_total Replies that arrived too late.\n" +
		"# TYPE smokeng_late_replies_total counter\n" +
		"smokeng_late_replies_total 42\n"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// Agent names come from operators, so a quote or backslash in one is not
// theoretical — and an unescaped one produces a scrape the collector drops
// without saying why.
func TestLabelsAreEscaped(t *testing.T) {
	got := render(t, []Metric{{
		Name: "smokeng_agent_enabled", Help: "Whether an agent is enabled.", Kind: Gauge,
		Samples: []Sample{{Labels: map[string]string{"agent": `we"ird\one`}, Value: 1}},
	}})
	if !strings.Contains(got, `agent="we\"ird\\one"`) {
		t.Errorf("label not escaped:\n%s", got)
	}
}

// Two scrapes of the same state must be byte-identical, or diffing them is
// noise rather than signal.
func TestOutputIsStable(t *testing.T) {
	m := []Metric{{
		Name: "smokeng_agent_last_seen_seconds", Help: "Last contact.", Kind: Gauge,
		Samples: []Sample{
			{Labels: map[string]string{"agent": "rtm"}, Value: 2},
			{Labels: map[string]string{"agent": "ams"}, Value: 1},
			{Labels: map[string]string{"agent": "gro"}, Value: 3},
		},
	}}
	first := render(t, m)
	for range 5 {
		if got := render(t, m); got != first {
			t.Fatalf("output is not stable:\n%s\n---\n%s", first, got)
		}
	}
	// Sorted, so ams comes before gro before rtm.
	if i, j := strings.Index(first, `"ams"`), strings.Index(first, `"rtm"`); i > j {
		t.Errorf("samples are not sorted:\n%s", first)
	}
}

func TestValueFormatting(t *testing.T) {
	got := render(t, []Metric{
		Simple("a_total", "h", Counter, 7),
		Simple("b_seconds", "h", Gauge, 0.25),
		Simple("c_seconds", "h", Gauge, 1_756_400_000),
	})
	for _, want := range []string{"a_total 7\n", "b_seconds 0.25\n", "c_seconds 1756400000\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

// An empty family would emit HELP and TYPE with no samples, which is legal
// but noise; skipping it keeps a scrape readable.
func TestEmptyFamiliesAreSkipped(t *testing.T) {
	got := render(t, []Metric{{Name: "empty", Help: "h", Kind: Gauge}})
	if got != "" {
		t.Errorf("expected nothing, got:\n%s", got)
	}
}
