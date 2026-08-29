package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/timdebruijn/smokeng/internal/auth"
	"github.com/timdebruijn/smokeng/internal/store"
	"github.com/timdebruijn/smokeng/internal/tree"
)

// GrantStore is the slice of the store scoped authorisation needs.
type GrantStore interface {
	ListGrants(ctx context.Context) ([]store.Grant, error)
	UpsertGrant(ctx context.Context, g *store.Grant) error
	DeleteGrant(ctx context.Context, id int64) error
}

// Scope is what one session may see and do, resolved against the tree
// (DESIGN.md §7.4).
//
// Everything outside a scope does not exist as far as the responses built from
// it are concerned: not the nodes, not their names, not the fact that there are
// any. That is the requirement — one customer must not learn that there are
// others — and it is why the read paths, not the write paths, are where the
// work is.
type Scope struct {
	// global is the role held over the whole installation: admin from the
	// admin claim, viewer when --default-role allows it, or empty for a user
	// whose access comes only from grants.
	global auth.Role
	// roots maps a granted node to the role held on it and its subtree.
	roots map[int64]auth.Role
	tr    *tree.Tree
}

// Unrestricted is the scope of an instance running without authentication, and
// of a global admin: everything, everywhere.
func Unrestricted(tr *tree.Tree) *Scope {
	return &Scope{global: auth.RoleAdmin, tr: tr}
}

// scopeFor resolves the caller's scope. It is the single place a session turns
// into permissions, so that adding a role or a grant kind cannot be done in
// half the handlers.
func (s *server) scopeFor(r *http.Request, tr *tree.Tree) (*Scope, error) {
	if s.auth == nil {
		return Unrestricted(tr), nil
	}
	sess, ok := s.auth.SessionFrom(r)
	if !ok {
		return &Scope{tr: tr}, nil
	}
	sc := &Scope{tr: tr, roots: map[int64]auth.Role{}}
	if sess.Role == auth.RoleAdmin {
		sc.global = auth.RoleAdmin
		return sc, nil
	}
	sc.global = s.defaultRole

	if s.grants != nil && len(sess.Groups) > 0 {
		held := map[string]bool{}
		for _, g := range sess.Groups {
			held[g] = true
		}
		all, err := s.grants.ListGrants(r.Context())
		if err != nil {
			return nil, err
		}
		for _, g := range all {
			if !held[g.Group] {
				continue
			}
			sc.roots[g.TargetID] = auth.Max(sc.roots[g.TargetID], auth.Role(g.Role))
		}
	}
	return sc, nil
}

// RoleOn returns the role the caller holds on a node, or the empty role if the
// node is not theirs to know about. A grant on an ancestor carries down, the
// same way every other setting on this tree does.
func (sc *Scope) RoleOn(id int64) auth.Role {
	if sc.global.AtLeast(auth.RoleAdmin) {
		return auth.RoleAdmin
	}
	role := sc.global
	if len(sc.roots) > 0 {
		for _, anc := range sc.chain(id) {
			if r, ok := sc.roots[anc]; ok {
				role = auth.Max(role, r)
			}
		}
	}
	return role
}

// Visible reports whether a node exists as far as this caller is concerned.
func (sc *Scope) Visible(id int64) bool { return sc.RoleOn(id).AtLeast(auth.RoleViewer) }

// CanWrite reports whether the caller may change a node.
func (sc *Scope) CanWrite(id int64) bool { return sc.RoleOn(id).AtLeast(auth.RoleEditor) }

// IsGlobalAdmin gates the things a grant never confers: agents, enrolment
// tokens, the root defaults, /metrics, and TOML import and export, which are
// declarative over the whole tree and cannot express a partial apply.
func (sc *Scope) IsGlobalAdmin() bool { return sc.global.AtLeast(auth.RoleAdmin) }

// RootOf returns the node a caller's view is rooted at: the highest ancestor
// they hold a grant on, or 0 when they can see the real root. Paths are
// rendered relative to it, so a scoped caller sees their subtree as though it
// were the whole installation.
func (sc *Scope) RootOf(id int64) int64 {
	if sc.global.AtLeast(auth.RoleViewer) {
		return 0
	}
	var root int64
	for _, anc := range sc.chain(id) {
		if _, ok := sc.roots[anc]; ok {
			root = anc // chain runs root-first, so the last hit is the highest
			break
		}
	}
	return root
}

// PathIn renders a node's path as the caller should see it: relative to their
// scope root, with the root's own name kept so two disjoint grants stay
// distinguishable.
func (sc *Scope) PathIn(id int64) (string, error) {
	full, err := sc.tr.Path(id)
	if err != nil {
		return "", err
	}
	root := sc.RootOf(id)
	if root == 0 {
		return full, nil
	}
	rootPath, err := sc.tr.Path(root)
	if err != nil {
		return "", err
	}
	// The grant root keeps its own name; everything above it disappears.
	parent := rootPath[:strings.LastIndex(rootPath, "/")]
	if parent == "" {
		return full, nil
	}
	return strings.TrimPrefix(full, parent), nil
}

// chain lists a node and its ancestors, root first.
func (sc *Scope) chain(id int64) []int64 {
	var out []int64
	for cur := id; cur != 0; {
		n, ok := sc.tr.Get(cur)
		if !ok {
			break
		}
		out = append(out, cur)
		if n.ParentID == nil {
			break
		}
		cur = *n.ParentID
	}
	// Reverse: callers want root-first so "the highest grant" is the first hit.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// forbidden answers a request for something the caller may not change. A node
// they cannot even see is answered as absent rather than as forbidden: "you
// may not touch /Klanten/GemeenteY" confirms that it exists.
func (sc *Scope) deny(w http.ResponseWriter, id int64) {
	if !sc.Visible(id) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such target"})
		return
	}
	writeJSON(w, http.StatusForbidden, map[string]string{
		"error": "this action needs write access to that part of the tree",
	})
}

// withScope loads the tree and resolves the caller's scope against it. Every
// scoped handler starts here, so there is one place that decides what a
// request may reach.
func (s *server) withScope(w http.ResponseWriter, r *http.Request) (*Scope, []tree.Target, bool) {
	targets, err := s.st.ListTargets(r.Context())
	if err != nil {
		internalError(w, err)
		return nil, nil, false
	}
	tr, err := tree.New(targets)
	if err != nil {
		internalError(w, err)
		return nil, nil, false
	}
	sc, err := s.scopeFor(r, tr)
	if err != nil {
		internalError(w, err)
		return nil, nil, false
	}
	return sc, targets, true
}

// requireWrite resolves the scope and checks the caller may change the node.
// It answers and returns false when they may not.
func (s *server) requireWrite(w http.ResponseWriter, r *http.Request, id int64) (*Scope, bool) {
	sc, _, ok := s.withScope(w, r)
	if !ok {
		return nil, false
	}
	if !sc.CanWrite(id) {
		sc.deny(w, id)
		return nil, false
	}
	return sc, true
}

// visibleIDs is the set of nodes this caller may know about.
func (sc *Scope) visibleIDs(targets []tree.Target) map[int64]bool {
	out := make(map[int64]bool, len(targets))
	for i := range targets {
		if sc.Visible(targets[i].ID) {
			out[targets[i].ID] = true
		}
	}
	return out
}

// requireVisible resolves the scope and checks the caller may know the node
// exists. A node outside their scope is answered as absent, never as
// forbidden: "you may not read that" confirms there is something to read.
func (s *server) requireVisible(w http.ResponseWriter, r *http.Request, id int64) bool {
	sc, _, ok := s.withScope(w, r)
	if !ok {
		return false
	}
	if !sc.Visible(id) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such target"})
		return false
	}
	return true
}
