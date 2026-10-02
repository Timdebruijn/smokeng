package ingest

import (
	"bytes"
	"encoding/binary"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/ipc"
	"github.com/apache/arrow-go/v18/arrow/memory"

	"github.com/timdebruijn/smokeng/internal/store"
)

// frame is one message of an IPC stream, located by an independent walk so a
// test can patch a field of a real, valid body instead of hand-building one.
type frame struct {
	start        int // the continuation marker
	metaOff      int
	metaLen      int
	bodyOff      int
	bodyLen      int
	end          int
	lengthPrefix int // offset of the 4-byte metadata length
}

func frames(t *testing.T, body []byte) (fs []frame, eos int) {
	t.Helper()
	off := 0
	for off < len(body) {
		f := frame{start: off}
		n := binary.LittleEndian.Uint32(body[off:])
		off += 4
		if n == 0xFFFFFFFF {
			f.lengthPrefix = off
			n = binary.LittleEndian.Uint32(body[off:])
			off += 4
		} else {
			f.lengthPrefix = f.start
		}
		if n == 0 {
			return fs, f.start
		}
		f.metaOff, f.metaLen = off, int(n)
		off += int(n)
		_, bl, err := inspectMessage(body[f.metaOff : f.metaOff+f.metaLen])
		if err != nil {
			t.Fatalf("the reference walk could not read a valid body: %v", err)
		}
		f.bodyOff, f.bodyLen = off, int(bl)
		off += int(bl)
		f.end = off
		fs = append(fs, f)
	}
	t.Fatal("no end-of-stream marker in a body the writer produced")
	return nil, 0
}

// patchMetaField overwrites an n-byte little-endian field inside one message's
// metadata, found through the flatbuffer's own vtable.
func patchMetaField(t *testing.T, body []byte, f frame, vt, n int, v uint64) []byte {
	t.Helper()
	meta := body[f.metaOff : f.metaOff+f.metaLen]
	root, err := fbRoot(meta)
	if err != nil {
		t.Fatal(err)
	}
	pos, ok, err := root.field(vt)
	if err != nil || !ok {
		t.Fatalf("field at vtable offset %d not found (%v)", vt, err)
	}
	out := slices.Clone(body)
	abs := f.metaOff + pos
	switch n {
	case 1:
		out[abs] = byte(v)
	case 8:
		binary.LittleEndian.PutUint64(out[abs:], v)
	default:
		t.Fatalf("unsupported field width %d", n)
	}
	return out
}

func allocated(f func()) uint64 {
	var a, b runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&a)
	f()
	runtime.ReadMemStats(&b)
	return b.TotalAlloc - a.TotalAlloc
}

func TestScreenFramesAcceptsAnHonestBatch(t *testing.T) {
	body, err := EncodeBatch([]store.Measurement{
		{TargetID: 1, TS: 100, Sent: 3, Received: 2, Samples: []uint32{10, 20}},
		{TargetID: 1, TS: 160, Sent: 3, Received: 3, Samples: []uint32{10, 20, 30}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := screenFrames(body); err != nil {
		t.Fatalf("an honest batch was refused: %v", err)
	}
	out, err := DecodeBatch(body, 7)
	if err != nil || len(out) != 2 {
		t.Fatalf("DecodeBatch = %d rows, %v", len(out), err)
	}
}

// The zstd decoder arrow-go builds sizes its window from the frame header
// before it has produced a byte, outside any allocator, so a small frame can
// cost far more than its size. smokeng's own encoder never compresses, so the
// rule is to refuse it, not to meter it.
func TestScreenFramesRefusesCompression(t *testing.T) {
	for name, opt := range map[string]ipc.Option{"zstd": ipc.WithZstd(), "lz4": ipc.WithLZ4()} {
		t.Run(name, func(t *testing.T) {
			body := oneRow(t, 2, opt)
			err := screenFrames(body)
			if err == nil {
				t.Fatal("a compressed batch passed the screen")
			}
			if !strings.Contains(err.Error(), "compressed") {
				t.Errorf("the refusal does not say why: %v", err)
			}
			if _, err := DecodeBatch(body, 3); err == nil {
				t.Error("DecodeBatch accepted a compressed batch")
			}
		})
	}
}

// The metadata of every message is read into make([]byte, n) with n straight
// off the wire, outside the allocator the reader is given, so a tiny body that
// claims a huge header used to cost the claim.
func TestScreenFramesRefusesALyingMetadataLength(t *testing.T) {
	good := oneRow(t, 2)
	fs, _ := frames(t, good)
	const claim = 1 << 30
	lie := slices.Clone(good)
	binary.LittleEndian.PutUint32(lie[fs[0].lengthPrefix:], claim)

	if err := screenFrames(lie); err == nil {
		t.Fatal("a header claiming 1 GiB in a body of a few hundred bytes passed the screen")
	}
	if n := allocated(func() { _, _ = DecodeBatch(lie, 3) }); n > 8<<20 {
		t.Errorf("decoding a lying header allocated %d MiB", n>>20)
	}
}

// The reader's own size limits are the second layer. NewReader builds its
// message reader without them, which is how a declared length became an
// allocation; decodeFrames wires them in, and this checks that wiring with the
// screen out of the way.
func TestDecodeFramesBoundsMetadataAndBodyWithoutTheScreen(t *testing.T) {
	good := oneRow(t, 2)
	fs, _ := frames(t, good)

	// Below arrow's own default ceiling for metadata (64 MiB), so the default
	// cannot be what refuses it: only the limit decodeFrames sets from the size
	// of the body can. (1 GiB would pass the test with that line deleted.)
	lieMeta := slices.Clone(good)
	binary.LittleEndian.PutUint32(lieMeta[fs[0].lengthPrefix:], 32<<20)
	if n := allocated(func() {
		if _, err := decodeFrames(lieMeta, 3, maxDecodeBytes); err == nil {
			t.Error("a lying metadata length was decoded")
		}
	}); n > 8<<20 {
		t.Errorf("a lying metadata length allocated %d MiB past the screen", n>>20)
	}

	// Under the allocator's own budget, so only the reader's limit can refuse it.
	lieBody := patchMetaField(t, good, fs[1], vtMessageBodyLength, 8, 32<<20)
	if n := allocated(func() {
		if _, err := decodeFrames(lieBody, 3, maxDecodeBytes); err == nil {
			t.Error("a lying body length was decoded")
		}
	}); n > 8<<20 {
		t.Errorf("a lying body length allocated %d MiB past the screen", n>>20)
	}
}

func TestScreenFramesRefusesALyingBodyLength(t *testing.T) {
	good := oneRow(t, 2)
	fs, _ := frames(t, good)
	for _, claim := range []uint64{uint64(len(good)) + 1, 1 << 40, 1 << 63} {
		lie := patchMetaField(t, good, fs[1], vtMessageBodyLength, 8, claim)
		err := screenFrames(lie)
		if err == nil {
			t.Errorf("a body length of %d in a %d-byte payload passed the screen", claim, len(good))
			continue
		}
		// For the right reason. A negative length would be stopped later anyway,
		// by a read past the front of the slice, but that is luck rather than the
		// check, and it ends the scan with a message about something else.
		if !strings.Contains(err.Error(), "body claims") {
			t.Errorf("a body length of %d was refused for the wrong reason: %v", int64(claim), err)
		}
	}
	// And the exact length is fine: this is a check of the bound, not a ban.
	same := patchMetaField(t, good, fs[1], vtMessageBodyLength, 8, uint64(fs[1].bodyLen))
	if err := screenFrames(same); err != nil {
		t.Errorf("the true body length was refused: %v", err)
	}
}

func TestScreenFramesRefusesWhatIsNotASchemaThenRecordBatches(t *testing.T) {
	good := oneRow(t, 2)
	fs, eos := frames(t, good)

	// A dictionary batch (2) or a tensor (4) in place of the record batch.
	for _, typ := range []uint64{uint64(ipc.MessageDictionaryBatch), uint64(ipc.MessageTensor), uint64(ipc.MessageNone)} {
		if err := screenFrames(patchMetaField(t, good, fs[1], vtMessageHeaderType, 1, typ)); err == nil {
			t.Errorf("message type %d passed the screen", typ)
		}
	}

	// A record batch with no schema in front of it.
	if err := screenFrames(append(slices.Clone(good[fs[1].start:fs[1].end]), good[eos:]...)); err == nil {
		t.Error("a record batch before any schema passed the screen")
	}
	// A schema that is not first.
	twice := slices.Concat(good[:fs[1].start], good[fs[0].start:fs[0].end], good[fs[1].start:])
	if err := screenFrames(twice); err == nil {
		t.Error("a second schema passed the screen")
	}
}

func TestScreenFramesCapsTheNumberOfMessages(t *testing.T) {
	good := oneRow(t, 2)
	fs, eos := frames(t, good)
	batch := good[fs[1].start:fs[1].end]
	build := func(n int) []byte {
		out := slices.Clone(good[:fs[1].start])
		for range n {
			out = append(out, batch...)
		}
		return append(out, good[eos:]...)
	}
	if err := screenFrames(build(maxMessages - 1)); err != nil {
		t.Errorf("a stream of %d messages was refused: %v", maxMessages, err)
	}
	if err := screenFrames(build(maxMessages)); err == nil {
		t.Errorf("a stream of %d messages passed the screen", maxMessages+1)
	}
}

// screenFrames runs before the recover in decodeFrames, on a body nobody has
// authenticated the contents of: it must return an error for any damage and
// never panic. Every byte of a real body is replaced with values that exercise
// both ends of a length or offset, and every prefix is tried.
func TestScreenFramesNeverPanics(t *testing.T) {
	good := oneRow(t, 2)
	try := func(label string, b []byte) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("%s: panic: %v", label, r)
			}
		}()
		_ = screenFrames(b)
	}
	for i := range good {
		for _, v := range []byte{0x00, 0x01, 0x7F, 0x80, 0xFE, 0xFF, good[i] ^ 0x01} {
			b := slices.Clone(good)
			b[i] = v
			try("byte "+string(rune('0'+i%10)), b)
		}
	}
	for n := range len(good) {
		try("prefix", good[:n])
	}
}

// IsNull indexes the validity bitmap without a length check, and the reader
// does not check it either. A bitmap shorter than its column was an
// out-of-range panic on the ninth row.
func TestDecodeBatchRefusesAShortValidityBitmap(t *testing.T) {
	const rows = 20
	rb := array.NewRecordBuilder(memory.DefaultAllocator, BatchSchema)
	defer rb.Release()
	for range rows {
		field[*array.Int64Builder](t, rb, "target_id").Append(1)
		field[*array.TimestampBuilder](t, rb, "ts").Append(arrow.Timestamp(100))
		field[*array.Uint16Builder](t, rb, "sent").Append(1)
		field[*array.Uint16Builder](t, rb, "received").Append(0)
		field[*array.Uint8Builder](t, rb, "flags").Append(0)
		field[*array.ListBuilder](t, rb, "samples").Append(true)
		field[*array.Uint16Builder](t, rb, "icmp_error").AppendNull()
		field[*array.Uint8Builder](t, rb, "send_error").AppendNull()
		for _, name := range store.KnownSeries {
			field[*array.ListBuilder](t, rb, name).AppendNull()
		}
	}
	rec := rb.NewRecord()
	defer rec.Release()

	// icmp_error with a one-byte bitmap for twenty rows.
	icmpIdx := rec.Schema().FieldIndices("icmp_error")[0]
	short := array.NewData(arrow.PrimitiveTypes.Uint16, rows,
		[]*memory.Buffer{memory.NewBufferBytes([]byte{0xFF}), memory.NewBufferBytes(make([]byte, rows*2))},
		nil, 1, 0)
	defer short.Release()
	cols := slices.Clone(rec.Columns())
	cols[icmpIdx] = array.NewUint16Data(short)
	bad := array.NewRecord(rec.Schema(), cols, rows)
	defer bad.Release()

	var buf bytes.Buffer
	w := ipc.NewWriter(&buf, ipc.WithSchema(BatchSchema))
	if err := w.Write(bad); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	_, err := DecodeBatch(buf.Bytes(), 3)
	if err == nil {
		t.Fatal("a batch whose validity bitmap is shorter than its column was decoded")
	}
	if !strings.Contains(err.Error(), "validity bitmap") {
		t.Errorf("the error does not name the bitmap: %v", err)
	}
}

func TestDecodeBatchCapsRows(t *testing.T) {
	rows := func(n int) []store.Measurement {
		ms := make([]store.Measurement, n)
		for i := range ms {
			ms[i] = store.Measurement{TargetID: 1, TS: int64(i), Sent: 1}
		}
		return ms
	}
	body, err := EncodeBatch(rows(maxBatchRows))
	if err != nil {
		t.Fatal(err)
	}
	if out, err := DecodeBatch(body, 3); err != nil || len(out) != maxBatchRows {
		t.Fatalf("a batch of exactly the limit = %d rows, %v", len(out), err)
	}
	body, err = EncodeBatch(rows(maxBatchRows + 1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeBatch(body, 3); err == nil {
		t.Error("a batch one row over the limit was decoded")
	}
}

// A reply needs a probe. More received than sent reads as negative loss.
func TestDecodeBatchRefusesMoreRepliesThanProbes(t *testing.T) {
	body, err := EncodeBatch([]store.Measurement{
		{TargetID: 1, TS: 100, Sent: 2, Received: 3, Samples: []uint32{1, 2, 3}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeBatch(body, 3); err == nil || !strings.Contains(err.Error(), "replies to") {
		t.Fatalf("received > sent: err = %v", err)
	}
}

// checkValidity has to count the array's offset: a slice of a larger array
// reads its bits from offset+i. Arrays that come off the wire always have
// offset 0, so only a direct test can see this term.
func TestCheckValidityCountsTheOffset(t *testing.T) {
	data := func(bitmap []byte, offset int) arrow.Array {
		d := array.NewData(arrow.PrimitiveTypes.Uint16, 8,
			[]*memory.Buffer{memory.NewBufferBytes(bitmap), memory.NewBufferBytes(make([]byte, 64))},
			nil, 1, offset)
		defer d.Release()
		return array.NewUint16Data(d)
	}
	// Eight rows starting at bit 8 need two bytes of bitmap.
	if err := checkValidity("x", data([]byte{0xFF}, 8)); err == nil {
		t.Error("a one-byte bitmap for rows 8..15 passed")
	}
	if err := checkValidity("x", data([]byte{0xFF, 0xFF}, 8)); err != nil {
		t.Errorf("a two-byte bitmap for rows 8..15 was refused: %v", err)
	}
	// No bitmap at all is how Arrow says nothing is null.
	if err := checkValidity("x", data(nil, 0)); err != nil {
		t.Errorf("an array without a bitmap was refused: %v", err)
	}
}
