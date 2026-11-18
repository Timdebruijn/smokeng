package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"testing"
)

func maskOf(n int, lostAt ...int) []bool {
	l := make([]bool, n)
	for _, i := range lostAt {
		l[i] = true
	}
	return l
}

// The encoding is a wire and storage format: what goes in has to come out, at
// the sizes where the last byte is full, nearly full, and just spilled over.
func TestLossMaskRoundTrip(t *testing.T) {
	for _, n := range []int{1, 7, 8, 9, 20, 1000} {
		lost := maskOf(n, 0, n-1, n/2)
		for _, spread := range []bool{false, true} {
			mask := EncodeLossMask(spread, lost)
			got := 0
			for _, l := range lost {
				if l {
					got++
				}
			}
			sp, back, err := DecodeLossMask(mask, n, n-got)
			if err != nil {
				t.Fatalf("n=%d spread=%v: %v", n, spread, err)
			}
			if sp != spread || !reflect.DeepEqual(back, lost) {
				t.Errorf("n=%d spread=%v: round trip changed the mask", n, spread)
			}
		}
	}
}

// A decoder that trusts a mask from another machine would store garbage that a
// later detector reads as fact; each way a mask can be wrong has to be refused.
func TestDecodeLossMaskRefuses(t *testing.T) {
	good := EncodeLossMask(false, maskOf(20, 15, 16, 17, 18, 19)) // 5 lost of 20
	with := func(f func(b []byte) []byte) []byte { return f(append([]byte(nil), good...)) }
	cases := map[string]struct {
		mask           []byte
		sent, received int
	}{
		"too short":          {good[:len(good)-1], 20, 15},
		"too long":           {append(append([]byte(nil), good...), 0), 20, 15},
		"empty":              {nil, 20, 15},
		"reserved bit":       {with(func(b []byte) []byte { b[0] |= 2; return b }), 20, 15},
		"padding bit":        {with(func(b []byte) []byte { b[3] |= 1 << 7; return b }), 20, 14},
		"popcount too low":   {good, 20, 14},
		"popcount too high":  {good, 20, 16},
		"received over sent": {good, 20, 21},
	}
	for name, c := range cases {
		if _, _, err := DecodeLossMask(c.mask, c.sent, c.received); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// A mask is the one thing that distinguishes "lost the tail" from "lost the
// head", so it has to survive the store; and a row without one has to stay
// without one rather than read back as an empty mask.
func TestLossMaskStoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	mask := EncodeLossMask(false, maskOf(10, 8, 9))
	if err := s.WriteMeasurements(ctx, []Measurement{
		{TargetID: 1, AgentID: 2, TS: 1000, Sent: 10, Received: 8, Samples: []uint32{1, 2, 3, 4, 5, 6, 7, 8}, LossMask: mask},
		{TargetID: 1, AgentID: 2, TS: 1060, Sent: 10, Received: 10, Samples: []uint32{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.QueryRange(ctx, 1, 2, 0, 0, 2000)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !reflect.DeepEqual(got[0].LossMask, mask) || got[1].LossMask != nil {
		t.Errorf("masks = %v, %v; want %v, nil", got[0].LossMask, got[1].LossMask, mask)
	}
}

// An agent's buffer is the same store: the mask has to leave the outbox with the
// row, or the master would never see one from a remote vantage point.
func TestPendingMeasurementsCarryLossMask(t *testing.T) {
	s := openStore(t)
	mask := EncodeLossMask(true, maskOf(4, 3))
	writeOne(t, s, Measurement{TargetID: 1, AgentID: 7, TS: 1000, Sent: 4, Received: 3,
		Samples: []uint32{1, 2, 3}, LossMask: mask})
	pend, err := s.PendingMeasurements(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pend) != 1 || !reflect.DeepEqual(pend[0].LossMask, mask) {
		t.Errorf("outbox = %+v, want the mask %v", pend, mask)
	}
}

// v25 adds a column to a table that already holds history. Those rows must open
// unchanged and read back with no mask: nothing may invent one.
func TestV25LeavesExistingRowsWithoutMask(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 24 {
		if _, err := db.Exec(migrations[i]); err != nil {
			t.Fatalf("v%d: %v", i+1, err)
		}
	}
	for _, q := range []string{
		`INSERT INTO measurements (target_id, agent_id, ts, sent, received, flags, samples, icmp_error, send_error)
		   VALUES (5, 0, 1000, 5, 1, 0, X'01A09C01', 259, 2)`,
		`PRAGMA user_version = 24`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("migrating a v24 database: %v", err)
	}
	defer s.Close()
	got, err := s.QueryRange(context.Background(), 5, LocalAgentID, 0, 0, 1<<40)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].LossMask != nil || got[0].Sent != 5 || got[0].Received != 1 ||
		got[0].ICMPErr == nil || *got[0].ICMPErr != 259 || got[0].SendErr == nil || *got[0].SendErr != 2 {
		t.Errorf("row after v25 = %+v", got)
	}
}
