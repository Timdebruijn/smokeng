package probe

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// httpsServer starts a TLS server with a freshly minted self-signed
// certificate — the shape of every internal service behind a private PKI,
// which is the case all of this exists for.
//
// The certificate is generated here rather than taken from
// httptest.NewTLSServer, which hands every server it makes the same built-in
// one. Two servers sharing a certificate cannot distinguish "this CA is
// trusted" from "some CA is trusted", so a test written against it would pass
// whether or not the pool did anything.
func httpsServer(t *testing.T) (*httptest.Server, TargetSpec) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "smokeng-test-" + serial.String()[:8]},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{{
		Certificate: [][]byte{der},
		PrivateKey:  key,
	}}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv, userspaceSpec(t, "https", portOf(t, srv))
}

// trustOnly points the probe pool at this server's certificate and restores it
// afterwards. The pool is process-wide, so a test that left it set would
// silently change what every later test verifies against.
func trustOnly(t *testing.T, srv *httptest.Server) {
	t.Helper()
	t.Cleanup(resetCAs)
	if err := TrustCAFiles([]string{pemFile(t, srv)}); err != nil {
		t.Fatalf("TrustCAFiles: %v", err)
	}
}

// resetCAs clears both sources. The pool is process-wide, so a test that left
// it set would silently change what every later test verifies against.
func resetCAs() {
	caMu.Lock()
	defer caMu.Unlock()
	localCAs, remoteCAs = nil, nil
	_ = rebuildLocked()
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

	t.Cleanup(resetCAs)
	if err := TrustCAFiles([]string{pemFile(t, a), pemFile(t, b)}); err != nil {
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
	t.Cleanup(resetCAs)
	if err := TrustCAFiles([]string{path}); err == nil {
		t.Fatal("a file with no PEM certificates was accepted")
	}
	if err := TrustCAFiles([]string{filepath.Join(t.TempDir(), "absent.pem")}); err == nil {
		t.Fatal("a missing CA file was accepted")
	}
}

// The master's CAs replace what the master supplied before, and never touch
// what this host was given locally. Without replacement, withdrawing a CA on
// the master could not reach the agents — the pool would keep trusting a root
// long after it was retired.
func TestRemoteCAsReplaceButLocalOnesSurvive(t *testing.T) {
	quietLog(t)
	local, localSpec := httpsServer(t)
	remote, remoteSpec := httpsServer(t)

	t.Cleanup(resetCAs)
	resetCAs()

	if err := TrustCAFiles([]string{pemFile(t, local)}); err != nil {
		t.Fatal(err)
	}
	if err := TrustRemoteCAPEMs([][]byte{pemBytes(local), pemBytes(remote)}); err != nil {
		t.Fatal(err)
	}
	if got := runOne(t, remoteSpec); got.received != 1 {
		t.Fatalf("a CA from the master was not trusted: %+v", got)
	}

	// The master withdraws everything. Its own CA goes; the local one stays.
	if err := TrustRemoteCAPEMs(nil); err != nil {
		t.Fatal(err)
	}
	if got := runOne(t, remoteSpec); got.received != 0 {
		t.Fatalf("a CA the master withdrew is still trusted: %+v", got)
	}
	if got := runOne(t, localSpec); got.received != 1 {
		t.Fatalf("the master's withdrawal revoked a locally-configured CA: %+v", got)
	}
}

// LocalCAPEMs is what a master hands down. It must never include what the
// master itself was handed, or one compromised master would propagate its
// trust decisions through every master that relayed for it.
func TestLocalCAPEMsExcludesRemoteOnes(t *testing.T) {
	quietLog(t)
	local, _ := httpsServer(t)
	remote, _ := httpsServer(t)
	t.Cleanup(resetCAs)

	if err := TrustCAFiles([]string{pemFile(t, local)}); err != nil {
		t.Fatal(err)
	}
	if err := TrustRemoteCAPEMs([][]byte{pemBytes(remote)}); err != nil {
		t.Fatal(err)
	}
	got := LocalCAPEMs()
	if len(got) != 1 || !bytes.Equal(got[0], pemBytes(local)) {
		t.Fatalf("LocalCAPEMs returned %d block(s); it must hand on only what this host "+
			"was configured with", len(got))
	}
}

func pemBytes(srv *httptest.Server) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
}

func pemFile(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(p, pemBytes(srv), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}
