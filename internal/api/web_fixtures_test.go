package api

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

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

func fixtureFiles() map[string][]byte {
	files := map[string][]byte{}
	for _, f := range []struct {
		name      string
		rows      int
		batchRows int
		series    bool
	}{
		{"empty", 0, 8192, true},
		{"one", 1, 8192, false},
		{"small", 7, 8192, true},
		{"multi", 8, 3, true}, // three batches: 3, 3, 2
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

		want := fixtureJSON{Series: map[string][]*[]int32{}}
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
