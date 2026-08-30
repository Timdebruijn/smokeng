package probe

import (
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// httpsServer starts a TLS server with a self-signed certificate — the shape
// of every internal service behind a private PKI, which is the case all of
// this exists for.
func httpsServer(t *testing.T) (*httptest.Server, TargetSpec) {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	spec := userspaceSpec(t, "https", portOf(t, srv))
	return srv, spec
}

// trustOnly points the probe pool at this server's certificate and restores it
// afterwards. The pool is process-wide, so a test that left it set would
// silently change what every later test verifies against.
func trustOnly(t *testing.T, srv *httptest.Server) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ca.pem")
	der := srv.Certificate().Raw
	if err := os.WriteFile(path, pem.EncodeToMemory(
		&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	before := trustedCAs.Load()
	t.Cleanup(func() { trustedCAs.Store(before) })
	if err := TrustCAFiles([]string{path}); err != nil {
		t.Fatalf("TrustCAFiles: %v", err)
	}
}

// The default has to be that an unverifiable certificate is not quietly
// measured as though it were fine.
func TestHTTPSRefusesAnUntrustedCertificate(t *testing.T) {
	_, spec := httpsServer(t)
	got := runOne(t, spec)
	if got.received != 0 {
		t.Fatalf("got %+v, want nothing received: an unverifiable certificate must not "+
			"produce a measurement as though the far end were trusted", got)
	}
}

// Trusting the issuer is the good path: verification stays on, and the target
// measures. This is what --tls-ca-file buys.
func TestHTTPSMeasuresWhenTheCAIsTrusted(t *testing.T) {
	srv, spec := httpsServer(t)
	trustOnly(t, srv)
	got := runOne(t, spec)
	if got.sent != 1 || got.received != 1 {
		t.Fatalf("got %+v, want one sent and one received with the CA trusted", got)
	}
	if spec.TLSSkipVerify {
		t.Fatal("this must pass with verification on, or it proves nothing")
	}
}

// The escape hatch, for when adding the CA is not possible.
func TestHTTPSSkipVerifyMeasuresAnUntrustedCertificate(t *testing.T) {
	_, spec := httpsServer(t)
	spec.TLSSkipVerify = true
	got := runOne(t, spec)
	if got.sent != 1 || got.received != 1 {
		t.Fatalf("got %+v, want one sent and one received with verification off", got)
	}
}

// Several CA files accumulate rather than the last one winning — an operator
// with two customers' internal PKIs passes both and both must verify.
//
// This does not assert that the system roots survived, though they do: the
// pool comes from x509.SystemCertPool and is added to, never replaced. There
// is no sound way to check that here, because Subjects() is documented not to
// report system roots for a pool obtained that way, and reaching a public
// endpoint to prove it would make this test depend on the network it is
// supposed to be measuring.
func TestTrustCAFilesAccumulate(t *testing.T) {
	a, specA := httpsServer(t)
	b, specB := httpsServer(t)

	dir := t.TempDir()
	var paths []string
	for i, srv := range []*httptest.Server{a, b} {
		p := filepath.Join(dir, string(rune('a'+i))+".pem")
		if err := os.WriteFile(p, pem.EncodeToMemory(
			&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}
	before := trustedCAs.Load()
	t.Cleanup(func() { trustedCAs.Store(before) })
	if err := TrustCAFiles(paths); err != nil {
		t.Fatal(err)
	}

	for name, spec := range map[string]TargetSpec{"first": specA, "second": specB} {
		if got := runOne(t, spec); got.received != 1 {
			t.Fatalf("%s CA: got %+v, want one received — passing two CA files must trust both",
				name, got)
		}
	}
}

// A file the operator meant to be trusted that yields no certificate is an
// error, not a silent no-op that surfaces later as unexplained loss.
func TestTrustCAFilesRejectsAFileWithNoCertificates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.pem")
	if err := os.WriteFile(path, []byte("not a certificate\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := trustedCAs.Load()
	t.Cleanup(func() { trustedCAs.Store(before) })
	if err := TrustCAFiles([]string{path}); err == nil {
		t.Fatal("a file with no PEM certificates was accepted")
	}
	if err := TrustCAFiles([]string{filepath.Join(t.TempDir(), "absent.pem")}); err == nil {
		t.Fatal("a missing CA file was accepted")
	}
}
