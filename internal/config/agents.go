package config

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/timdebruijn/smokeng/internal/store"
	"github.com/timdebruijn/smokeng/internal/tree"
)

// LocalAgent is the master's own prober. It is reserved: no enrolled agent may
// be called this, so a name in an `agents` list always means exactly one thing.
const LocalAgent = "local"

// AgentList is the set of vantage points that measure a node and its subtree
// (DESIGN.md §4.4).
//
// TOML accepts either an array — `agents = ["local", "ams-01"]`, which is what
// this is — or the space-separated string the format used before v0.6, because
// configurations written against that are still out there. It is stored and
// resolved as the space-separated form throughout, so nothing below the parser
// has to know which spelling the file used.
type AgentList []string

// normaliseAgents turns whatever the TOML decoder produced for an `agents`
// key into an AgentList.
//
// The decoder handles the array form natively, because AgentList is a []string.
// The pre-v0.6 space-separated string needs converting, and go-toml's
// unmarshaler interface — which would let AgentList decode itself — is opt-in
// and documented as unstable, which is not a foundation for a config format.
// So the field is decoded as `any` and normalised here.
func normaliseAgents(v any) (AgentList, error) {
	switch t := v.(type) {
	case nil:
		return nil, nil
	case AgentList:
		return t, nil
	case string:
		return AgentList(strings.Fields(t)), nil
	case []string:
		return AgentList(t), nil
	case []any:
		out := make(AgentList, 0, len(t))
		for i, e := range t {
			name, ok := e.(string)
			if !ok {
				return nil, fmt.Errorf("agents[%d] is %T, want a name", i, e)
			}
			// A name containing a space could never be matched, because the
			// resolved form is space-separated. Refuse it here rather than let
			// it silently name nothing.
			if len(strings.Fields(name)) != 1 {
				return nil, fmt.Errorf("agents[%d] = %q is not a single name", i, name)
			}
			out = append(out, name)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("agents is %T, want an array of names", v)
	}
}

// String renders the resolved form.
func (a AgentList) String() string { return strings.Join(a, " ") }

func agentListFrom(s *string) any {
	if s == nil {
		return nil
	}
	return orNil(AgentList(strings.Fields(*s)))
}

func agentStringFrom(v any) *string {
	list, err := normaliseAgents(v)
	if err != nil || list == nil {
		return nil
	}
	s := list.String()
	return &s
}

// knownAgents is the set of names an `agents` list may legally contain.
func knownAgents(ctx context.Context, st Store) (map[string]bool, error) {
	lister, ok := st.(interface {
		ListAgents(ctx context.Context) ([]store.AgentRecord, error)
	})
	if !ok {
		// A store that cannot enumerate agents cannot be checked against.
		return nil, nil
	}
	agents, err := lister.ListAgents(ctx)
	if err != nil {
		return nil, err
	}
	known := map[string]bool{LocalAgent: true}
	for _, a := range agents {
		known[a.Name] = true
	}
	return known, nil
}

// checkAgentRefs reports every `agents` entry that names nothing.
//
// This exists because the failure it catches is silent: a target assigned to
// an agent that was never enrolled is measured by nobody, and draws exactly
// like a target that is measured and answering nothing. That is the one place
// smokeng hid something it knew (DESIGN.md §4.4).
func checkAgentRefs(nodes map[string]*tree.Target, declared, known map[string]bool) []string {
	if known == nil {
		return nil
	}
	var problems []string
	paths := make([]string, 0, len(nodes))
	for p := range nodes {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		// Only what this file declares. A node that predates the import may
		// name an agent that has since been removed, and failing the whole
		// apply over something the operator did not just write would be a
		// surprise rather than a warning.
		if !declared[p] {
			continue
		}
		n := nodes[p]
		if n.Settings.Agents == nil {
			continue
		}
		names := strings.Fields(*n.Settings.Agents)
		var unknown []string
		for _, name := range names {
			if !known[name] {
				unknown = append(unknown, name)
			}
		}
		if len(unknown) == 0 {
			continue
		}
		where := p
		if where == "" {
			where = "[defaults]"
		}
		problems = append(problems, fmt.Sprintf("%s: no enrolled agent named %s",
			where, strings.Join(quoteAll(unknown), ", ")))
	}
	return problems
}

func quoteAll(names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = fmt.Sprintf("%q", n)
	}
	return out
}

// agentRefError turns the problems into one error that also says what the
// operator could have meant. A rejection that does not list the alternatives
// makes them go and read the database.
func agentRefError(problems []string, known map[string]bool) error {
	names := make([]string, 0, len(known))
	for n := range known {
		names = append(names, n)
	}
	sort.Strings(names)
	return fmt.Errorf("config: %s\nenrolled agents are: %s\n"+
		"enrol it first with `smokeng agent add`, or pass --allow-unknown-agents "+
		"if the tree is meant to land before the agent does",
		strings.Join(problems, "\n"), strings.Join(names, ", "))
}

// normaliseAgentLists rewrites every `agents` value in the file to an
// AgentList, so the shape is validated once and nothing downstream has to know
// which spelling the file used.
func (f *File) normaliseAgentLists() error {
	list, err := normaliseAgents(f.Defaults.Agents)
	if err != nil {
		return fmt.Errorf("config: [defaults]: %w", err)
	}
	// Assigning a typed nil into an `any` leaves a non-nil interface, and every
	// "is this set?" check downstream would then wipe the value it guards.
	f.Defaults.Agents = orNil(list)
	for p, e := range f.Targets {
		list, err := normaliseAgents(e.Agents)
		if err != nil {
			return fmt.Errorf("config: %s: %w", p, err)
		}
		e.Agents = orNil(list)
		f.Targets[p] = e
	}
	return nil
}

func orNil(a AgentList) any {
	if a == nil {
		return nil
	}
	return a
}
