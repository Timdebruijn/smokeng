package config

import (
	"bufio"
	"bytes"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
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

		// A line starting with whitespace continues the previous value.
		if pendingKey != "" && (strings.HasPrefix(raw, " ") || strings.HasPrefix(raw, "\t")) {
			if pendingNode != nil {
				pendingNode.keys[pendingKey] += " " + trimmed
			} else {
				top[pendingKey] += " " + trimmed
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

		// Presentation.
		if v := n.keys["title"]; v != "" {
			entry.Title = strPtr(v)
		} else if v := n.keys["menu"]; v != "" {
			entry.Title = strPtr(v)
		}
		if v := n.keys["remark"]; v != "" {
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
