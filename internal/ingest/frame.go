package ingest

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/apache/arrow-go/v18/arrow/ipc"
)

// screenFrames walks the Arrow IPC framing of a submission and refuses it
// before the reader has allocated anything on its word.
//
// Two things in arrow-go's reader trust the stream. The metadata of each
// message is read into make([]byte, n) with n taken straight off the wire, and
// that allocation never passes through the allocator decodeBatch hands the
// reader, so a header that claims a large length costs that much however short
// the body is. And a compressed record batch is expanded by a zstd decoder
// built with its default options, which sizes its window from the frame header
// before it has produced a byte, again outside the allocator: the cost is set
// by what the frame claims, not by its size.
//
// Neither is reachable by an honest agent. The body is in memory already, so
// every length the stream declares can be checked against the bytes that are
// actually there, and smokeng's own encoder never compresses and never writes
// anything but one schema message and record batches. So the rule is
// deliberately blunt: refuse what is not that, instead of trying to meter it.
//
// It reads the flatbuffer headers itself because arrow-go keeps them behind an
// internal package. Every read is bounds-checked, written so the check cannot
// wrap on a 32-bit int, so a malformed header is an error and not a panic. What
// it must also do is see the same fields the reader sees, and the test that
// guards that compares it with the reader rather than with the format.
func screenFrames(body []byte) error {
	off, messages := 0, 0
	for off < len(body) {
		n, ok := le32(body, off)
		if !ok {
			return errFrame("truncated length prefix")
		}
		off += 4
		if n == 0xFFFFFFFF { // continuation marker, then the real length
			if n, ok = le32(body, off); !ok {
				return errFrame("truncated length prefix")
			}
			off += 4
		}
		if n == 0 { // end-of-stream marker
			return nil
		}
		if messages++; messages > maxMessages {
			return errFrame("more than %d messages", maxMessages)
		}
		if uint64(n) > uint64(len(body)-off) {
			return errFrame("message header claims %d bytes, %d remain", n, len(body)-off)
		}
		meta := body[off : off+int(n)]
		off += int(n)

		hdr, bodyLen, err := inspectMessage(meta)
		if err != nil {
			return err
		}
		switch hdr.typ {
		case ipc.MessageSchema:
			if messages != 1 {
				return errFrame("schema message is not first")
			}
			if hdr.schemaFields > maxSchemaFields {
				return errFrame("schema declares %d fields, more than the %d allowed", hdr.schemaFields, maxSchemaFields)
			}
		case ipc.MessageRecordBatch:
			if messages == 1 {
				return errFrame("record batch before any schema")
			}
			if hdr.compressed {
				return errFrame("compressed record batch; smokeng agents do not compress, " +
					"and the decoder sizes its buffers from the compressed frame")
			}
		default:
			return errFrame("message type %d is not part of a submission", hdr.typ)
		}
		if bodyLen < 0 || bodyLen > int64(len(body)-off) {
			return errFrame("message body claims %d bytes, %d remain", bodyLen, len(body)-off)
		}
		off += int(bodyLen)
	}
	return nil
}

// maxMessages bounds one stream: the schema, then record batches. An agent
// sends one batch of at most pushBatch rows, so this is generous, and it stops
// a stream of empty batches from being a way to spend CPU on a body that is
// otherwise small.
const maxMessages = 32

// maxSchemaFields bounds the columns a schema may declare. The reader builds a
// Go value per field before it reads a row, outside the allocator, so the cost
// is set by the count the schema claims. The real schema has a dozen.
const maxSchemaFields = 64

type msgHeader struct {
	typ          ipc.MessageType
	compressed   bool
	schemaFields int
}

// Flatbuffer vtable offsets are 4 + 2*slot. These are the slots of the Arrow
// Message and RecordBatch tables (format/Message.fbs), checked against the
// generated code in arrow-go rather than remembered.
const (
	vtMessageHeaderType = 6  // Message.header_type, ubyte
	vtMessageHeader     = 8  // Message.header, union
	vtMessageBodyLength = 10 // Message.bodyLength, int64
	vtBatchCompression  = 10 // RecordBatch.compression, table
	vtSchemaFields      = 6  // Schema.fields, vector of Field
)

var errFrameShort = errors.New("ingest: malformed message header")

func errFrame(format string, args ...any) error {
	return fmt.Errorf("ingest: malformed batch: "+format, args...)
}

// inspectMessage reads what screenFrames needs from one message's metadata.
func inspectMessage(meta []byte) (hdr msgHeader, bodyLen int64, err error) {
	root, err := fbRoot(meta)
	if err != nil {
		return hdr, 0, err
	}
	if p, ok, err := root.field(vtMessageHeaderType); err != nil {
		return hdr, 0, err
	} else if ok {
		if p >= len(meta) {
			return hdr, 0, errFrameShort
		}
		hdr.typ = ipc.MessageType(meta[p])
	}
	if p, ok, err := root.field(vtMessageBodyLength); err != nil {
		return hdr, 0, err
	} else if ok {
		if p+8 > len(meta) {
			return hdr, 0, errFrameShort
		}
		bodyLen = int64(binary.LittleEndian.Uint64(meta[p:]))
	}
	if hdr.typ == ipc.MessageSchema {
		schema, ok, err := root.table(vtMessageHeader)
		if err != nil {
			return hdr, 0, err
		}
		if ok {
			if hdr.schemaFields, err = schema.vectorLen(vtSchemaFields); err != nil {
				return hdr, 0, err
			}
		}
	}
	if hdr.typ == ipc.MessageRecordBatch {
		batch, ok, err := root.table(vtMessageHeader)
		if err != nil {
			return hdr, 0, err
		}
		if !ok {
			return hdr, 0, errFrame("record batch message without a record batch")
		}
		if _, present, err := batch.field(vtBatchCompression); err != nil {
			return hdr, 0, err
		} else if present {
			hdr.compressed = true
		}
	}
	return hdr, bodyLen, nil
}

// fbTable is a flatbuffer table inside a byte slice. It exists so that no read
// of a hostile header can index past the slice.
type fbTable struct {
	b   []byte
	pos int
}

func fbRoot(b []byte) (fbTable, error) {
	off, ok := le32(b, 0)
	if !ok || uint64(off) >= uint64(len(b)) {
		return fbTable{}, errFrameShort
	}
	return fbTable{b: b, pos: int(off)}, nil
}

// field returns the absolute position of the field whose vtable offset is vt.
// Absent is not an error: it is how a flatbuffer says "default".
func (t fbTable) field(vt int) (pos int, present bool, err error) {
	so, ok := le32(t.b, t.pos)
	if !ok {
		return 0, false, errFrameShort
	}
	vtable := t.pos - int(int32(so))
	size, ok := le16(t.b, vtable)
	if !ok || size < 4 {
		return 0, false, errFrameShort
	}
	// Present when the offset is below the vtable's declared size, which is the
	// rule flatbuffers (and so arrow-go's reader) applies. Not "when both bytes
	// of the slot fit": for an odd size the two disagree on the last slot, the
	// reader finds a field the screen thinks is absent, and that is how a
	// compressed batch used to get past it. le16 below checks the slice, so
	// reading the slot's second byte past the declared size is safe.
	if vt >= int(size) {
		return 0, false, nil
	}
	rel, ok := le16(t.b, vtable+vt)
	if !ok {
		return 0, false, errFrameShort
	}
	if rel == 0 {
		return 0, false, nil
	}
	pos = t.pos + int(rel)
	if pos < 0 || pos >= len(t.b) {
		return 0, false, errFrameShort
	}
	return pos, true, nil
}

// table follows an offset field to the table it points at.
func (t fbTable) table(vt int) (fbTable, bool, error) {
	p, present, err := t.field(vt)
	if err != nil || !present {
		return fbTable{}, false, err
	}
	rel, ok := le32(t.b, p)
	if !ok {
		return fbTable{}, false, errFrameShort
	}
	pos := p + int(rel)
	if pos < 0 || pos >= len(t.b) {
		return fbTable{}, false, errFrameShort
	}
	return fbTable{b: t.b, pos: pos}, true, nil
}

// vectorLen follows an offset field to a vector and reports how many elements it
// declares, or 0 when the field is absent.
func (t fbTable) vectorLen(vt int) (int, error) {
	p, present, err := t.field(vt)
	if err != nil || !present {
		return 0, err
	}
	rel, ok := le32(t.b, p)
	if !ok || uint64(rel) > uint64(len(t.b)-p) {
		return 0, errFrameShort
	}
	n, ok := le32(t.b, p+int(rel))
	if !ok || uint64(n) > uint64(len(t.b)) { // an element is at least a byte, so more than the slice holds is a lie
		return 0, errFrameShort
	}
	return int(n), nil
}

// The bounds are written as off > len(b)-n rather than off+n > len(b): on a
// 32-bit target (linux/386 and linux/arm are release builds) an offset near
// MaxInt32 makes off+n wrap negative, the comparison passes, and the slice
// expression panics. The offsets come from a header the sender wrote.
func le16(b []byte, off int) (uint16, bool) {
	if off < 0 || off > len(b)-2 {
		return 0, false
	}
	return binary.LittleEndian.Uint16(b[off:]), true
}

func le32(b []byte, off int) (uint32, bool) {
	if off < 0 || off > len(b)-4 {
		return 0, false
	}
	return binary.LittleEndian.Uint32(b[off:]), true
}
