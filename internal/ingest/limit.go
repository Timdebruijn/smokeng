package ingest

import (
	"fmt"

	"github.com/apache/arrow-go/v18/arrow/memory"
)

// maxDecodeBytes caps everything one submission may allocate while it is being
// decoded.
//
// It exists because the body limit does not bound the decode. Arrow IPC carries
// an optional per-buffer compression codec, and the reader allocates whatever
// uncompressed size the message header claims — arrow-go does
// `raw.Resize(int(uncompressedSize))` with no ceiling and no ratio check. A
// 629 KB body declaring three lists of 500M zeros grows the heap by 5.6 GiB
// inside the reader, before a line of DecodeBatch runs; at the 8 MiB body limit
// a 9313:1 zstd ratio reaches tens of gigabytes. The master is OOM-killed, and
// on a single-host install that takes the prober with it.
//
// smokeng's own encoder never compresses, so no honest agent is affected. The
// limit is generous next to an uncompressed batch — the wire form is a few tens
// of bytes per measurement, so 8 MiB of body decodes to roughly 20 MiB — and
// still refuses the pathological case long before the host notices.
const maxDecodeBytes = 64 << 20

// errAllocationLimit is what a limitAllocator panics with. The Arrow reader
// gives no way to fail an allocation politely: the allocator interface returns
// a slice and the reader panics on a short one anyway. So the limit unwinds by
// panic and DecodeBatch turns it back into an ordinary error, which keeps the
// blast radius inside one request rather than relying on net/http's recovery.
type errAllocationLimit struct{ want, used, limit int }

func (e *errAllocationLimit) Error() string {
	return fmt.Sprintf("ingest: batch wanted %d more bytes to decode (%d already used, limit %d); "+
		"refusing it rather than allocating what a compressed payload claims",
		e.want, e.used, e.limit)
}

// limitAllocator is a memory.Allocator that refuses to hand out more than limit
// bytes in total. It is deliberately not concurrency-safe: one is made per
// DecodeBatch call and never shared, so a mutex would only cost.
type limitAllocator struct {
	mem   memory.Allocator
	used  int
	limit int
}

func newLimitAllocator(limit int) *limitAllocator {
	return &limitAllocator{mem: memory.DefaultAllocator, limit: limit}
}

// take accounts for delta more bytes, panicking with errAllocationLimit when
// that would cross the limit. A negative size is refused too: it can only come
// from a corrupt or hostile header, and passing it on invites the allocator
// beneath to panic somewhere less legible.
func (a *limitAllocator) take(delta int) {
	if delta < 0 || delta > a.limit-a.used {
		panic(&errAllocationLimit{want: delta, used: a.used, limit: a.limit})
	}
	a.used += delta
}

func (a *limitAllocator) Allocate(size int) []byte {
	a.take(size)
	return a.mem.Allocate(size)
}

func (a *limitAllocator) Reallocate(size int, b []byte) []byte {
	// Only growth is charged; shrinking is not credited back. Over one batch
	// that overstates usage slightly, which errs towards refusing early — the
	// safe direction for a limit whose whole job is to stop an allocation that
	// should never have been requested.
	if d := size - len(b); d > 0 {
		a.take(d)
	}
	return a.mem.Reallocate(size, b)
}

func (a *limitAllocator) Free(b []byte) { a.mem.Free(b) }
