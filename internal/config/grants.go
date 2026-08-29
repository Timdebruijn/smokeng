package config

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/timdebruijn/smokeng/internal/store"
	"github.com/timdebruijn/smokeng/internal/tree"
)

// GrantEntry is one grant as TOML expresses it. Targets are named by path
// here, as everywhere else in this format: an id is a fact about a database,
// not about a configuration someone keeps in a repository.
type GrantEntry struct {
	Group string `toml:"group"`
	Path  string `toml:"path"`
	Role  string `toml:"role"`
}

// GrantStore is the persistence grants need. It is separate from Store so an
// import against something that cannot hold grants simply does not sync them,
// rather than failing.
type GrantStore interface {
	ListGrants(ctx context.Context) ([]store.Grant, error)
	UpsertGrant(ctx context.Context, g *store.Grant) error
	DeleteGrant(ctx context.Context, id int64) error
}

// syncGrants applies the file's grants.
//
// Grants are fully declarative in a way targets are not: a grant absent from
// the file is removed, with no --prune to ask for it. The asymmetry is
// deliberate. Absence disables a target because its measurements are the
// product and deleting them would be the destructive act; a grant holds no
// data, and the destructive direction for authorisation is the one that leaves
// stale access in place.
//
// The exception is a file with no `grants` key at all, which leaves them
// alone. Otherwise importing a targets-only file would silently revoke
// everything.
func syncGrants(ctx context.Context, st Store, f File, tr *tree.Tree, targets []tree.Target, sum *Summary) error {
	if f.Grants == nil {
		return nil
	}
	gs, ok := st.(GrantStore)
	if !ok {
		return nil
	}
	byPath := map[string]int64{}
	for i := range targets {
		p, err := tr.Path(targets[i].ID)
		if err != nil {
			return err
		}
		byPath[strings.TrimPrefix(p, "/")] = targets[i].ID
	}

	type key struct {
		group string
		id    int64
	}
	wanted := map[key]string{}
	for i, e := range *f.Grants {
		group := strings.TrimSpace(e.Group)
		if group == "" {
			return fmt.Errorf("config: grants[%d]: group is required", i)
		}
		if e.Role != "viewer" && e.Role != "editor" {
			return fmt.Errorf("config: grants[%d]: role must be viewer or editor, got %q", i, e.Role)
		}
		path := strings.Trim(e.Path, "/")
		id, ok := byPath[path]
		if !ok {
			return fmt.Errorf("config: grants[%d]: no target at %q", i, e.Path)
		}
		wanted[key{group, id}] = e.Role
	}

	current, err := gs.ListGrants(ctx)
	if err != nil {
		return err
	}
	have := map[key]store.Grant{}
	for _, g := range current {
		have[key{g.Group, g.TargetID}] = g
	}

	// Deterministic order, so an import reports the same thing twice running.
	keys := make([]key, 0, len(wanted))
	for k := range wanted {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].group != keys[j].group {
			return keys[i].group < keys[j].group
		}
		return keys[i].id < keys[j].id
	})

	for _, k := range keys {
		role := wanted[k]
		if prev, ok := have[k]; ok {
			if prev.Role == role {
				continue
			}
			prev.Role = role
			if err := gs.UpsertGrant(ctx, &prev); err != nil {
				return err
			}
			sum.GrantsUpdated++
			continue
		}
		g := store.Grant{Group: k.group, TargetID: k.id, Role: role}
		if err := gs.UpsertGrant(ctx, &g); err != nil {
			return err
		}
		sum.GrantsCreated++
	}
	for k, g := range have {
		if _, ok := wanted[k]; ok {
			continue
		}
		if err := gs.DeleteGrant(ctx, g.ID); err != nil {
			return err
		}
		sum.GrantsRemoved++
	}
	return nil
}

// exportGrants renders the grants as TOML entries, sorted so an export is
// stable enough to keep in a repository.
func exportGrants(ctx context.Context, st Store, tr *tree.Tree) ([]GrantEntry, error) {
	gs, ok := st.(GrantStore)
	if !ok {
		return nil, nil
	}
	current, err := gs.ListGrants(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]GrantEntry, 0, len(current))
	for _, g := range current {
		p, err := tr.Path(g.TargetID)
		if err != nil {
			// A grant on a node that no longer exists cannot be written down.
			continue
		}
		out = append(out, GrantEntry{
			Group: g.Group, Path: strings.TrimPrefix(p, "/"), Role: g.Role,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Group != out[j].Group {
			return out[i].Group < out[j].Group
		}
		return out[i].Path < out[j].Path
	})
	return out, nil
}
