package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

const (
	sessionCookie = "smokeng_session"
	stateCookie   = "smokeng_oidc"
	sessionTTL    = 12 * time.Hour
)

// Config describes the identity provider and how its claims map onto roles.
type Config struct {
	Issuer       string
	ClientID     string
	ClientSecret string
	RedirectURL  string
	// AdminClaim is the ID-token claim listing a user's groups; AdminValue
	// the membership that grants the admin role. With AdminValue empty every
	// authenticated user is an admin, which Authenticator warns about rather
	// than leaving to be discovered.
	AdminClaim string
	AdminValue string
	// Insecure allows the session cookie over plain HTTP, for local
	// development only.
	Insecure bool
}

// Authenticator handles the OIDC login flow and turns its result into a
// signed session cookie.
type Authenticator struct {
	cfg      Config
	verifier *oidc.IDTokenVerifier
	oauth    *oauth2.Config
	signer   *signer
}

// New builds an Authenticator, discovering the provider's endpoints. The
// signing key is supplied by the caller so it can be persisted; a fresh key
// would log everyone out on every restart.
func New(ctx context.Context, cfg Config, signingKey []byte) (*Authenticator, error) {
	provider, err := oidc.NewProvider(ctx, cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("auth: discover %s: %w", cfg.Issuer, err)
	}
	if cfg.AdminClaim == "" {
		cfg.AdminClaim = "groups"
	}
	if cfg.AdminValue == "" {
		log.Printf("auth: no admin group configured, so every authenticated user is an admin; "+
			"set --oidc-admin-value to restrict it (claim %q)", cfg.AdminClaim)
	}
	return &Authenticator{
		cfg:      cfg,
		verifier: provider.Verifier(&oidc.Config{ClientID: cfg.ClientID}),
		oauth: &oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			RedirectURL:  cfg.RedirectURL,
			Endpoint:     provider.Endpoint(),
			Scopes:       []string{oidc.ScopeOpenID, "profile", "email", cfg.AdminClaim},
		},
		signer: &signer{key: signingKey},
	}, nil
}

// Routes registers the login endpoints on mux.
func (a *Authenticator) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /auth/login", a.handleLogin)
	mux.HandleFunc("GET /auth/callback", a.handleCallback)
	mux.HandleFunc("POST /auth/logout", a.handleLogout)
}

// handleLogin starts the flow, remembering state, nonce and PKCE verifier in
// a short-lived cookie so the callback can check what it gets back.
func (a *Authenticator) handleLogin(w http.ResponseWriter, r *http.Request) {
	state, nonce, verifier := randomToken(), randomToken(), oauth2.GenerateVerifier()
	pending, err := json.Marshal(map[string]string{
		"state": state, "nonce": nonce, "verifier": verifier,
	})
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, a.cookie(stateCookie, base64.RawURLEncoding.EncodeToString(pending), 10*time.Minute))
	http.Redirect(w, r, a.oauth.AuthCodeURL(state,
		oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier)), http.StatusFound)
}

func (a *Authenticator) handleCallback(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie(stateCookie)
	if err != nil {
		http.Error(w, "login expired, try again", http.StatusBadRequest)
		return
	}
	raw, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil {
		http.Error(w, "bad login state", http.StatusBadRequest)
		return
	}
	var pending map[string]string
	if err := json.Unmarshal(raw, &pending); err != nil {
		http.Error(w, "bad login state", http.StatusBadRequest)
		return
	}
	// Clear it either way: the state is single-use.
	http.SetCookie(w, a.cookie(stateCookie, "", -time.Hour))

	if r.URL.Query().Get("state") != pending["state"] {
		http.Error(w, "state mismatch", http.StatusBadRequest)
		return
	}
	token, err := a.oauth.Exchange(r.Context(), r.URL.Query().Get("code"),
		oauth2.VerifierOption(pending["verifier"]))
	if err != nil {
		log.Printf("auth: token exchange: %v", err)
		http.Error(w, "login failed", http.StatusUnauthorized)
		return
	}
	rawID, ok := token.Extra("id_token").(string)
	if !ok {
		http.Error(w, "provider returned no id_token", http.StatusUnauthorized)
		return
	}
	idToken, err := a.verifier.Verify(r.Context(), rawID)
	if err != nil {
		log.Printf("auth: verify id_token: %v", err)
		http.Error(w, "login failed", http.StatusUnauthorized)
		return
	}
	if idToken.Nonce != pending["nonce"] {
		http.Error(w, "nonce mismatch", http.StatusBadRequest)
		return
	}

	var claims struct {
		Email string `json:"email"`
		Name  string `json:"name"`
	}
	_ = idToken.Claims(&claims)

	sess := Session{
		Subject: idToken.Subject,
		Email:   claims.Email,
		Name:    claims.Name,
		Role:    a.roleFor(idToken),
		Expires: time.Now().Add(sessionTTL).Unix(),
	}
	value, err := a.signer.encode(sess)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, a.cookie(sessionCookie, value, sessionTTL))
	http.Redirect(w, r, "/", http.StatusFound)
}

func (a *Authenticator) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, a.cookie(sessionCookie, "", -time.Hour))
	w.WriteHeader(http.StatusNoContent)
}

// roleFor maps the provider's claims onto a role.
func (a *Authenticator) roleFor(idToken *oidc.IDToken) Role {
	var all map[string]any
	if err := idToken.Claims(&all); err != nil {
		return RoleViewer
	}
	return roleFromClaims(all, a.cfg.AdminClaim, a.cfg.AdminValue)
}

// roleFromClaims decides the role. Anything not recognised as an admin is a
// viewer: read access is the safe default, so a provider that renames a claim
// or stops sending it demotes people rather than promoting them.
func roleFromClaims(claims map[string]any, claimName, adminValue string) Role {
	if adminValue == "" {
		return RoleAdmin
	}
	switch v := claims[claimName].(type) {
	case string:
		// Some providers send a space- or comma-separated string.
		for _, item := range strings.FieldsFunc(v, func(r rune) bool { return r == ' ' || r == ',' }) {
			if item == adminValue {
				return RoleAdmin
			}
		}
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok && s == adminValue {
				return RoleAdmin
			}
		}
	}
	return RoleViewer
}

func (a *Authenticator) cookie(name, value string, ttl time.Duration) *http.Cookie {
	return &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   !a.cfg.Insecure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(ttl.Seconds()),
	}
}

// SessionFrom returns the caller's session, if the request carries a valid one.
func (a *Authenticator) SessionFrom(r *http.Request) (Session, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return Session{}, false
	}
	sess, err := a.signer.decode(c.Value)
	if err != nil {
		return Session{}, false
	}
	return sess, true
}

func randomToken() string {
	var b [24]byte
	rand.Read(b[:])
	return base64.RawURLEncoding.EncodeToString(b[:])
}
