package ingest

import (
	"bytes"
	"strings"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/ipc"
	"github.com/apache/arrow-go/v18/arrow/memory"

	"github.com/timdebruijn/smokeng/internal/store"
	"github.com/timdebruijn/smokeng/internal/store/enc"
)

// oneRow builds a valid single-measurement batch, optionally compressed, with
// a series of the given length.
func oneRow(t *testing.T, seriesLen int, opts ...ipc.Option) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := ipc.NewWriter(&buf, append([]ipc.Option{ipc.WithSchema(BatchSchema)}, opts...)...)
	rb := array.NewRecordBuilder(memory.DefaultAllocator, BatchSchema)
	defer rb.Release()
	rb.Field(0).(*array.Int64Builder).Append(1)
	rb.Field(1).(*array.TimestampBuilder).Append(arrow.Timestamp(100))
	rb.Field(2).(*array.Uint16Builder).Append(2)
	rb.Field(3).(*array.Uint16Builder).Append(2)
	rb.Field(4).(*array.Uint8Builder).Append(0)
	l := rb.Field(5).(*array.ListBuilder)
	l.Append(true)
	l.ValueBuilder().(*array.Uint32Builder).AppendValues([]uint32{10, 20}, nil)
	rb.Field(6).(*array.Uint16Builder).AppendNull()
	for i := range 3 {
		sl := rb.Field(7 + i).(*array.ListBuilder)
		if i > 0 || seriesLen < 0 {
			sl.AppendNull()
			continue
		}
		sl.Append(true)
		vals := make([]int32, seriesLen)
		for j := range vals {
			vals[j] = int32(j)
		}
		sl.ValueBuilder().(*array.Int32Builder).AppendValues(vals, nil)
	}
	rec := rb.NewRecord()
	defer rec.Release()
	if err := w.Write(rec); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// A compressed batch is decoded through a bounded allocator. smokeng's own
// encoder never compresses, and the Arrow reader allocates whatever
// uncompressed size a message header claims — so without the bound a small
// body can ask for tens of gigabytes and take the master with it.
func TestDecodeBatchBoundsAllocation(t *testing.T) {
	// A well-formed compressed batch still decodes: the bound is a ceiling,
	// not a ban.
	body := oneRow(t, 2, ipc.WithZstd())
	out, err := DecodeBatch(body, 3)
	if err != nil {
		t.Fatalf("a compressed batch within the limit was refused: %v", err)
	}
	if len(out) != 1 || len(out[0].Series[store.SeriesIPDVSend]) != 2 {
		t.Fatalf("compressed batch did not survive: %+v", out)
	}

	// And the allocator itself refuses to cross the ceiling rather than
	// handing out what was asked for.
	a := newLimitAllocator(1024)
	a.Allocate(600)
	func() {
		defer func() {
			r := recover()
			if r == nil {
				t.Fatal("the allocator handed out more than its limit")
			}
			if _, ok := r.(*errAllocationLimit); !ok {
				t.Fatalf("panicked with %T, want *errAllocationLimit", r)
			}
		}()
		a.Allocate(600)
	}()
	// A negative size is a corrupt header, not a free allocation — on both
	// paths. Reallocate charges only growth, and a negative size is not growth,
	// so it was the one path that could carry a corrupt header's arithmetic
	// through unchecked.
	for _, c := range []struct {
		name string
		call func(*limitAllocator)
	}{
		{"Allocate", func(a *limitAllocator) { a.Allocate(-1) }},
		{"Reallocate", func(a *limitAllocator) { a.Reallocate(-1, make([]byte, 8)) }},
	} {
		func() {
			defer func() {
				r := recover()
				if r == nil {
					t.Fatalf("%s accepted a negative size", c.name)
				}
				lim, ok := r.(*errAllocationLimit)
				if !ok {
					t.Fatalf("%s panicked with %T, want *errAllocationLimit", c.name, r)
				}
				// And says what actually happened, rather than reporting a
				// negative number of wanted bytes as if it were a shortfall.
				if !strings.Contains(lim.Error(), "negative allocation") {
					t.Errorf("%s: %v", c.name, lim)
				}
			}()
			c.call(newLimitAllocator(1024))
		}()
	}
}

// List offsets arrive unvalidated and were used as a make() capacity, so a
// tiny body could reserve gigabytes before the read that would have failed.
func TestListBoundsRefusesImpossibleOffsets(t *testing.T) {
	cases := []struct {
		name    string
		offsets []int32
		row     int
		child   int
	}{
		{"past the end", []int32{0, 1 << 30}, 0, 4},
		{"negative", []int32{-1, 2}, 0, 4},
		{"descending", []int32{3, 1}, 0, 4},
		{"no offset for the row", []int32{0}, 0, 4},
	}
	for _, c := range cases {
		if _, _, err := listBounds(c.offsets, c.row, c.child, "ipdv_send"); err == nil {
			t.Errorf("%s: offsets %v accepted", c.name, c.offsets)
		}
	}
	// The honest case still works.
	lo, hi, err := listBounds([]int32{0, 2, 5}, 1, 5, "ipdv_send")
	if err != nil || lo != 2 || hi != 5 {
		t.Errorf("valid offsets rejected: %d,%d,%v", lo, hi, err)
	}
}

// A series cannot hold more values than the interval sent probes. Without the
// bound one row could carry millions of values into a blob kept forever.
func TestDecodeBatchRefusesOversizedSeries(t *testing.T) {
	_, err := DecodeBatch(oneRow(t, 50), 3) // sent = 2
	if err == nil {
		t.Fatal("a series longer than the probes sent was accepted")
	}
	if !strings.Contains(err.Error(), "probes were sent") {
		t.Errorf("error does not explain the bound: %v", err)
	}
}

// An agent that sends a distribution out of order must not be able to wedge
// its own outbox. The store's codec requires ascending order and fails the
// whole write otherwise; since these are distributions, order carries no
// meaning and sorting on the way in is lossless.
func TestDecodeBatchNormalisesUnsortedValues(t *testing.T) {
	var buf bytes.Buffer
	w := ipc.NewWriter(&buf, ipc.WithSchema(BatchSchema))
	rb := array.NewRecordBuilder(memory.DefaultAllocator, BatchSchema)
	defer rb.Release()
	rb.Field(0).(*array.Int64Builder).Append(1)
	rb.Field(1).(*array.TimestampBuilder).Append(arrow.Timestamp(100))
	rb.Field(2).(*array.Uint16Builder).Append(3)
	rb.Field(3).(*array.Uint16Builder).Append(3)
	rb.Field(4).(*array.Uint8Builder).Append(0)
	l := rb.Field(5).(*array.ListBuilder)
	l.Append(true)
	l.ValueBuilder().(*array.Uint32Builder).AppendValues([]uint32{30, 10, 20}, nil)
	rb.Field(6).(*array.Uint16Builder).AppendNull()
	sl := rb.Field(7).(*array.ListBuilder)
	sl.Append(true)
	sl.ValueBuilder().(*array.Int32Builder).AppendValues([]int32{5, -3, 1}, nil)
	rb.Field(8).(*array.ListBuilder).AppendNull()
	rb.Field(9).(*array.ListBuilder).AppendNull()
	rec := rb.NewRecord()
	defer rec.Release()
	if err := w.Write(rec); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	out, err := DecodeBatch(buf.Bytes(), 3)
	if err != nil {
		t.Fatalf("an unsorted batch was refused rather than normalised: %v", err)
	}
	if got := out[0].Samples; got[0] != 10 || got[2] != 30 {
		t.Errorf("samples not normalised: %v", got)
	}
	got := out[0].Series[store.SeriesIPDVSend]
	if got[0] != -3 || got[2] != 5 {
		t.Errorf("series not normalised: %v", got)
	}
	// The whole point: what comes out is writable by the store.
	if _, err := encodeCheck(out[0]); err != nil {
		t.Errorf("normalised batch still not storable: %v", err)
	}
}

// encodeCheck runs the same blob encoding the store would, so the test asserts
// storability rather than a proxy for it.
func encodeCheck(m store.Measurement) ([]byte, error) {
	b, err := enc.Encode(m.Samples)
	if err != nil {
		return nil, err
	}
	for _, v := range m.Series {
		if _, err := enc.EncodeSigned(v); err != nil {
			return nil, err
		}
	}
	return b, nil
}

// The real path, end to end: a compressed batch whose uncompressed buffers
// exceed the budget is refused by the reader before it can allocate them.
//
// The earlier test only called the allocator directly, which proved the
// accounting but not the wiring — and the wiring is the part that was
// described wrongly: arrow-go recovers the allocator's panic itself, one frame
// in, so the error arrives through reader.Err() rather than through this
// package's own recover. Either way it must be an error and not an
// out-of-memory, and it must name the limit.
func TestDecodeBatchRefusesOversizedDecompression(t *testing.T) {
	// Large enough to be worth compressing, small enough to build quickly.
	const rows = 4000
	var buf bytes.Buffer
	w := ipc.NewWriter(&buf, ipc.WithSchema(BatchSchema), ipc.WithZstd())
	rb := array.NewRecordBuilder(memory.DefaultAllocator, BatchSchema)
	defer rb.Release()
	for range rows {
		rb.Field(0).(*array.Int64Builder).Append(1)
		rb.Field(1).(*array.TimestampBuilder).Append(arrow.Timestamp(100))
		rb.Field(2).(*array.Uint16Builder).Append(2)
		rb.Field(3).(*array.Uint16Builder).Append(2)
		rb.Field(4).(*array.Uint8Builder).Append(0)
		l := rb.Field(5).(*array.ListBuilder)
		l.Append(true)
		l.ValueBuilder().(*array.Uint32Builder).AppendValues([]uint32{0, 0}, nil)
		rb.Field(6).(*array.Uint16Builder).AppendNull()
		for i := range 3 {
			rb.Field(7 + i).(*array.ListBuilder).AppendNull()
		}
	}
	rec := rb.NewRecord()
	defer rec.Release()
	if err := w.Write(rec); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	body := buf.Bytes()

	// With a budget below what those buffers decompress to, it is refused.
	if _, err := decodeBatch(body, 3, 1024); err == nil {
		t.Fatal("a batch whose buffers exceed the allocation budget was decoded")
	} else if !strings.Contains(err.Error(), "refusing it rather than allocating") {
		t.Errorf("the error does not explain the refusal: %v", err)
	}

	// And with the production budget the same batch is perfectly ordinary, so
	// the limit is a ceiling rather than a ban on compression.
	out, err := decodeBatch(body, 3, maxDecodeBytes)
	if err != nil {
		t.Fatalf("an honest compressed batch was refused: %v", err)
	}
	if len(out) != rows {
		t.Errorf("got %d measurements, want %d", len(out), rows)
	}
}
