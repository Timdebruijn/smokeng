package enc

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
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

// A delta large enough to wrap the accumulator used to land back inside uint32
// as a small, plausible value: the blob decoded without error into samples that
// were wrong and no longer sorted. Checking the sum after the addition cannot
// see that; checking the delta before it can.
func TestDecodeRejectsDeltaThatWrapsTheAccumulator(t *testing.T) {
	var blob []byte
	blob = append(blob, 1) // version
	var buf [binary.MaxVarintLen64]byte
	// First sample near the top of the range, then a delta that wraps.
	blob = append(blob, buf[:binary.PutUvarint(buf[:], math.MaxUint32-1)]...)
	blob = append(blob, buf[:binary.PutUvarint(buf[:], math.MaxUint64-10)]...)

	got, err := Decode(blob)
	if err == nil {
		t.Fatalf("decoded a wrapped delta into %v instead of refusing it", got)
	}
}

func TestSignedRoundTrip(t *testing.T) {
	cases := [][]int32{
		{},
		{0},
		{-5},
		{-1200, -300, -1, 0, 1, 4, 900},
		{math.MinInt32, 0, math.MaxInt32},
		{-7, -7, -7},
	}
	for _, want := range cases {
		blob, err := EncodeSigned(want)
		if err != nil {
			t.Fatalf("EncodeSigned(%v): %v", want, err)
		}
		got, err := DecodeSigned(blob)
		if err != nil {
			t.Fatalf("DecodeSigned(%v): %v", want, err)
		}
		if len(got) != len(want) {
			t.Fatalf("length %d, want %d (%v)", len(got), len(want), want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("sample %d = %d, want %d (%v)", i, got[i], want[i], want)
			}
		}
	}
}

func TestSignedRejectsUnsorted(t *testing.T) {
	if _, err := EncodeSigned([]int32{3, -4}); !errors.Is(err, ErrUnsorted) {
		t.Errorf("err = %v, want ErrUnsorted", err)
	}
}

// The two blob formats must never be read as each other. A version-1 blob
// decoded as signed would report inter-packet delay variation that is really
// round-trip time, and a version-2 blob decoded as unsigned would turn a
// negative first sample into a very large positive one — both plausible enough
// to be believed and wrong. Each decoder checks the version byte instead.
func TestBlobVersionsDoNotCross(t *testing.T) {
	unsigned, err := Encode([]uint32{100, 200, 300})
	if err != nil {
		t.Fatal(err)
	}
	signed, err := EncodeSigned([]int32{-100, 0, 300})
	if err != nil {
		t.Fatal(err)
	}
	if unsigned[0] == signed[0] {
		t.Fatalf("both formats start with 0x%02x; they are indistinguishable", unsigned[0])
	}
	if _, err := DecodeSigned(unsigned); err == nil {
		t.Error("DecodeSigned accepted a version-1 blob")
	}
	if _, err := Decode(signed); err == nil {
		t.Error("Decode accepted a version-2 blob")
	}
}

func TestSignedRejectsCorruptBlob(t *testing.T) {
	if _, err := DecodeSigned(nil); !errors.Is(err, ErrEmpty) {
		t.Errorf("empty blob err = %v, want ErrEmpty", err)
	}
	// A delta that walks the accumulator past int32 must be refused, not
	// wrapped into a small in-range number that still looks sorted.
	blob := []byte{Version2}
	var tmp [binary.MaxVarintLen64]byte
	blob = append(blob, tmp[:binary.PutVarint(tmp[:], math.MaxInt32-1)]...)
	blob = append(blob, tmp[:binary.PutUvarint(tmp[:], 1<<40)]...)
	if _, err := DecodeSigned(blob); err == nil {
		t.Error("DecodeSigned accepted a delta that overflows int32")
	}
	// A truncated varint is a truncated blob, not a shorter series.
	if _, err := DecodeSigned([]byte{Version2, 0xff}); err == nil {
		t.Error("DecodeSigned accepted a truncated varint")
	}
}
