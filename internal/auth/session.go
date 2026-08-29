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

// Role is what a session is permitted to do. Two roles, deliberately: viewer
// reads, admin also writes.
type Role string

const (
	RoleViewer Role = "viewer"
	RoleAdmin  Role = "admin"
)

// Session is the authenticated identity carried in a cookie.
type Session struct {
	Subject string `json:"sub"`
	Email   string `json:"email,omitempty"`
	Name    string `json:"name,omitempty"`
	Role    Role   `json:"role"`
	Expires int64  `json:"exp"`
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
