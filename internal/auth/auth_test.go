package auth

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func testSigner() *signer { return &signer{key: []byte("a-32-byte-test-key-for-hmac-abcd")} }

func TestSessionRoundTrip(t *testing.T) {
	s := testSigner()
	want := Session{
		Subject: "user-1", Email: "tim@example.org", Name: "Tim",
		Role: RoleAdmin, Groups: []string{"ops", "gemeente-x"},
		Expires: time.Now().Add(time.Hour).Unix(),
	}
	token, err := s.encode(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.decode(token)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("decode = %+v, want %+v", got, want)
	}
}

// The signature is the only thing standing between a viewer and an admin
// cookie, so every way of tampering with one must be rejected.
func TestForgedSessionsAreRejected(t *testing.T) {
	s := testSigner()
	valid, err := s.encode(Session{Subject: "u", Role: RoleViewer,
		Expires: time.Now().Add(time.Hour).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	payload, sig, _ := strings.Cut(valid, ".")

	// A payload promoted to admin, keeping the old signature.
	promoted, err := s.encode(Session{Subject: "u", Role: RoleAdmin,
		Expires: time.Now().Add(time.Hour).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	promotedPayload, _, _ := strings.Cut(promoted, ".")

	cases := map[string]string{
		"no signature":        payload,
		"empty signature":     payload + ".",
		"wrong signature":     payload + "." + strings.Repeat("A", len(sig)),
		"swapped payload":     promotedPayload + "." + sig,
		"garbage":             "not-a-token",
		"signed by other key": mustEncode(t, &signer{key: []byte("a-different-32-byte-key-nope-abc")}),
	}
	for name, token := range cases {
		if _, err := s.decode(token); err == nil {
			t.Errorf("%s: accepted a forged session", name)
		}
	}

	// An expired session is rejected even though its signature is valid.
	expired, err := s.encode(Session{Subject: "u", Role: RoleAdmin,
		Expires: time.Now().Add(-time.Minute).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.decode(expired); err == nil {
		t.Error("accepted an expired session")
	}

	// A role the code does not know is not a role.
	bogus, err := s.encode(Session{Subject: "u", Role: Role("superuser"),
		Expires: time.Now().Add(time.Hour).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.decode(bogus); err == nil {
		t.Error("accepted an unknown role")
	}
}

func mustEncode(t *testing.T, s *signer) string {
	t.Helper()
	token, err := s.encode(Session{Subject: "u", Role: RoleAdmin,
		Expires: time.Now().Add(time.Hour).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func TestRoleFromClaims(t *testing.T) {
	cases := map[string]struct {
		claims     map[string]any
		claimName  string
		adminValue string
		want       Role
	}{
		"member of the admin group": {
			map[string]any{"groups": []any{"staff", "smokeng-admins"}}, "groups", "smokeng-admins", RoleAdmin,
		},
		"not a member": {
			map[string]any{"groups": []any{"staff"}}, "groups", "smokeng-admins", RoleViewer,
		},
		"space-separated string": {
			map[string]any{"groups": "staff smokeng-admins"}, "groups", "smokeng-admins", RoleAdmin,
		},
		"comma-separated string": {
			map[string]any{"roles": "a,smokeng-admins,b"}, "roles", "smokeng-admins", RoleAdmin,
		},
		"claim missing entirely": {
			map[string]any{}, "groups", "smokeng-admins", RoleViewer,
		},
		"claim is not a list of strings": {
			map[string]any{"groups": 42}, "groups", "smokeng-admins", RoleViewer,
		},
		// Substrings must not match: "smokeng-admins-readonly" is not it.
		"near miss": {
			map[string]any{"groups": []any{"smokeng-admins-readonly"}}, "groups", "smokeng-admins", RoleViewer,
		},
		// Configuring no admin group is documented as "everyone is an admin".
		"no admin group configured": {
			map[string]any{"groups": []any{"staff"}}, "groups", "", RoleAdmin,
		},
	}
	for name, c := range cases {
		if got := roleFromClaims(c.claims, c.claimName, c.adminValue); got != c.want {
			t.Errorf("%s: role = %q, want %q", name, got, c.want)
		}
	}
}
