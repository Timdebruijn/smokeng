package store

import "testing"

// AlertInput is the one place a stored measurement becomes an alert input, so
// PositionalLoss must be set exactly when the master flagged the interval.
func TestAlertInputCarriesPositionalLoss(t *testing.T) {
	m := Measurement{Sent: 20, Received: 17}
	if in := m.AlertInput(); in.PositionalLoss {
		t.Error("an unflagged interval reads as positional")
	}
	m.Positional = &PositionalLoss{}
	if in := m.AlertInput(); !in.PositionalLoss {
		t.Error("a flagged interval does not read as positional")
	}
}
