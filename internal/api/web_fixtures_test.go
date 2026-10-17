package api

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/ipc"
	"github.com/apache/arrow-go/v18/arrow/memory"

	"github.com/timdebruijn/smokeng/internal/store"
)

// The frontend decodes this stream with a reader of its own, so its tests read
// what the real writer produces: web/testdata/*.arrow, written by
// writeMeasurementsArrow, with the values they hold in a .json beside each. This
// test regenerates both and fails if the committed files differ, so they cannot
// drift from the writer; run it with UPDATE_WEB_FIXTURES=1 to rewrite them.

type fixtureJSON struct {
	TS       []int64               `json:"ts"`
	Sent     []int                 `json:"sent"`
	Received []int                 `json:"received"`
	Flags    []int                 `json:"flags"`
	Samples  [][]uint32            `json:"samples"`
	ICMP     []*uint16             `json:"icmp_error"`
	SendErr  []*uint8              `json:"send_error"`
	Series   map[string][]*[]int32 `json:"series"`
	Batches  int                   `json:"batches"`
}

func fixtureMeasurements(rows int) []store.Measurement {
	icmp := func(v uint16) *uint16 { return &v }
	send := func(v uint8) *uint8 { return &v }
	var ms []store.Measurement
	for i := 0; i < rows; i++ {
		m := store.Measurement{
			TS: 1_756_400_000 + int64(i)*60, Sent: 20, Received: i % 21, Flags: uint8(i % 3),
			Samples: []uint32{1000 + uint32(i), 2000 + uint32(i), 3000 + uint32(i)}[:i%4],
		}
		if i%5 == 0 {
			m.ICMPErr = icmp(0x0303)
		}
		if i%7 == 0 {
			m.SendErr = send(2)
		}
		m.Series = map[string][]int32{}
		for j, name := range store.KnownSeries {
			if (i+j)%3 != 0 {
				m.Series[name] = []int32{-5, 0, 5}[:i%4]
			}
		}
		ms = append(ms, m)
	}
	return ms
}

// nullReceived is a stream the server never writes: a required column with a
// null in it. The reader has to refuse it rather than read the zero in the slot,
// which for `received` would be a hundred percent loss.
func nullReceived() []byte {
	var buf bytes.Buffer
	w := ipc.NewWriter(&buf, ipc.WithSchema(measurementsSchema))
	rb := array.NewRecordBuilder(memory.DefaultAllocator, measurementsSchema)
	defer rb.Release()
	for i := 0; i < 2; i++ {
		rb.Field(0).(*array.TimestampBuilder).Append(arrow.Timestamp(1_756_400_000 + int64(i)*60))
		rb.Field(1).(*array.Uint16Builder).Append(20)
		if i == 1 {
			rb.Field(2).(*array.Uint16Builder).AppendNull()
		} else {
			rb.Field(2).(*array.Uint16Builder).Append(18)
		}
		rb.Field(3).(*array.Uint8Builder).Append(0)
		l := rb.Field(4).(*array.ListBuilder)
		l.Append(true)
		l.ValueBuilder().(*array.Uint32Builder).AppendValues([]uint32{1000, 2000}, nil)
		rb.Field(5).(*array.Uint16Builder).AppendNull()
		rb.Field(6).(*array.Uint8Builder).AppendNull()
		for j := 0; j < len(store.KnownSeries); j++ {
			rb.Field(7 + j).(*array.ListBuilder).AppendNull()
		}
	}
	rec := rb.NewRecord()
	defer rec.Release()
	if err := w.Write(rec); err != nil {
		panic(err)
	}
	if err := w.Close(); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

// variant writes one row of a stream whose schema is measurementsSchema with
// one column changed, or one added, to something the server never writes. Every
// integer is 1 and every list is [1], so what a reader does with it depends on
// the schema alone.
func variant(change func(fields []arrow.Field) []arrow.Field) []byte {
	fields := append([]arrow.Field(nil), measurementsSchema.Fields()...)
	schema := arrow.NewSchema(change(fields), nil)
	var buf bytes.Buffer
	w := ipc.NewWriter(&buf, ipc.WithSchema(schema))
	rb := array.NewRecordBuilder(memory.DefaultAllocator, schema)
	defer rb.Release()
	for i := range schema.Fields() {
		switch b := rb.Field(i).(type) {
		case *array.TimestampBuilder:
			b.Append(1_756_400_000)
		case *array.Uint8Builder:
			b.Append(1)
		case *array.Uint16Builder:
			b.Append(1)
		case *array.Uint32Builder:
			b.Append(1)
		case *array.Uint64Builder:
			b.Append(1)
		case *array.Int8Builder:
			b.Append(1)
		case *array.Int16Builder:
			b.Append(1)
		case *array.Int32Builder:
			b.Append(1)
		case *array.Int64Builder:
			b.Append(1)
		case *array.Float32Builder:
			b.Append(1)
		case *array.StringBuilder:
			b.Append("x")
		case *array.BinaryDictionaryBuilder:
			if err := b.AppendString("x"); err != nil {
				panic(err)
			}
		case *array.ListBuilder:
			b.Append(true)
			switch v := b.ValueBuilder().(type) {
			case *array.Uint16Builder:
				v.Append(1)
			case *array.Uint32Builder:
				v.Append(1)
			case *array.Uint64Builder:
				v.Append(1)
			case *array.Int16Builder:
				v.Append(1)
			case *array.Int32Builder:
				v.Append(1)
			case *array.Int64Builder:
				v.Append(1)
			case *array.StructBuilder:
				v.Append(true)
				v.FieldBuilder(0).(*array.Int32Builder).Append(1)
			default:
				panic("variant: no filler for a list of this type")
			}
		default:
			panic("variant: no filler for this column type")
		}
	}
	rec := rb.NewRecord()
	defer rec.Release()
	if err := w.Write(rec); err != nil {
		panic(err)
	}
	if err := w.Close(); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

// withType replaces the type of the column called name.
func withType(name string, t arrow.DataType) func([]arrow.Field) []arrow.Field {
	return func(fields []arrow.Field) []arrow.Field {
		for i := range fields {
			if fields[i].Name == name {
				fields[i].Type = t
			}
		}
		return fields
	}
}

func variants() map[string][]byte {
	return map[string][]byte{
		// Fine: a column the reader has no use for is skipped, whatever it holds.
		"extra-int-column.arrow": variant(func(f []arrow.Field) []arrow.Field {
			return append(f, arrow.Field{Name: "zz", Type: arrow.PrimitiveTypes.Uint8})
		}),
		// Each of these is something the reader has to refuse, because it could not
		// put the values in the arrays it has without changing them.
		"samples-u64.arrow":     variant(withType("samples", arrow.ListOf(arrow.PrimitiveTypes.Uint64))),
		"samples-u16.arrow":     variant(withType("samples", arrow.ListOf(arrow.PrimitiveTypes.Uint16))),
		"samples-flat.arrow":    variant(withType("samples", arrow.PrimitiveTypes.Uint32)),
		"series-flat.arrow":     variant(withType("ipdv_send", arrow.PrimitiveTypes.Int32)),
		"samples-signed.arrow":  variant(withType("samples", arrow.ListOf(arrow.PrimitiveTypes.Int32))),
		"series-unsigned.arrow": variant(withType("ipdv_send", arrow.ListOf(arrow.PrimitiveTypes.Uint32))),
		"sent-u64.arrow":        variant(withType("sent", arrow.PrimitiveTypes.Uint64)),
		"sent-float.arrow":      variant(withType("sent", arrow.PrimitiveTypes.Float32)),
		"flags-u16.arrow":       variant(withType("flags", arrow.PrimitiveTypes.Uint16)),
		"flags-i8.arrow":        variant(withType("flags", arrow.PrimitiveTypes.Int8)),
		"series-i16.arrow":      variant(withType("ipdv_send", arrow.ListOf(arrow.PrimitiveTypes.Int16))),
		"extra-utf8.arrow": variant(func(f []arrow.Field) []arrow.Field {
			return append(f, arrow.Field{Name: "zz", Type: arrow.BinaryTypes.String})
		}),
		"extra-dictionary.arrow": variant(func(f []arrow.Field) []arrow.Field {
			return append(f, arrow.Field{Name: "zz", Type: &arrow.DictionaryType{
				IndexType: arrow.PrimitiveTypes.Int8, ValueType: arrow.BinaryTypes.String}})
		}),
		"samples-of-structs.arrow": variant(withType("samples",
			arrow.ListOf(arrow.StructOf(arrow.Field{Name: "a", Type: arrow.PrimitiveTypes.Int32})))),
		"ts-in-millis.arrow": variant(withType("ts", &arrow.TimestampType{Unit: arrow.Millisecond, TimeZone: "UTC"})),
	}
}

func fixtureFiles() map[string][]byte {
	files := map[string][]byte{"null-received.arrow": nullReceived()}
	for name, data := range variants() {
		files[name] = data
	}
	for _, f := range []struct {
		name      string
		rows      int
		batchRows int
		series    bool
	}{
		{"empty", 0, 8192, true},
		{"one", 1, 8192, false},
		{"small", 7, 8192, true},
		{"multi", 8, 3, true},    // three batches: 3, 3, 2
		{"wide", 20, 8192, true}, // validity bitmaps of three bytes
	} {
		ms := fixtureMeasurements(f.rows)
		if !f.series {
			for i := range ms {
				ms[i].Series = nil
			}
		}
		var buf bytes.Buffer
		writeMeasurementsArrow(&buf, ms, f.batchRows)
		files[f.name+".arrow"] = buf.Bytes()

		// Slices that are empty and not nil, so an empty stream reads as [] in the
		// JSON, not null.
		want := fixtureJSON{
			TS: []int64{}, Sent: []int{}, Received: []int{}, Flags: []int{}, Samples: [][]uint32{},
			ICMP: []*uint16{}, SendErr: []*uint8{}, Series: map[string][]*[]int32{},
		}
		for _, name := range store.KnownSeries {
			want.Series[name] = []*[]int32{}
		}
		want.Batches = (f.rows + f.batchRows - 1) / f.batchRows
		if f.rows == 0 {
			want.Batches = 1 // the empty batch that carries the schema
		}
		for _, m := range ms {
			want.TS = append(want.TS, m.TS)
			want.Sent = append(want.Sent, m.Sent)
			want.Received = append(want.Received, m.Received)
			want.Flags = append(want.Flags, int(m.Flags))
			want.Samples = append(want.Samples, append([]uint32{}, m.Samples...))
			want.ICMP = append(want.ICMP, m.ICMPErr)
			want.SendErr = append(want.SendErr, m.SendErr)
			for _, name := range store.KnownSeries {
				if v, ok := m.Series[name]; ok {
					v := append([]int32{}, v...)
					want.Series[name] = append(want.Series[name], &v)
				} else {
					want.Series[name] = append(want.Series[name], nil)
				}
			}
		}
		j, err := json.MarshalIndent(want, "", " ")
		if err != nil {
			panic(err)
		}
		files[f.name+".json"] = append(j, '\n')
	}
	return files
}

func TestWebFixturesAreWhatTheWriterProduces(t *testing.T) {
	dir := filepath.Join("..", "..", "web", "testdata")
	files := fixtureFiles()
	if os.Getenv("UPDATE_WEB_FIXTURES") != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for name, data := range files {
			if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return
	}
	for name, want := range files {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("%s: %v (run with UPDATE_WEB_FIXTURES=1)", name, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("web/testdata/%s is not what the writer produces now; run with UPDATE_WEB_FIXTURES=1", name)
		}
	}
}
