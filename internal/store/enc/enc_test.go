package enc

import (
	"bytes"
	"slices"
	"testing"
)

func TestGolden(t *testing.T) {
	// 20000 µs, +150, +0 (duplicate RTT), +850.
	samples := []uint32{20000, 20150, 20150, 21000}
	want := []byte{0x01, 0xA0, 0x9C, 0x01, 0x96, 0x01, 0x00, 0xD2, 0x06}
	got, err := Encode(samples)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("Encode = %#v, want %#v", got, want)
	}
}

func TestRoundTrip(t *testing.T) {
	cases := [][]uint32{
		nil,   // 100% loss interval
		{0},   // sub-µs RTT rounds to 0
		{137}, // LAN-scale
		{20000, 20150, 20150, 21000},
		{1, 4294967295}, // full uint32 range
	}
	for _, samples := range cases {
		blob, err := Encode(samples)
		if err != nil {
			t.Fatalf("Encode(%v): %v", samples, err)
		}
		got, err := Decode(blob)
		if err != nil {
			t.Fatalf("Decode(Encode(%v)): %v", samples, err)
		}
		if !slices.Equal(got, samples) {
			t.Fatalf("round trip %v -> %v", samples, got)
		}
	}
}

func TestEncodeUnsorted(t *testing.T) {
	if _, err := Encode([]uint32{5, 3}); err != ErrUnsorted {
		t.Fatalf("err = %v, want ErrUnsorted", err)
	}
}

func TestDecodeErrors(t *testing.T) {
	for name, blob := range map[string][]byte{
		"empty":       {},
		"bad version": {0x02, 0x01},
		"truncated":   {0x01, 0x80},                         // continuation bit set, nothing follows
		"overflow":    {0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0x7F}, // > MaxUint32
	} {
		if _, err := Decode(blob); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}
