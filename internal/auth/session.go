// Package auth provides OIDC login and the two roles smokeng recognises
// (DESIGN.md §7.1). There is no local account store and no password handling:
// identity is somebody else's problem, solved better elsewhere.
package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Role is what a session is permitted to do. Viewer reads and admin writes,
// both globally; editor sits between them and exists only inside a grant,
// where it means "write, but only in this subtree" (DESIGN.md §7.4).
type Role string

const (
	// RoleNone is no access at all. It is spelled rather than left as the
	// zero value because an operator has to be able to write it down.
	RoleNone   Role = "none"
	RoleViewer Role = "viewer"
	RoleEditor Role = "editor"
	RoleAdmin  Role = "admin"
)

// rank orders the ladder so two claims on the same node can be compared.
func (r Role) rank() int {
	switch r {
	case RoleAdmin:
		return 3
	case RoleEditor:
		return 2
	case RoleViewer:
		return 1
	}
	// RoleNone and the zero value both mean no access.
	return 0
}

// AtLeast reports whether r permits everything want permits.
func (r Role) AtLeast(want Role) bool { return r.rank() >= want.rank() }

// Max returns the more permissive of two roles, for a user who holds a grant
// on a node and another on one of its ancestors.
func Max(a, b Role) Role {
	if a.rank() >= b.rank() {
		return a
	}
	return b
}

// Session is the authenticated identity carried in a cookie.
type Session struct {
	Subject string `json:"sub"`
	Email   string `json:"email,omitempty"`
	Name    string `json:"name,omitempty"`
	Role    Role   `json:"role"`
	// Groups are the provider's group claim, verbatim. Grants are keyed on
	// them, so they have to travel with the session; there is no user
	// directory here to look them up in later.
	Groups  []string `json:"groups,omitempty"`
	Expires int64    `json:"exp"`
}

// Sessions are held in a signed cookie rather than server-side, so that a
// restart does not log everybody out and there is no session table to expire.
// The key is persisted, so signatures survive restarts too; rotating it
// invalidates every session, which is the intended way to do that.
type signer struct {
	key []byte
}

var errBadSession = errors.New("auth: invalid session")

// encode renders a session as payload.signature, both base64url.
func (s *signer) encode(sess Session) (string, error) {
	body, err := json.Marshal(sess)
	if err != nil {
		return "", err
	}
	payload := base64.RawURLEncoding.EncodeToString(body)
	return payload + "." + s.sign(payload), nil
}

func (s *signer) decode(token string) (Session, error) {
	payload, sig, found := strings.Cut(token, ".")
	if !found {
		return Session{}, errBadSession
	}
	// Constant-time comparison: this check is what stands between a viewer
	// and forging an admin cookie.
	if !hmac.Equal([]byte(sig), []byte(s.sign(payload))) {
		return Session{}, errBadSession
	}
	body, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return Session{}, errBadSession
	}
	var sess Session
	if err := json.Unmarshal(body, &sess); err != nil {
		return Session{}, errBadSession
	}
	if time.Now().Unix() > sess.Expires {
		return Session{}, fmt.Errorf("auth: session expired")
	}
	if sess.Role != RoleViewer && sess.Role != RoleAdmin {
		return Session{}, errBadSession
	}
	return sess, nil
}

func (s *signer) sign(payload string) string {
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
