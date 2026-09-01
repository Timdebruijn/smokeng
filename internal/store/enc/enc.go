// Package enc implements the versioned samples-blob codec (DESIGN.md §3.3).
//
// Blob layout, format version 1:
//
//	byte 0    format version (0x01)
//	uvarint   first RTT, in microseconds
//	uvarint*  deltas to each subsequent RTT (non-negative: samples sorted ascending)
//
// An empty sample set (a 100%-loss interval) encodes to just the version byte.
// The sample count is implicit: varints are self-delimiting and are decoded to
// the end of the blob. The unit is fixed at 1 µs in version 1.
package enc

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// Version1 is the only blob format version currently defined.
const Version1 = 0x01

var (
	// ErrEmpty is returned for a zero-length blob, which is invalid: even a
	// fully lost interval carries the version byte.
	ErrEmpty = errors.New("enc: empty blob")
	// ErrUnsorted is returned by Encode when samples are not sorted ascending.
	ErrUnsorted = errors.New("enc: samples not sorted ascending")
)

// Encode encodes RTT samples (microseconds, sorted ascending) into a
// version-1 blob.
func Encode(samples []uint32) ([]byte, error) {
	buf := make([]byte, 1, 1+len(samples)*3)
	buf[0] = Version1
	var tmp [binary.MaxVarintLen64]byte
	var prev uint32
	for i, s := range samples {
		if i > 0 && s < prev {
			return nil, ErrUnsorted
		}
		v := uint64(s)
		if i > 0 {
			v = uint64(s - prev)
		}
		n := binary.PutUvarint(tmp[:], v)
		buf = append(buf, tmp[:n]...)
		prev = s
	}
	return buf, nil
}

// Decode decodes a samples blob. The returned samples are in microseconds,
// sorted ascending; a fully lost interval decodes to an empty slice.
func Decode(blob []byte) ([]uint32, error) {
	if len(blob) == 0 {
		return nil, ErrEmpty
	}
	if blob[0] != Version1 {
		return nil, fmt.Errorf("enc: unsupported blob version 0x%02x", blob[0])
	}
	rest := blob[1:]
	var out []uint32
	var cur uint64
	for i := 0; len(rest) > 0; i++ {
		v, n := binary.Uvarint(rest)
		if n <= 0 {
			return nil, fmt.Errorf("enc: truncated or oversized varint at sample %d", i)
		}
		rest = rest[n:]
		if i == 0 {
			cur = v
		} else {
			// Check before adding. Testing the sum afterwards missed a delta
			// big enough to wrap the accumulator, which lands back in range as
			// a small number that passes every later check — a corrupt blob
			// decoding to plausible, wrong, unsorted samples.
			if v > math.MaxUint32-cur {
				return nil, fmt.Errorf("enc: delta at sample %d overflows uint32 microseconds", i)
			}
			cur += v
		}
		if cur > math.MaxUint32 {
			return nil, fmt.Errorf("enc: sample %d overflows uint32 microseconds", i)
		}
		out = append(out, uint32(cur))
	}
	return out, nil
}

// Blob layout, format version 2 (signed series):
//
//	byte 0    format version (0x02)
//	varint    first value, in microseconds, zig-zag encoded (may be negative)
//	uvarint*  deltas to each subsequent value (non-negative: sorted ascending)
//
// Version 2 exists because not every per-packet series is a duration. Inter-
// packet delay variation is a difference between consecutive packets, so it is
// negative exactly when a packet arrived sooner than the one before it, and
// roughly half of a healthy series is. Storing its absolute value — which is
// what irtt's own summary statistics do — would throw away the difference
// between a link that jitters symmetrically and one that only ever bursts late.
//
// Only the first value needs the signed encoding: the samples are sorted
// ascending, so every delta after it is non-negative and pays the same single
// byte a version-1 delta does.

// Version2 is the signed-series blob format.
const Version2 = 0x02

// EncodeSigned encodes signed samples (microseconds, sorted ascending) into a
// version-2 blob.
func EncodeSigned(samples []int32) ([]byte, error) {
	buf := make([]byte, 1, 1+len(samples)*3)
	buf[0] = Version2
	var tmp [binary.MaxVarintLen64]byte
	var prev int32
	for i, s := range samples {
		if i > 0 && s < prev {
			return nil, ErrUnsorted
		}
		var n int
		if i == 0 {
			n = binary.PutVarint(tmp[:], int64(s))
		} else {
			n = binary.PutUvarint(tmp[:], uint64(int64(s)-int64(prev)))
		}
		buf = append(buf, tmp[:n]...)
		prev = s
	}
	return buf, nil
}

// DecodeSigned decodes a version-2 blob. The returned samples are in
// microseconds, sorted ascending; an empty series decodes to an empty slice.
func DecodeSigned(blob []byte) ([]int32, error) {
	if len(blob) == 0 {
		return nil, ErrEmpty
	}
	if blob[0] != Version2 {
		return nil, fmt.Errorf("enc: unsupported signed blob version 0x%02x", blob[0])
	}
	rest := blob[1:]
	var out []int32
	var cur int64
	for i := 0; len(rest) > 0; i++ {
		if i == 0 {
			v, n := binary.Varint(rest)
			if n <= 0 {
				return nil, fmt.Errorf("enc: truncated or oversized varint at sample %d", i)
			}
			rest, cur = rest[n:], v
		} else {
			v, n := binary.Uvarint(rest)
			if n <= 0 {
				return nil, fmt.Errorf("enc: truncated or oversized varint at sample %d", i)
			}
			rest = rest[n:]
			// Checked before adding, for the same reason version 1 does: a
			// delta big enough to wrap the accumulator lands back in range as
			// a plausible, wrong, still-ascending sample.
			if v > uint64(math.MaxInt32-cur) {
				return nil, fmt.Errorf("enc: delta at sample %d overflows int32 microseconds", i)
			}
			cur += int64(v)
		}
		if cur < math.MinInt32 || cur > math.MaxInt32 {
			return nil, fmt.Errorf("enc: sample %d overflows int32 microseconds", i)
		}
		out = append(out, int32(cur))
	}
	return out, nil
}
