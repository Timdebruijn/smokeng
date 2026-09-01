package main

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
