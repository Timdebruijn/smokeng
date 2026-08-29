// Package metrics writes Prometheus text exposition for smokeng's own
// operational health (DESIGN.md §7.1).
//
// What it deliberately does not carry is measurement data. Latency and loss
// live in the store at full resolution and are read as Arrow; pushing them
// through Prometheus would flatten every interval to a single number, which
// is the exact loss this project exists to avoid. These metrics answer "is
// smokeng healthy", never "what is the network doing".
//
// The exposition format is written by hand rather than pulled in with a
// client library: counters and gauges are all that is needed here, and the
// format is short enough to implement correctly and test.
package metrics

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// Kind is a metric's Prometheus type.
type Kind string

const (
	Counter Kind = "counter"
	Gauge   Kind = "gauge"
)

// Metric is one named family with one or more samples.
type Metric struct {
	Name    string
	Help    string
	Kind    Kind
	Samples []Sample
}

// Sample is one value, optionally with labels.
type Sample struct {
	Labels map[string]string
	Value  float64
}

// Simple builds a metric with a single unlabelled sample.
func Simple(name, help string, kind Kind, value float64) Metric {
	return Metric{Name: name, Help: help, Kind: kind, Samples: []Sample{{Value: value}}}
}

// Write renders the metrics in Prometheus text exposition format. Families
// are emitted in the order given; samples within a family are sorted by their
// rendered labels so repeated scrapes are byte-stable, which makes diffing
// two scrapes meaningful.
func Write(w io.Writer, ms []Metric) error {
	var b strings.Builder
	for _, m := range ms {
		if len(m.Samples) == 0 {
			continue
		}
		fmt.Fprintf(&b, "# HELP %s %s\n", m.Name, escapeHelp(m.Help))
		fmt.Fprintf(&b, "# TYPE %s %s\n", m.Name, m.Kind)

		lines := make([]string, 0, len(m.Samples))
		for _, s := range m.Samples {
			lines = append(lines, m.Name+renderLabels(s.Labels)+" "+formatValue(s.Value))
		}
		sort.Strings(lines)
		for _, line := range lines {
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func renderLabels(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+`="`+escapeLabel(labels[k])+`"`)
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// escapeLabel escapes a label value. Agent names come from operators, so this
// is not theoretical: an unescaped quote would produce a scrape the collector
// silently drops.
func escapeLabel(v string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return r.Replace(v)
}

// escapeHelp escapes a HELP string, where only backslash and newline are
// special.
func escapeHelp(v string) string {
	r := strings.NewReplacer(`\`, `\\`, "\n", `\n`)
	return r.Replace(v)
}

func formatValue(v float64) string {
	// Integral values render without a decimal point, which is what makes a
	// counter readable at a glance.
	if v == float64(int64(v)) {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}
