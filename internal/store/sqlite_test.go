package store

import (
	"context"
	"path/filepath"
	"slices"
	"testing"

	"smokeng/internal/tree"
)

func ptr[T any](v T) *T { return &v }

func TestSQLiteRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// A fresh database seeds a complete root and passes tree validation.
	targets, err := s.ListTargets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 || targets[0].ID != 1 || targets[0].ParentID != nil {
		t.Fatalf("fresh db targets = %+v, want only the seeded root", targets)
	}
	if _, err := tree.New(targets); err != nil {
		t.Fatalf("seeded root fails tree validation: %v", err)
	}

	child := tree.Target{
		ParentID:      ptr(int64(1)),
		Name:          "cloudflare-v4",
		Host:          ptr("1.1.1.1"),
		AddressFamily: ptr("v4"),
		Enabled:       true,
		Settings:      tree.Settings{PingsPerInterval: ptr(40)},
	}
	if err := s.UpsertTarget(ctx, &child); err != nil {
		t.Fatal(err)
	}
	if child.ID == 0 {
		t.Fatal("UpsertTarget did not assign an id")
	}

	m := Measurement{
		TargetID: child.ID,
		AgentID:  LocalAgentID,
		TS:       1_756_400_000,
		Sent:     20,
		Received: 3,
		Flags:    FlagUserspaceTX,
		Samples:  []uint32{20000, 20150, 21000},
	}
	if err := s.WriteMeasurements(ctx, []Measurement{m}); err != nil {
		t.Fatal(err)
	}
	// Idempotency: rewriting the same row must not duplicate or error.
	if err := s.WriteMeasurements(ctx, []Measurement{m}); err != nil {
		t.Fatal(err)
	}

	got, err := s.QueryRange(ctx, child.ID, LocalAgentID, m.TS, m.TS+1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("QueryRange returned %d rows, want 1", len(got))
	}
	if got[0].Sent != 20 || got[0].Received != 3 || got[0].Flags != FlagUserspaceTX ||
		!slices.Equal(got[0].Samples, m.Samples) {
		t.Fatalf("round trip = %+v, want %+v", got[0], m)
	}

	// The received == len(samples) invariant is enforced on write.
	bad := m
	bad.Received = 5
	if err := s.WriteMeasurements(ctx, []Measurement{bad}); err == nil {
		t.Fatal("expected error for received != len(samples)")
	}
}
