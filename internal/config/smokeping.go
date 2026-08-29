package config

import (
	"bufio"
	"bytes"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
)

// ParseSmokePing converts a SmokePing Targets file into smokeng's
// configuration form. The `+`/`++`/`+++` hierarchy becomes the target tree and
// per-node keys become local settings, so inheritance survives the move
// rather than being flattened.
//
// Anything smokeng deliberately does not implement — alert definitions,
// alternative hierarchies, multi-host overlay graphs, DYNAMIC hosts — is
// reported as a warning rather than dropped in silence. An adoption path that
// quietly loses configuration is worse than one that refuses it out loud.
func ParseSmokePing(data []byte, alsoIPv6 bool) (File, []string, error) {
	f := File{Targets: map[string]Entry{}}
	var warnings []string
	warn := func(format string, args ...any) {
		warnings = append(warnings, fmt.Sprintf(format, args...))
	}

	type node struct {
		path string
		keys map[string]string
	}
	var stack []node           // one entry per depth level
	top := map[string]string{} // keys before the first '+' line
	nodes := []node{}          // completed nodes, in file order
	inTargets := true          // until a section header says otherwise
	sawTargetsHeader := false

	var pendingKey string
	var pendingNode *node

	flush := func() {
		if len(stack) > 0 {
			nodes = append(nodes, stack[len(stack)-1])
		}
	}

	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	lineNo := 0
	for sc.Scan() {
		raw := sc.Text()
		lineNo++
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}

		// Section headers: *** Targets ***, *** Probes ***, ...
		if strings.HasPrefix(trimmed, "***") && strings.HasSuffix(trimmed, "***") {
			name := strings.ToLower(strings.TrimSpace(strings.Trim(trimmed, "* ")))
			inTargets = name == "targets"
			sawTargetsHeader = sawTargetsHeader || inTargets
			pendingKey, pendingNode = "", nil
			continue
		}
		if !inTargets {
			continue
		}
		if strings.HasPrefix(trimmed, "@include") {
			warn("line %d: @include is not followed; import the included file separately", lineNo)
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
				return f, warnings, fmt.Errorf("smokeping: line %d: %q has no name", lineNo, trimmed)
			}
			if depth > len(stack)+1 {
				return f, warnings, fmt.Errorf("smokeping: line %d: %q skips a level", lineNo, trimmed)
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
			warn("line %d: ignoring unrecognised line %q", lineNo, trimmed)
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
	if err := sc.Err(); err != nil {
		return f, warnings, fmt.Errorf("smokeping: read: %w", err)
	}
	flush()
	if !sawTargetsHeader {
		warn("no '*** Targets ***' header found; treating the whole file as targets")
	}

	// File-level defaults become the root's settings.
	if v, ok := top["pings"]; ok {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			f.Defaults.PingsPerInterval = &n
		} else {
			warn("top-level pings = %q is not a positive number; ignored", v)
		}
	}
	if v, ok := top["step"]; ok {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			f.Defaults.IntervalS = &n
		} else {
			warn("top-level step = %q is not a positive number; ignored", v)
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
				warn("%s: pings = %q is not a positive number; ignored", n.path, v)
			}
		}
		if v, ok := n.keys["step"]; ok {
			if s, err := strconv.Atoi(v); err == nil && s > 0 {
				entry.IntervalS = &s
			} else {
				warn("%s: step = %q is not a positive number; ignored", n.path, v)
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
			warn("%s: assigned to agents %q — enrol them before they report, "+
				"or the import that follows will refuse the unknown names",
				n.path, list.String())
		} else if isYes(n.keys["nomasterpoll"]) {
			warn("%s: nomasterpoll is set but no slaves are listed; the target would never be measured, "+
				"so it stays assigned to the local agent", n.path)
		}

		// Things smokeng does not implement.
		if v, ok := n.keys["alerts"]; ok {
			warn("%s: alerts = %q not imported; smokeng expresses alert rules differently, see docs/alerting.md", n.path, v)
		}
		if v, ok := n.keys["parents"]; ok {
			warn("%s: parents = %q not imported; smokeng has a single target tree, not alternative hierarchies",
				n.path, v)
		}

		switch {
		case host == "":
			// A group node: no host, settings still cascade to its children.
		case strings.EqualFold(host, "DYNAMIC"):
			warn("%s: host = DYNAMIC not imported; smokeng re-resolves hostnames on their DNS TTL instead",
				n.path)
			continue
		case strings.ContainsAny(host, " \t"):
			warn("%s: host = %q looks like a multi-host overlay graph; not imported", n.path, host)
			continue
		default:
			probe := probeAt[n.path]
			fam, dup := familyFor(host, probe, alsoIPv6)
			entry.Host = strPtr(host)
			entry.AddressFamily = strPtr(fam)
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
