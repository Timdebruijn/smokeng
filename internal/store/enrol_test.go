package store

import (
	"context"
	"crypto/ed25519"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openStore(t *testing.T) *SQLite {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "enrol.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func testKey(t *testing.T) ed25519.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return pub
}

func TestEnrolmentTokenRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	now := time.Unix(1_800_000_000, 0)

	tok, err := s.MintEnrolmentToken(ctx, "ams-01", time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tok.Plaintext, TokenPrefix) {
		t.Errorf("token %q does not carry the recognisable prefix", tok.Plaintext)
	}

	pub := testKey(t)
	agent, err := s.RedeemEnrolmentToken(ctx, tok.Plaintext, pub, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("redeem: %v", err)
	}
	if agent.Name != "ams-01" {
		t.Errorf("agent name = %q, want the name the token carried", agent.Name)
	}
	if !agent.Enabled {
		t.Error("a freshly enrolled agent is disabled")
	}

	agents, err := s.ListAgents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, a := range agents {
		if a.ID == agent.ID && a.Name == "ams-01" && a.PubKey.Equal(pub) {
			found = true
		}
	}
	if !found {
		t.Error("the enrolled agent is not in the agent list with its key")
	}
}

// The plaintext must not be recoverable from the database. Anyone who can read
// the file must not come away with a usable credential.
func TestEnrolmentTokenIsStoredHashed(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	tok, err := s.MintEnrolmentToken(ctx, "ams-01", time.Hour, time.Unix(1_800_000_000, 0))
	if err != nil {
		t.Fatal(err)
	}
	var stored []byte
	if err := s.db.QueryRowContext(ctx,
		"SELECT token_hash FROM enrolment_tokens WHERE id = ?", tok.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stored), tok.Plaintext) {
		t.Fatal("the token plaintext is in the database")
	}
	if len(stored) != 32 {
		t.Fatalf("stored token is %d bytes, want a 32-byte sha256", len(stored))
	}

	listed, err := s.ListEnrolmentTokens(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range listed {
		if l.Plaintext != "" {
			t.Error("listing tokens handed back a plaintext")
		}
	}
}

// Single use is the whole point: a token that keeps working is the credential
// that ends up in a wiki.
func TestEnrolmentTokenIsSingleUse(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	now := time.Unix(1_800_000_000, 0)
	tok, err := s.MintEnrolmentToken(ctx, "ams-01", time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RedeemEnrolmentToken(ctx, tok.Plaintext, testKey(t), now); err != nil {
		t.Fatal(err)
	}
	_, err = s.RedeemEnrolmentToken(ctx, tok.Plaintext, testKey(t), now)
	if !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("second redemption returned %v, want ErrTokenInvalid", err)
	}
}

func TestEnrolmentTokenExpires(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	now := time.Unix(1_800_000_000, 0)
	tok, err := s.MintEnrolmentToken(ctx, "ams-01", time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.RedeemEnrolmentToken(ctx, tok.Plaintext, testKey(t), now.Add(2*time.Hour))
	if !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("an expired token returned %v, want ErrTokenInvalid", err)
	}
}

func TestEnrolmentTokenRejectsUnknownToken(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	_, err := s.RedeemEnrolmentToken(ctx, TokenPrefix+"nonsense", testKey(t), time.Unix(1_800_000_000, 0))
	if !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("an unknown token returned %v, want ErrTokenInvalid", err)
	}
}

// A name collision is the operator's mistake, and must not cost them the
// token: they should be able to retry under another name.
func TestNameCollisionDoesNotSpendTheToken(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	now := time.Unix(1_800_000_000, 0)

	tok, err := s.MintEnrolmentToken(ctx, "ams-01", time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	// Someone enrols that name by hand in the meantime.
	if _, err := s.AddAgent(ctx, "ams-01", testKey(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RedeemEnrolmentToken(ctx, tok.Plaintext, testKey(t), now); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("redeeming into a taken name returned %v, want ErrNameTaken", err)
	}

	listed, err := s.ListEnrolmentTokens(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range listed {
		if l.ID == tok.ID && l.UsedAt != 0 {
			t.Fatal("a rejected redemption spent the token anyway")
		}
	}
}

func TestMintRejectsReservedAndTakenNames(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	now := time.Unix(1_800_000_000, 0)

	if _, err := s.MintEnrolmentToken(ctx, "local", time.Hour, now); err == nil {
		t.Error("minted a token for the reserved name 'local'")
	}
	if _, err := s.MintEnrolmentToken(ctx, "  ", time.Hour, now); err == nil {
		t.Error("minted a token for a blank name")
	}
	if _, err := s.AddAgent(ctx, "ams-01", testKey(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MintEnrolmentToken(ctx, "ams-01", time.Hour, now); !errors.Is(err, ErrNameTaken) {
		t.Error("minting for an existing name should fail while the operator is still looking at the form")
	}
}

func TestRevokeOnlyUnspentTokens(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	now := time.Unix(1_800_000_000, 0)

	unspent, err := s.MintEnrolmentToken(ctx, "rtd-01", time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeEnrolmentToken(ctx, unspent.ID); err != nil {
		t.Fatalf("revoking an unspent token: %v", err)
	}
	if _, err := s.RedeemEnrolmentToken(ctx, unspent.Plaintext, testKey(t), now); !errors.Is(err, ErrTokenInvalid) {
		t.Error("a revoked token still enrols")
	}

	spent, err := s.MintEnrolmentToken(ctx, "ams-01", time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RedeemEnrolmentToken(ctx, spent.Plaintext, testKey(t), now); err != nil {
		t.Fatal(err)
	}
	// A spent token is the record of how an agent came to exist; keep it.
	if err := s.RevokeEnrolmentToken(ctx, spent.ID); err == nil {
		t.Error("revoking a spent token deleted the record of an enrolment")
	}
}

func TestRenameAgent(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	a, err := s.AddAgent(ctx, "ams-01", testKey(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddAgent(ctx, "rtd-01", testKey(t)); err != nil {
		t.Fatal(err)
	}
	if err := s.RenameAgent(ctx, a.ID, "ams-02"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if err := s.RenameAgent(ctx, a.ID, "rtd-01"); !errors.Is(err, ErrNameTaken) {
		t.Error("renaming onto another agent's name should be refused")
	}
	if err := s.RenameAgent(ctx, a.ID, "local"); err == nil {
		t.Error("renaming to the reserved name 'local' should be refused")
	}
}
