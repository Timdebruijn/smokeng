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
