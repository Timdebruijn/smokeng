package store

import (
	"errors"
	"fmt"
	"math/bits"
)

// A loss mask records which probes of an interval got no reply, in the order
// they were sent. An interval's RTTs are stored sorted, so the send order is
// gone once it is written and a pattern like "the tail of every burst is
// lost" — what ICMP rate limiting at a target produces — cannot be recovered.
//
// Layout: one header byte, then ceil(sent/8) bytes of bits.
//
//	header bit 0   1 when the probes were sent in spread mode, 0 for burst
//	header bits 1-7 reserved, written as 0; a decoder refuses a mask that sets
//	               any, which is how a future format is told apart from this one
//	probe i        bit i%8 (least significant first) of byte 1+i/8; set = lost
//
// Bits past the last probe are 0, and a decoder refuses a mask where they are
// not. A mask is stored only for an interval whose loss was partial and whose
// positions are known (see the prober's finalize); nil means "not recorded".

const lossMaskSpread = 1

// EncodeLossMask builds the mask for probes in send order, lost[i] being true
// when probe i got no valid reply.
func EncodeLossMask(spread bool, lost []bool) []byte {
	out := make([]byte, 1+(len(lost)+7)/8)
	if spread {
		out[0] = lossMaskSpread
	}
	for i, l := range lost {
		if l {
			out[1+i/8] |= 1 << (i % 8)
		}
	}
	return out
}

// DecodeLossMask checks a mask against the counts of the row it belongs to and
// returns what it says. The counts are what make it checkable: a mask from
// another machine whose length or popcount disagrees with sent and received did
// not describe this interval.
func DecodeLossMask(mask []byte, sent, received int) (spread bool, lost []bool, err error) {
	if sent < 0 || received < 0 || received > sent {
		return false, nil, fmt.Errorf("loss mask: %d received of %d sent", received, sent)
	}
	if want := 1 + (sent+7)/8; len(mask) != want {
		return false, nil, fmt.Errorf("loss mask: %d bytes for %d probes, want %d", len(mask), sent, want)
	}
	if mask[0]&^lossMaskSpread != 0 {
		return false, nil, errors.New("loss mask: reserved header bits set")
	}
	if pad := sent % 8; pad != 0 && mask[len(mask)-1]>>pad != 0 {
		return false, nil, errors.New("loss mask: bits set past the last probe")
	}
	lost = make([]bool, sent)
	n := 0
	for i := range lost {
		lost[i] = mask[1+i/8]&(1<<(i%8)) != 0
	}
	for _, b := range mask[1:] {
		n += bits.OnesCount8(b)
	}
	if n != sent-received {
		return false, nil, fmt.Errorf("loss mask: %d probes marked lost, but %d sent and %d received", n, sent, received)
	}
	return mask[0]&lossMaskSpread != 0, lost, nil
}
