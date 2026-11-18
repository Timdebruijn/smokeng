package ingest

import (
	"testing"

	"github.com/timdebruijn/smokeng/internal/store"
)

// Protects an agent's no-data interval (sent = 0, not sent, with a reason) from
// being refused or dropped at the door: a refused batch is discarded by the
// agent, so a prober that lost its route would lose every other row with it.
func TestDecodeBatchAcceptsANoDataRow(t *testing.T) {
	reason := store.SendReasonNoAddress
	in := []store.Measurement{
		{TargetID: 1, TS: 100, Sent: 1, Received: 1, Samples: []uint32{10}},
		{TargetID: 1, TS: 160, Flags: store.FlagSendFailed, SendErr: &reason},
	}
	out, err := DecodeBatch(mustEncode(t, in), 7)
	if err != nil || len(out) != 2 {
		t.Fatalf("rows = %d, err = %v, want both kept", len(out), err)
	}
	if m := out[1]; m.Sent != 0 || m.Received != 0 || m.Flags != store.FlagSendFailed ||
		m.SendErr == nil || *m.SendErr != reason {
		t.Errorf("row came back as %+v", m)
	}
}
