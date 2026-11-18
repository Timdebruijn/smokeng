package dnscache

import (
	"context"
	"encoding/binary"
	"net"
	"sync"
	"testing"
	"time"

	"codeberg.org/miekg/dns"
)

// fakeResolver answers A with 192.0.2.1 and AAAA with 2001:db8::1 for any name,
// each answer carrying its own query's ID, a little late so that queries sent
// together are in flight together. It is a correct resolver: whatever goes
// wrong against it goes wrong in the client.
func fakeResolver(t *testing.T) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			q := append([]byte(nil), buf[:n]...)
			go func() {
				// Skip the header and the question's name to find its type.
				i := 12
				for i < len(q) && q[i] != 0 {
					i += int(q[i]) + 1
				}
				if i+5 > len(q) {
					return
				}
				qtype := binary.BigEndian.Uint16(q[i+1:])
				r := append([]byte(nil), q[:i+5]...)
				r[2], r[3] = 0x81, 0x80 // a response, recursion available, NOERROR
				binary.BigEndian.PutUint16(r[6:], 1)
				binary.BigEndian.PutUint16(r[8:], 0)
				binary.BigEndian.PutUint16(r[10:], 0)
				r = append(r, 0xc0, 0x0c) // the question's name
				r = binary.BigEndian.AppendUint16(r, qtype)
				r = binary.BigEndian.AppendUint16(r, 1)
				r = binary.BigEndian.AppendUint32(r, 60)
				if qtype == dns.TypeAAAA {
					r = binary.BigEndian.AppendUint16(r, 16)
					r = append(r, net.ParseIP("2001:db8::1").To16()...)
				} else {
					r = binary.BigEndian.AppendUint16(r, 4)
					r = append(r, 192, 0, 2, 1)
				}
				time.Sleep(20 * time.Millisecond)
				pc.WriteTo(r, from)
			}()
		}
	}()
	return pc.LocalAddr().String()
}

// Protects lookups that run at the same moment, which is every interval for
// the two families of one name and at every start for all names at once. Each
// must get its own answer.
func TestConcurrentLookupsEachGetTheirOwnAnswer(t *testing.T) {
	// Built by hand rather than with New, which needs a system resolver
	// configuration and would have this test skipped where there is none.
	c := &Cache{servers: []string{fakeResolver(t)}, exchange: exchangeUDP, entries: map[key]*entry{}}

	const rounds, each = 20, 8
	var wg sync.WaitGroup
	errs := make(chan error, rounds*each)
	for range rounds {
		for i := range each {
			family := "v4"
			if i%2 == 1 {
				family = "v6"
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				addr, _, err := c.resolve(ctx, "name.example.", family)
				if err != nil {
					errs <- err
					return
				}
				if want := map[string]string{"v4": "192.0.2.1", "v6": "2001:db8::1"}[family]; addr.String() != want {
					t.Errorf("%s lookup got %s, want %s", family, addr, want)
				}
			}()
		}
		wg.Wait()
	}
	close(errs)
	n := 0
	for err := range errs {
		if n++; n <= 3 {
			t.Errorf("lookup failed: %v", err)
		}
	}
	if n > 0 {
		t.Errorf("%d of %d concurrent lookups failed", n, rounds*each)
	}
}
