package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/timdebruijn/smokeng/internal/store"
	"github.com/timdebruijn/smokeng/internal/tree"
)

// handleTargets returns the full tree. Every inheritable setting is a
// {local, effective, source} object (DESIGN.md §4.2) — never a flat value, so
// the UI can say "20 pings, inherited from Production" and offer an override.
func (s *server) handleTargets(w http.ResponseWriter, r *http.Request) {
	targets, err := s.st.ListTargets(r.Context())
	if err != nil {
		internalError(w, err)
		return
	}
	tr, err := tree.New(targets)
	if err != nil {
		internalError(w, err)
		return
	}
	out := make([]map[string]any, 0, len(targets))
	for i := range targets {
		body, err := targetJSON(tr, &targets[i])
		if err != nil {
			internalError(w, err)
			return
		}
		out = append(out, body)
	}
	writeJSON(w, http.StatusOK, map[string]any{"targets": out})
}

func targetJSON(tr *tree.Tree, n *tree.Target) (map[string]any, error) {
	res, err := tr.Resolve(n.ID)
	if err != nil {
		return nil, err
	}
	path, err := tr.Path(n.ID)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"id":             n.ID,
		"parent_id":      n.ParentID,
		"name":           n.Name,
		"path":           path,
		"host":           n.Host,
		"address_family": n.AddressFamily,
		"title":          n.Title,
		"notes":          n.Notes,
		"hidden":         n.Hidden,
		"enabled":        n.Enabled,
		"sort_order":     n.SortOrder,
		"is_group":       n.Host == nil,
		"settings": map[string]any{
			"interval_s":         settingJSON(n.ID, res.IntervalS),
			"pings_per_interval": settingJSON(n.ID, res.PingsPerInterval),
			"probe_mode":         settingJSON(n.ID, res.ProbeMode),
			"burst_gap_ms":       settingJSON(n.ID, res.BurstGapMS),
			"timeout_ms":         settingJSON(n.ID, res.TimeoutMS),
			"packet_size":        settingJSON(n.ID, res.PacketSize),
			"dscp":               settingJSON(n.ID, res.DSCP),
			"agents":             settingJSON(n.ID, res.Agents),
			"trace_interval_s":   settingJSON(n.ID, res.TraceIntervalS),
		},
	}, nil
}

// settingJSON renders one resolved setting: source is the literal string
// "local" when the node sets the value itself, else the providing ancestor.
func settingJSON[T any](nodeID int64, v tree.Value[T]) map[string]any {
	src := any("local")
	if v.Source.ID != nodeID {
		src = v.Source
	}
	return map[string]any{"local": v.Local, "effective": v.Effective, "source": src}
}

// handleCreateTarget adds a node. Settings absent from the payload stay NULL,
// which means "inherit".
func (s *server) handleCreateTarget(w http.ResponseWriter, r *http.Request) {
	body, err := decodeObject(r)
	if err != nil {
		badRequest(w, err)
		return
	}
	targets, err := s.st.ListTargets(r.Context())
	if err != nil {
		internalError(w, err)
		return
	}

	n := tree.Target{Enabled: true}
	if err := applyPatch(&n, body); err != nil {
		badRequest(w, err)
		return
	}
	if err := s.checkAgentNames(r.Context(), n.Settings.Agents); err != nil {
		badRequest(w, err)
		return
	}
	if n.ParentID == nil {
		badRequest(w, errors.New("parent_id is required; there is exactly one root and it already exists"))
		return
	}

	// Validate the whole resulting tree before writing anything. The new node
	// gets a synthetic id so inheritance and structure can be checked.
	planned := append(append([]tree.Target(nil), targets...), n)
	planned[len(planned)-1].ID = synthID(targets)
	if _, err := tree.New(planned); err != nil {
		badRequest(w, err)
		return
	}

	if err := s.st.UpsertTarget(r.Context(), &n); err != nil {
		internalError(w, err)
		return
	}
	s.respondTarget(w, r, n.ID, http.StatusCreated)
}

// handleUpdateTarget applies a partial update. A setting given as null is
// cleared to NULL, which reverts it to inheritance (DESIGN.md §4.2).
func (s *server) handleUpdateTarget(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		badRequest(w, errors.New("bad target id"))
		return
	}
	body, err := decodeObject(r)
	if err != nil {
		badRequest(w, err)
		return
	}
	targets, err := s.st.ListTargets(r.Context())
	if err != nil {
		internalError(w, err)
		return
	}

	idx := -1
	for i := range targets {
		if targets[i].ID == id {
			idx = i
		}
	}
	if idx < 0 {
		notFound(w)
		return
	}
	if targets[idx].ParentID == nil {
		if _, moving := body["parent_id"]; moving {
			badRequest(w, errors.New("the root target cannot be reparented"))
			return
		}
	}

	updated := targets[idx]
	if err := applyPatch(&updated, body); err != nil {
		badRequest(w, err)
		return
	}
	if err := s.checkAgentNames(r.Context(), updated.Settings.Agents); err != nil {
		badRequest(w, err)
		return
	}
	planned := append([]tree.Target(nil), targets...)
	planned[idx] = updated
	if _, err := tree.New(planned); err != nil {
		badRequest(w, err)
		return
	}

	if err := s.st.UpsertTarget(r.Context(), &updated); err != nil {
		internalError(w, err)
		return
	}
	s.respondTarget(w, r, id, http.StatusOK)
}

// handleDeleteTarget removes a node. Measurements are never deleted: history
// outlives the target row. A node with children needs ?recursive=true.
func (s *server) handleDeleteTarget(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		badRequest(w, errors.New("bad target id"))
		return
	}
	targets, err := s.st.ListTargets(r.Context())
	if err != nil {
		internalError(w, err)
		return
	}
	byID := map[int64]*tree.Target{}
	children := map[int64][]int64{}
	for i := range targets {
		byID[targets[i].ID] = &targets[i]
		if p := targets[i].ParentID; p != nil {
			children[*p] = append(children[*p], targets[i].ID)
		}
	}
	n, ok := byID[id]
	if !ok {
		notFound(w)
		return
	}
	if n.ParentID == nil {
		badRequest(w, errors.New("the root target cannot be deleted"))
		return
	}

	// Collect the subtree, deepest first, so foreign keys stay satisfied.
	var order []int64
	var walk func(int64)
	walk = func(cur int64) {
		for _, c := range children[cur] {
			walk(c)
		}
		order = append(order, cur)
	}
	walk(id)
	if len(order) > 1 && r.URL.Query().Get("recursive") != "true" {
		badRequest(w, fmt.Errorf("target %d has %d descendant(s); pass ?recursive=true to delete them too",
			id, len(order)-1))
		return
	}
	for _, victim := range order {
		if err := s.st.DeleteTarget(r.Context(), victim); err != nil {
			internalError(w, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": order})
}

func (s *server) respondTarget(w http.ResponseWriter, r *http.Request, id int64, status int) {
	targets, err := s.st.ListTargets(r.Context())
	if err != nil {
		internalError(w, err)
		return
	}
	tr, err := tree.New(targets)
	if err != nil {
		internalError(w, err)
		return
	}
	for i := range targets {
		if targets[i].ID == id {
			body, err := targetJSON(tr, &targets[i])
			if err != nil {
				internalError(w, err)
				return
			}
			writeJSON(w, status, body)
			return
		}
	}
	internalError(w, fmt.Errorf("api: target %d vanished after write", id))
}

// applyPatch mutates n with the fields present in body. A key that is absent
// leaves the field alone; a key explicitly set to null clears it. That
// distinction is the whole point of the override UI, so the payload is
// decoded key-by-key rather than into a struct.
// checkAgentNames refuses an `agents` list that names an agent nobody enrolled.
// The UI offers a picker so it cannot happen there, but the API is the API, and
// the failure it prevents is a target measured by nobody (DESIGN.md §4.4).
func (s *server) checkAgentNames(ctx context.Context, agents *string) error {
	if agents == nil {
		return nil
	}
	records, err := s.agents.ListAgents(ctx)
	if err != nil {
		return err
	}
	known := map[string]bool{store.LocalAgentName: true}
	names := make([]string, 0, len(records)+1)
	names = append(names, store.LocalAgentName)
	for _, a := range records {
		if a.ID == store.LocalAgentID {
			continue
		}
		known[a.Name] = true
		names = append(names, a.Name)
	}
	var unknown []string
	for _, want := range strings.Fields(*agents) {
		if !known[want] {
			unknown = append(unknown, strconv.Quote(want))
		}
	}
	if len(unknown) > 0 {
		sort.Strings(names)
		return fmt.Errorf("no enrolled agent named %s; enrolled agents are: %s",
			strings.Join(unknown, ", "), strings.Join(names, ", "))
	}
	return nil
}

func applyPatch(n *tree.Target, body map[string]json.RawMessage) error {
	if raw, ok := body["parent_id"]; ok {
		if isNull(raw) {
			return errors.New("parent_id may not be null; there is exactly one root")
		}
		var v int64
		if err := json.Unmarshal(raw, &v); err != nil {
			return fmt.Errorf("parent_id: %w", err)
		}
		n.ParentID = &v
	}
	if err := patchString(body, "name", func(v *string) error {
		if v == nil {
			return errors.New("name may not be null")
		}
		n.Name = *v
		return nil
	}); err != nil {
		return err
	}
	if err := patchString(body, "host", func(v *string) error { n.Host = v; return nil }); err != nil {
		return err
	}
	if err := patchString(body, "address_family", func(v *string) error { n.AddressFamily = v; return nil }); err != nil {
		return err
	}
	if err := patchString(body, "title", func(v *string) error { n.Title = v; return nil }); err != nil {
		return err
	}
	if err := patchString(body, "notes", func(v *string) error { n.Notes = v; return nil }); err != nil {
		return err
	}
	for key, dst := range map[string]*bool{"hidden": &n.Hidden, "enabled": &n.Enabled} {
		if raw, ok := body[key]; ok {
			var v bool
			if err := json.Unmarshal(raw, &v); err != nil {
				return fmt.Errorf("%s: %w", key, err)
			}
			*dst = v
		}
	}
	if raw, ok := body["sort_order"]; ok {
		var v int
		if err := json.Unmarshal(raw, &v); err != nil {
			return fmt.Errorf("sort_order: %w", err)
		}
		n.SortOrder = v
	}

	if raw, ok := body["settings"]; ok {
		var settings map[string]json.RawMessage
		if err := json.Unmarshal(raw, &settings); err != nil {
			return fmt.Errorf("settings: %w", err)
		}
		ints := map[string]**int{
			"interval_s":         &n.Settings.IntervalS,
			"pings_per_interval": &n.Settings.PingsPerInterval,
			"burst_gap_ms":       &n.Settings.BurstGapMS,
			"timeout_ms":         &n.Settings.TimeoutMS,
			"packet_size":        &n.Settings.PacketSize,
			"dscp":               &n.Settings.DSCP,
			"trace_interval_s":   &n.Settings.TraceIntervalS,
		}
		strs := map[string]**string{
			"probe_mode": &n.Settings.ProbeMode,
			"agents":     &n.Settings.Agents,
		}
		for key, raw := range settings {
			// The UI sends agents as an array; a string still works and means
			// the same thing.
			if key == "agents" && !isNull(raw) {
				var list []string
				if err := json.Unmarshal(raw, &list); err == nil {
					joined := strings.Join(list, " ")
					n.Settings.Agents = &joined
					continue
				}
			}
			switch {
			case ints[key] != nil:
				if isNull(raw) {
					*ints[key] = nil
					continue
				}
				var v int
				if err := json.Unmarshal(raw, &v); err != nil {
					return fmt.Errorf("settings.%s: %w", key, err)
				}
				*ints[key] = &v
			case strs[key] != nil:
				if isNull(raw) {
					*strs[key] = nil
					continue
				}
				var v string
				if err := json.Unmarshal(raw, &v); err != nil {
					return fmt.Errorf("settings.%s: %w", key, err)
				}
				*strs[key] = &v
			default:
				return fmt.Errorf("settings.%s: unknown setting", key)
			}
		}
	}
	if n.Name != "" {
		n.Name = strings.TrimSpace(n.Name)
	}
	return nil
}

func patchString(body map[string]json.RawMessage, key string, set func(*string) error) error {
	raw, ok := body[key]
	if !ok {
		return nil
	}
	if isNull(raw) {
		return set(nil)
	}
	var v string
	if err := json.Unmarshal(raw, &v); err != nil {
		return fmt.Errorf("%s: %w", key, err)
	}
	return set(&v)
}

func isNull(raw json.RawMessage) bool {
	return string(raw) == "null"
}

func decodeObject(r *http.Request) (map[string]json.RawMessage, error) {
	var body map[string]json.RawMessage
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20)).Decode(&body); err != nil {
		return nil, fmt.Errorf("invalid JSON body: %w", err)
	}
	return body, nil
}

// synthID returns an id no existing target uses, for validating a node that
// has not been written yet.
func synthID(targets []tree.Target) int64 {
	maxID := int64(0)
	for _, t := range targets {
		maxID = max(maxID, t.ID)
	}
	return maxID + 1
}
