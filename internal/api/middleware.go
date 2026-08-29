package api

import (
	"net/http"

	"github.com/timdebruijn/smokeng/internal/auth"
)

// Authenticator is the part of OIDC the API needs. Nil means smokeng is
// running unauthenticated, which is only permitted on loopback.
type Authenticator interface {
	SessionFrom(r *http.Request) (auth.Session, bool)
	Routes(mux *http.ServeMux)
}

// requireRole wraps a handler so that only sessions holding at least the
// given role reach it. Reads need a viewer, writes need an admin: the two
// roles the design allows, and no more.
//
// The check is on the method rather than on each route because the rule is
// uniform — anything that changes state is an admin action — and a per-route
// list would eventually miss a route.
func (s *server) requireRole(want auth.Role, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.auth == nil {
			next(w, r)
			return
		}
		sess, ok := s.auth.SessionFrom(r)
		if !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]string{
				"error": "not signed in",
				"login": "/auth/login",
			})
			return
		}
		if want == auth.RoleAdmin && sess.Role != auth.RoleAdmin {
			writeJSON(w, http.StatusForbidden, map[string]string{
				"error": "this action needs the admin role",
			})
			return
		}
		next(w, r)
	}
}

// handleMe reports who the caller is, so the UI can show the signed-in user
// and hide controls it knows will be refused.
func (s *server) handleMe(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"authenticated": false,
			"auth_enabled":  false,
			"role":          string(auth.RoleAdmin),
			"version":       s.version,
			"external_url":  s.externalURL,
		})
		return
	}
	sess, ok := s.auth.SessionFrom(r)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{
			"authenticated": false,
			"auth_enabled":  true,
		})
		return
	}
	body := map[string]any{
		"authenticated": true,
		"auth_enabled":  true,
		"version":       s.version,
		"subject":       sess.Subject,
		"email":         sess.Email,
		"name":          sess.Name,
		"role":          string(sess.Role),
	}
	// Only for an admin, and only because the grants page would otherwise be
	// lying by omission: adding a grant changes nothing at all while every
	// authenticated user can already read everything. It is a configuration
	// detail, so it is not told to anyone who cannot act on it.
	if sess.Role == auth.RoleAdmin {
		body["default_role"] = string(s.defaultRole)
		if s.externalURL != "" {
			body["external_url"] = s.externalURL
		}
	}
	writeJSON(w, http.StatusOK, body)
}
