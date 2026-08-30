package agent

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
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
	"strings"
	"testing"
	"time"

	"github.com/timdebruijn/smokeng/internal/probe"
	"github.com/timdebruijn/smokeng/internal/store"
)

// The CAs a master hands down reach https probes and nothing else. In
// particular they must not let the master vouch for its own certificate:
// "who this agent measures" and "who this agent believes" are separate
// questions, and collapsing them would let one compromised master bootstrap
// trust in itself.
//
// The check is behavioural rather than a reading of the code. A master is
// served under a certificate that is trusted *only* through the probe pool;
// if the agent could reach it, the separation would have been lost.
func TestProbeCAsDoNotVouchForTheMaster(t *testing.T) {
	certPEM, tlsCert := selfSigned(t)

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{tlsCert}}
	srv.StartTLS()
	defer srv.Close()

	// Trust it for probing, and only for probing.
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caFile, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := probe.TrustCAFiles([]string{caFile}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = probe.TrustCAFiles(nil) })

	st, err := store.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	a, err := New(Config{Master: srv.URL, AgentID: 1}, key, st)
	if err != nil {
		t.Fatal(err)
	}

	err = a.pull(context.Background())
	if err == nil {
		t.Fatal("the agent reached a master whose certificate is trusted only through the " +
			"probe CA pool; the pool has leaked into the agent's own connection, which " +
			"would let a master vouch for itself")
	}
	if !strings.Contains(err.Error(), "certificate") && !strings.Contains(err.Error(), "x509") {
		t.Fatalf("failed for the wrong reason: %v", err)
	}
}

func selfSigned(t *testing.T) ([]byte, tls.Certificate) {
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
		Subject:               pkix.Name{CommonName: "smokeng-master-test"},
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
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}
