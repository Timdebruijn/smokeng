package config

import (
	"bufio"
	"bytes"
	"fmt"
	"html"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// srcLine is one input line with where it came from, so a warning can name the
// file and line even after @include has spliced several files together.
type srcLine struct {
	file string
	no   int
	text string
}

// maxIncludeDepth bounds @include recursion. A real config nests a couple of
// levels; anything deeper is a loop the cycle guard did not catch or a mistake.
const maxIncludeDepth = 20

// ParseSmokePing converts a SmokePing Targets file into smokeng's
// configuration form. The `+`/`++`/`+++` hierarchy becomes the target tree and
// per-node keys become local settings, so inheritance survives the move rather
// than being flattened. The `*** Probes ***` section is read so each target's
// probe module maps to a smokeng probe type — icmp, dns, tcp, http or https —
// rather than everything arriving as icmp.
//
// Anything smokeng deliberately does not implement — alert definitions,
// alternative hierarchies, multi-host overlay graphs, DYNAMIC hosts — is
// reported as a warning rather than dropped in silence. An adoption path that
// quietly loses configuration is worse than one that refuses it out loud.
//
// This byte form cannot follow @include (it has no file to resolve paths
// against); ParseSmokePingFile can, and is what the CLI uses.
func ParseSmokePing(data []byte, alsoIPv6 bool) (File, []string, error) {
	var lines []srcLine
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	no := 0
	for sc.Scan() {
		no++
		lines = append(lines, srcLine{file: "(input)", no: no, text: sc.Text()})
	}
	if err := sc.Err(); err != nil {
		return File{}, nil, fmt.Errorf("smokeping: read: %w", err)
	}
	return parseSmokePing(lines, alsoIPv6, false)
}

// ParseSmokePingFile reads a SmokePing config from disk and follows @include
// directives, resolving each relative to the including file's directory. This
// is what turns a multi-file SmokePing install — almost every real one — into a
// single import rather than one per file.
func ParseSmokePingFile(path string, alsoIPv6 bool) (File, []string, error) {
	lines, warnings, err := readSmokePingLines(path, map[string]bool{}, 0)
	if err != nil {
		return File{}, warnings, err
	}
	f, w2, err := parseSmokePing(lines, alsoIPv6, true)
	return f, append(warnings, w2...), err
}

// readSmokePingLines reads one file into source lines, expanding @include in
// place. seen guards against an include cycle; depth is a backstop.
func readSmokePingLines(path string, seen map[string]bool, depth int) ([]srcLine, []string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, nil, err
	}
	if seen[abs] {
		// A cycle: the file includes itself directly or through others. Stop
		// rather than loop, and say which file closed the loop.
		return nil, []string{fmt.Sprintf("@include cycle at %s; not expanded again", path)}, nil
	}
	if depth > maxIncludeDepth {
		return nil, nil, fmt.Errorf("smokeping: @include nested deeper than %d at %s", maxIncludeDepth, path)
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, nil, err
	}
	seen[abs] = true
	defer delete(seen, abs) // a file may be included by two siblings; only a true cycle is on the current stack

	var out []srcLine
	var warnings []string
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	no := 0
	for sc.Scan() {
		no++
		text := sc.Text()
		trimmed := strings.TrimSpace(text)
		if strings.HasPrefix(trimmed, "@include") {
			inc := strings.TrimSpace(strings.TrimPrefix(trimmed, "@include"))
			inc = strings.Trim(inc, `"' `)
			if inc == "" {
				warnings = append(warnings, fmt.Sprintf("%s line %d: @include with no path; skipped", path, no))
				continue
			}
			if !filepath.IsAbs(inc) {
				inc = filepath.Join(filepath.Dir(abs), inc)
			}
			sub, subWarn, err := readSmokePingLines(inc, seen, depth+1)
			warnings = append(warnings, subWarn...)
			if err != nil {
				return nil, warnings, fmt.Errorf("smokeping: @include %q (from %s line %d): %w", inc, path, no, err)
			}
			out = append(out, sub...)
			continue
		}
		out = append(out, srcLine{file: path, no: no, text: text})
	}
	if err := sc.Err(); err != nil {
		return nil, warnings, fmt.Errorf("smokeping: read %s: %w", path, err)
	}
	return out, warnings, nil
}

func parseSmokePing(lines []srcLine, alsoIPv6, includesFollowed bool) (File, []string, error) {
	f := File{Targets: map[string]Entry{}}
	var warnings []string
	warn := func(sl srcLine, format string, args ...any) {
		where := fmt.Sprintf("%s line %d: ", sl.file, sl.no)
		warnings = append(warnings, where+fmt.Sprintf(format, args...))
	}
	warnAt := func(path, format string, args ...any) {
		warnings = append(warnings, path+": "+fmt.Sprintf(format, args...))
	}

	type node struct {
		path string
		keys map[string]string
	}
	var stack []node           // one entry per depth level
	top := map[string]string{} // keys before the first '+' line
	nodes := []node{}          // completed target nodes, in file order
	// probeModule maps every probe name defined in the Probes section — at any
	// depth — to the top-level SmokePing module it descends from, so a target's
	// `probe = MyWebProbe` resolves to EchoPingHttp and thus to https.
	probeModule := map[string]string{}

	const (
		secTargets = iota
		secProbes
		secOther
	)
	section := secTargets
	sawTargetsHeader := false

	var pendingKey string
	var pendingNode *node
	var probeStack []string // top-level module name per depth in the Probes section

	flush := func() {
		if len(stack) > 0 {
			nodes = append(nodes, stack[len(stack)-1])
		}
	}

	for _, sl := range lines {
		raw := sl.text
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}

		// Section headers: *** Targets ***, *** Probes ***, ...
		if strings.HasPrefix(trimmed, "***") && strings.HasSuffix(trimmed, "***") {
			flush()
			stack = nil
			name := strings.ToLower(strings.TrimSpace(strings.Trim(trimmed, "* ")))
			switch name {
			case "targets":
				section = secTargets
				sawTargetsHeader = true
			case "probes":
				section = secProbes
			default:
				section = secOther
			}
			pendingKey, pendingNode, probeStack = "", nil, nil
			continue
		}

		if section == secProbes {
			// Build the probe-name → module map. Depth-1 names are modules;
			// deeper names are subclasses that inherit their ancestor's module.
			if strings.HasPrefix(trimmed, "+") {
				depth := 0
				for depth < len(trimmed) && trimmed[depth] == '+' {
					depth++
				}
				pname := strings.TrimSpace(trimmed[depth:])
				if pname == "" {
					continue
				}
				if depth-1 < len(probeStack) {
					probeStack = probeStack[:depth-1]
				}
				module := pname
				if len(probeStack) > 0 {
					module = probeStack[0]
				}
				probeStack = append(probeStack, module)
				probeModule[pname] = module
			}
			continue
		}
		if section == secOther {
			continue
		}

		// --- Targets section ---

		if strings.HasPrefix(trimmed, "@include") {
			if includesFollowed {
				// readSmokePingLines already expanded it; this line cannot occur.
				continue
			}
			warn(sl, "@include is not followed by the byte parser; use "+
				"`smokeng config import-smokeping FILE`, which follows includes relative to the file")
			continue
		}

		// A line starting with whitespace continues the previous value. The
		// backslash that marks the continuation is SmokePing's syntax, not part
		// of the text: leaving it in put a stray "\" in the middle of every
		// wrapped sentence, which is what a real import produced.
		if pendingKey != "" && (strings.HasPrefix(raw, " ") || strings.HasPrefix(raw, "\t")) {
			if pendingNode != nil {
				pendingNode.keys[pendingKey] = joinContinuation(pendingNode.keys[pendingKey], trimmed)
			} else {
				top[pendingKey] = joinContinuation(top[pendingKey], trimmed)
			}
			continue
		}

		if strings.HasPrefix(trimmed, "+") {
			depth := 0
			for depth < len(trimmed) && trimmed[depth] == '+' {
				depth++
			}
			name := strings.TrimSpace(trimmed[depth:])
			if name == "" {
				return f, warnings, fmt.Errorf("smokeping: %s line %d: %q has no name", sl.file, sl.no, trimmed)
			}
			if depth > len(stack)+1 {
				return f, warnings, fmt.Errorf("smokeping: %s line %d: %q skips a level", sl.file, sl.no, trimmed)
			}
			flush()
			if depth-1 < len(stack) {
				stack = stack[:depth-1]
			}
			parent := ""
			if len(stack) > 0 {
				parent = stack[len(stack)-1].path + "/"
			}
			n := node{path: parent + name, keys: map[string]string{}}
			stack = append(stack, n)
			pendingKey, pendingNode = "", &stack[len(stack)-1]
			continue
		}

		key, value, found := strings.Cut(trimmed, "=")
		if !found {
			warn(sl, "ignoring unrecognised line %q", trimmed)
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.TrimSpace(value)
		if len(stack) == 0 {
			top[key] = value
			pendingKey, pendingNode = key, nil
		} else {
			cur := &stack[len(stack)-1]
			cur.keys[key] = value
			pendingKey, pendingNode = key, cur
		}
	}
	flush()
	if !sawTargetsHeader {
		warnAt("(input)", "no '*** Targets ***' header found; treating the whole file as targets")
	}

	// File-level defaults become the root's settings.
	if v, ok := top["pings"]; ok {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			f.Defaults.PingsPerInterval = &n
		} else {
			warnAt("(defaults)", "top-level pings = %q is not a positive number; ignored", v)
		}
	}
	if v, ok := top["step"]; ok {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			f.Defaults.IntervalS = &n
		} else {
			warnAt("(defaults)", "top-level step = %q is not a positive number; ignored", v)
		}
	}

	// probe inherits down the tree in SmokePing, so resolve it per node.
	probeAt := map[string]string{"": strings.TrimSpace(top["probe"])}
	for _, n := range nodes {
		inherited := probeAt[parentPath(n.path)]
		if p, ok := n.keys["probe"]; ok {
			inherited = strings.TrimSpace(p)
		}
		probeAt[n.path] = inherited
	}

	for _, n := range nodes {
		entry := Entry{}
		host := strings.TrimSpace(n.keys["host"])

		// Presentation. SmokePing renders these as HTML; smokeng renders them
		// as text, so the markup has to come out here rather than be shown.
		if v := plainText(n.keys["title"]); v != "" {
			entry.Title = strPtr(v)
		} else if v := plainText(n.keys["menu"]); v != "" {
			entry.Title = strPtr(v)
		}
		if v := plainText(n.keys["remark"]); v != "" {
			entry.Notes = strPtr(v)
		}
		if isYes(n.keys["hide"]) {
			entry.Hidden = true
		}

		// Timing.
		if v, ok := n.keys["pings"]; ok {
			if p, err := strconv.Atoi(v); err == nil && p > 0 {
				entry.PingsPerInterval = &p
			} else {
				warnAt(n.path, "pings = %q is not a positive number; ignored", v)
			}
		}
		if v, ok := n.keys["step"]; ok {
			if s, err := strconv.Atoi(v); err == nil && s > 0 {
				entry.IntervalS = &s
			} else {
				warnAt(n.path, "step = %q is not a positive number; ignored", v)
			}
		}

		// Which agents measure this node. SmokePing's slaves add to master
		// polling unless nomasterpoll is set.
		if v, ok := n.keys["slaves"]; ok && strings.TrimSpace(v) != "" {
			agents := strings.Fields(v)
			if !isYes(n.keys["nomasterpoll"]) {
				agents = append([]string{"local"}, agents...)
			}
			list := AgentList(agents)
			entry.Agents = list
			warnAt(n.path, "assigned to agents %q — enrol them before they report, "+
				"or the import that follows will refuse the unknown names", list.String())
		} else if isYes(n.keys["nomasterpoll"]) {
			warnAt(n.path, "nomasterpoll is set but no slaves are listed; the target would never be measured, "+
				"so it stays assigned to the local agent")
		}

		// Things smokeng does not implement.
		if v, ok := n.keys["alerts"]; ok {
			warnAt(n.path, "alerts = %q not imported; smokeng expresses alert rules differently, see docs/alerting.md", v)
		}
		if v, ok := n.keys["parents"]; ok {
			warnAt(n.path, "parents = %q not imported; smokeng has a single target tree, not alternative hierarchies", v)
		}

		switch {
		case host == "":
			// A group node: no host, settings still cascade to its children.
		case strings.EqualFold(host, "DYNAMIC"):
			warnAt(n.path, "host = DYNAMIC not imported; smokeng re-resolves hostnames on their DNS TTL instead")
			continue
		case strings.ContainsAny(host, " \t"):
			warnAt(n.path, "host = %q looks like a multi-host overlay graph; not imported", host)
			continue
		case isDerivedIRTTMetric(probeTypeFor(moduleAt(probeAt[n.path], probeModule)), n.keys):
			// SmokePing's IRTT probe graphs one figure per target — the round
			// trip, or the send or receive jitter — chosen with `metric`. All of
			// them come from the same session, so several such targets are
			// several views of one measurement, not several measurements.
			//
			// Importing each as its own target opens a separate irtt session to
			// the same server, and they collide: only one wins per interval and
			// the rest record as send failures. That is what happened on the
			// first real migration. smokeng keeps the whole round-trip
			// distribution and shows the spread, so the derived views are
			// already in the plot the plain target draws.
			warnAt(n.path, "metric = %q is a derived view of the same irtt session as the "+
				"plain target for this host; not imported — smokeng keeps the whole round-trip "+
				"distribution, so its spread already shows the jitter",
				strings.TrimSpace(n.keys["metric"]))
			continue
		default:
			probe := probeAt[n.path]
			module := probeModule[probe]
			if module == "" {
				module = probe // referenced a built-in module directly
			}
			ptype := probeTypeFor(module)
			fam, dup := familyFor(host, probe, alsoIPv6)
			entry.Host = strPtr(host)
			entry.AddressFamily = strPtr(fam)
			applyProbeType(&entry, ptype, module, n.path, n.keys, warnAt)
			if dup {
				v6 := entry
				v6.Host = strPtr(host)
				v6.AddressFamily = strPtr("v6")
				f.Targets[n.path+"-v6"] = v6
			}
		}
		f.Targets[n.path] = entry
	}
	if len(f.Targets) == 0 {
		return f, warnings, fmt.Errorf("smokeping: no targets found in the file")
	}
	return f, warnings, nil
}

// probeTypeFor maps a SmokePing probe module to a smokeng probe type, or ""
// when it cannot tell. The match is by substring so a subclass named after its
// module ("FPingLarge") still resolves. Order matters: https before http,
// and dns/tcp/http before the icmp fallback.
// moduleAt resolves a probe reference to the module it is built on, following
// the subclass chain a Probes section defines; a name that is not defined there
// is a built-in module referenced directly.
func moduleAt(probe string, probeModule map[string]string) string {
	if m := probeModule[probe]; m != "" {
		return m
	}
	return probe
}

// isDerivedIRTTMetric reports whether this is an IRTT target that graphs a
// figure derived from the same session as the plain one — SmokePing's `metric`
// key, naming send_ipdv or recv_ipdv rather than the round trip. smokeng has no
// use for these as separate targets: they would each open their own session to
// the same server and collide, and the distribution it keeps already contains
// what they show.
func isDerivedIRTTMetric(ptype string, keys map[string]string) bool {
	if ptype != "irtt" {
		return false
	}
	m := strings.ToLower(strings.TrimSpace(keys["metric"]))
	return m != "" && m != "rtt"
}

func probeTypeFor(module string) string {
	m := strings.ToLower(module)
	switch {
	case m == "":
		return "icmp" // no probe named at all: SmokePing's default is fping
	case strings.Contains(m, "dns"):
		return "dns"
	case strings.Contains(m, "https"):
		return "https"
	case strings.Contains(m, "http") || strings.Contains(m, "curl"):
		return "http"
	case strings.Contains(m, "tcp"):
		return "tcp"
	case strings.Contains(m, "irtt"):
		// SmokePing's IRTT probe measures with the same tool smokeng does, so
		// this is the one mapping that carries the measurement method across
		// exactly rather than approximating it. Missing it imported a
		// round-trip-per-packet UDP measurement as an ICMP ping — a different
		// measurement wearing the same graph, which is the failure this whole
		// mapping exists to prevent.
		return "irtt"
	case strings.Contains(m, "fping") || strings.Contains(m, "ping"):
		return "icmp"
	}
	return ""
}

// applyProbeType sets the probe type and the parameters that carry across, and
// warns for the ones that need a human. icmp is left implicit — it is the root
// default, so annotating every ping target with it would only add noise.
func applyProbeType(entry *Entry, ptype, module, path string, keys map[string]string, warnAt func(path, format string, args ...any)) {
	switch ptype {
	case "", "icmp":
		if ptype == "" {
			warnAt(path, "probe %q has no smokeng equivalent; imported as icmp — set probe_type by hand "+
				"if that is wrong", module)
		}
		return
	case "dns":
		entry.ProbeType = strPtr("dns")
		// EchoPingDNS names the record with `lookup` and the type with `recordtype`.
		if v := strings.TrimSpace(keys["lookup"]); v != "" {
			entry.DNSQuery = strPtr(v)
		}
		if v := strings.TrimSpace(keys["recordtype"]); v != "" {
			entry.DNSRRType = strPtr(strings.ToUpper(v))
		}
	case "tcp":
		entry.ProbeType = strPtr("tcp")
		if p := probePort(keys); p != 0 {
			entry.ProbePort = &p
		} else {
			warnAt(path, "imported as tcp but no port was found; smokeng will not measure a tcp target "+
				"without probe_port — set one")
		}
	case "irtt":
		entry.ProbeType = strPtr("irtt")
		// An irtt server's port, where the config names one. Left unset it
		// falls to smokeng's default, the same 2112 irtt itself uses.
		if p := probePort(keys); p != 0 {
			entry.ProbePort = &p
		}
		// A shared HMAC secret is deliberately not carried across: a key in the
		// target tree would travel into the API, the export and version control.
		// Say that it exists and where it goes instead, because without it the
		// server refuses the session and the target reads as a send failure —
		// a silent, puzzling outage rather than a missing setting.
		if strings.TrimSpace(keys["hmac"]) != "" {
			warnAt(path, "this irtt target authenticates with an HMAC key; the key is not "+
				"imported (a secret does not belong in the target tree) — configure it with "+
				"--irtt-hmac-keys, or the server will refuse the session")
		}
		// SmokePing's IRTT probe graphs one of several derived figures — the
		// round trip, or the send or receive jitter — chosen by the probe
		// variant. smokeng keeps the whole round-trip distribution and derives
		// what it shows from that, so a target that existed to graph jitter
		// becomes a target whose spread shows it. Nothing is lost, but the
		// operator should know the graph will not look identical.
		warnAt(path, "imported as irtt; smokeng graphs the round-trip distribution "+
			"rather than a derived jitter figure, so this plot will look different")
	case "http", "https":
		entry.ProbeType = strPtr(ptype)
		if p := probePort(keys); p != 0 {
			entry.ProbePort = &p
		}
		// The path is buried in urlformat/url in ways too varied to reconstruct
		// safely, so say so rather than guess.
		if keys["urlformat"] != "" || keys["url"] != "" {
			warnAt(path, "imported as %s; set http_path if the probe requested a path other than /", ptype)
		}
	}
}

// probePort reads a port from the keys SmokePing probes use for one.
func probePort(keys map[string]string) int {
	for _, k := range []string{"port", "tcp_port"} {
		if v, ok := keys[k]; ok {
			if p, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && p > 0 && p < 65536 {
				return p
			}
		}
	}
	return 0
}

// familyFor decides the address family, which smokeng requires to be explicit.
// A literal address decides itself; otherwise the probe name is the signal,
// and a hostname defaults to v4 — optionally duplicated into a v6 sibling.
func familyFor(host, probe string, alsoIPv6 bool) (family string, duplicate bool) {
	if ip, err := netip.ParseAddr(host); err == nil {
		if ip.Is6() && !ip.Is4In6() {
			return "v6", false
		}
		return "v4", false
	}
	if strings.Contains(strings.ToLower(probe), "6") {
		return "v6", false
	}
	return "v4", alsoIPv6
}

func isYes(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "yes", "true", "1":
		return true
	}
	return false
}

func strPtr(s string) *string { return &s }

// joinContinuation appends a continuation line, dropping the backslash that
// marked the previous line as continued.
func joinContinuation(sofar, next string) string {
	return strings.TrimSuffix(strings.TrimSpace(sofar), "\\") + " " + next
}

// plainText turns one of SmokePing's display strings into text.
//
// SmokePing renders title, menu and remark as HTML and its configs use that:
// entities for punctuation, <b> for emphasis, <br> for a break. smokeng renders
// them as text, so an import carried the markup through literally — "&mdash;"
// where a dash belonged, "<b>" around a word. A migration produced sixteen of
// the first and ten of the second, and they were being corrected by hand one
// title at a time.
//
// Tags are removed before entities are decoded, so a config that escaped a
// literal "<b>" as "&lt;b&gt;" keeps it as text rather than having it stripped
// on the second pass.
func plainText(s string) string {
	s = htmlBreak.ReplaceAllString(s, " ")
	s = htmlTag.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	// Collapsing runs of space last: removing a tag or folding a line leaves
	// doubled spaces that were not in the original.
	return strings.TrimSpace(spaceRun.ReplaceAllString(s, " "))
}

var (
	htmlBreak = regexp.MustCompile(`(?i)<br\s*/?>`)
	htmlTag   = regexp.MustCompile(`</?[a-zA-Z][a-zA-Z0-9]*(\s[^<>]*)?/?>`)
	spaceRun  = regexp.MustCompile(`[ \t]{2,}`)
)
