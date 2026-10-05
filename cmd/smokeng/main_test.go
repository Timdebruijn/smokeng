package main

import (
	"bytes"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A secret read from a file is only as private as the file. Deploying it 0600
// and then having nothing notice when it is not would be worse than leaving it
// on the command line, because the command line at least looks obviously
// public. So a loose mode is reported, and a tight one is silent.
func TestSecretFileReportsALooseMode(t *testing.T) {
	for _, tc := range []struct {
		mode     os.FileMode
		wantWarn bool
	}{
		{0o600, false},
		{0o640, true}, // the group can read it
		{0o604, true}, // and so can everyone
	} {
		path := filepath.Join(t.TempDir(), "secret")
		if err := os.WriteFile(path, []byte("hunter2\n"), tc.mode); err != nil {
			t.Fatal(err)
		}
		// WriteFile is subject to the umask, so set the mode we mean.
		if err := os.Chmod(path, tc.mode); err != nil {
			t.Fatal(err)
		}

		var logged bytes.Buffer
		old := log.Writer()
		log.SetOutput(&logged)
		b, err := readSecretFile(path)
		log.SetOutput(old)

		if err != nil {
			t.Fatalf("mode %#o: %v", tc.mode, err)
		}
		if got := strings.TrimSpace(string(b)); got != "hunter2" {
			t.Errorf("mode %#o: read %q", tc.mode, got)
		}
		if warned := strings.Contains(logged.String(), "chmod 600"); warned != tc.wantWarn {
			t.Errorf("mode %#o: warned = %v, want %v (log: %q)",
				tc.mode, warned, tc.wantWarn, logged.String())
		}
	}
}

// A proxy terminating TLS on the same host leaves smokeng listening on
// loopback while the browser is on https. Deciding from the listen address
// alone dropped Secure from the session cookie there, which puts it one
// downgrade away from travelling in the clear.
func TestCookieSecurityFollowsTheAddressTheBrowserUsed(t *testing.T) {
	cases := []struct {
		name        string
		externalURL string
		listen      string
		wantInsecur bool
	}{
		{"proxy on the same host, TLS outside", "https://smokeng.example.org", "127.0.0.1:8080", false},
		{"no proxy, bound publicly", "", "0.0.0.0:8080", false},
		{"local development", "", "127.0.0.1:8080", true},
		{"external address is plain HTTP", "http://smokeng.lan:8080", "127.0.0.1:8080", true},
	}
	for _, tc := range cases {
		if got := cookieInsecure(tc.externalURL, tc.listen); got != tc.wantInsecur {
			t.Errorf("%s: cookieInsecure = %v, want %v", tc.name, got, tc.wantInsecur)
		}
	}
}

func TestTheServerHasTimeouts(t *testing.T) {
	s := newHTTPServer("127.0.0.1:0", http.NotFoundHandler())
	for name, d := range map[string]time.Duration{
		"ReadHeaderTimeout": s.ReadHeaderTimeout, "ReadTimeout": s.ReadTimeout,
		"WriteTimeout": s.WriteTimeout, "IdleTimeout": s.IdleTimeout,
	} {
		if d <= 0 {
			t.Errorf("%s is not set", name)
		}
	}
	// A header has to arrive well before the body does.
	if s.ReadHeaderTimeout >= s.ReadTimeout {
		t.Errorf("headers get %v, the whole request %v", s.ReadHeaderTimeout, s.ReadTimeout)
	}
}

func TestHostsAreRestrictedOnlyWithoutAuthOnLoopback(t *testing.T) {
	for _, c := range []struct {
		auth   bool
		listen string
		want   bool
	}{
		{false, "127.0.0.1:8080", true},
		{false, "[::1]:8080", true},
		{false, "localhost:8080", true},
		{true, "127.0.0.1:8080", false},
		{false, "0.0.0.0:8080", false},
		{false, ":8080", false},
		{true, "0.0.0.0:8080", false},
	} {
		if got := hostsRestricted(c.auth, c.listen); got != c.want {
			t.Errorf("hostsRestricted(%v, %q) = %v, want %v", c.auth, c.listen, got, c.want)
		}
	}
}

// Whoever the identity provider lets through becomes an administrator of the
// tree, its agents and its grants if no admin group is named. That is a policy
// of the provider's reach (an application assigned to everyone, a social login
// someone enabled later), so it has to be asked for, not arrived at by leaving a
// flag out.
func TestAdminPolicyMustBeChosenWhenOIDCIsOn(t *testing.T) {
	for _, c := range []struct {
		name           string
		issuer, admin  string
		everyone, fail bool
	}{
		{"no oidc", "", "", false, false},
		{"admin group named", "https://id.example.org", "smokeng-admins", false, false},
		{"nothing chosen", "https://id.example.org", "", false, true},
		{"everyone, asked for", "https://id.example.org", "", true, false},
		{"both, which is a contradiction", "https://id.example.org", "smokeng-admins", true, true},
		{"everyone without oidc means nothing", "", "", true, true},
	} {
		err := checkAdminPolicy(c.issuer, c.admin, c.everyone)
		if (err != nil) != c.fail {
			t.Errorf("%s: err = %v, want failure = %v", c.name, err, c.fail)
		}
	}
}

// The URL of a webhook is often what authorises a post to it, and an argument
// is readable by every local user in the process list.
func TestWebhookURLCanComeFromAFile(t *testing.T) {
	file := func(content string, mode os.FileMode) string {
		p := filepath.Join(t.TempDir(), "webhook")
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	good := "https://hooks.example.org/services/T0/B0/xyz"
	for _, c := range []struct {
		name, flag, file string
		want             string
		fail             bool
	}{
		{"neither", "", "", "", false},
		{"flag", good, "", good, false},
		{"file, with the newline an editor adds", "", file(good+"\n", 0o600), good, false},
		{"both", good, file(good, 0o600), "", true},
		{"empty file", "", file("\n", 0o600), "", true},
		{"missing file", "", filepath.Join(t.TempDir(), "absent"), "", true},
		{"not a URL", "alertmanager:9093", "", "", true},
		{"wrong scheme", "ftp://example.org/x", "", "", true},
		{"no host", "https:///path", "", "", true},
	} {
		got, err := loadWebhookURL(c.flag, c.file)
		if (err != nil) != c.fail || got != c.want {
			t.Errorf("%s: got %q, %v; want %q, failure %v", c.name, got, err, c.want, c.fail)
		}
		if err != nil && strings.Contains(err.Error(), "xyz") {
			t.Errorf("%s: the error repeats the URL: %v", c.name, err)
		}
	}
}
