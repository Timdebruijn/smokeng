package probe

import (
	"context"
	"net"
	"net/netip"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/heistp/irtt"

	"github.com/timdebruijn/smokeng/internal/store"
)

// irttServerWithKey starts an irtt server that requires the given HMAC key.
func irttServerWithKey(t *testing.T, key []byte) int {
	t.Helper()
	c, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := c.LocalAddr().(*net.UDPAddr).Port
	c.Close()

	cfg := irtt.NewServerConfig()
	cfg.Addrs = []string{net.JoinHostPort("127.0.0.1", strconv.Itoa(port))}
	// Its own filler, not the package-level DefaultServerFiller every
	// ServerConfig points at by default. That filler keeps an unsynchronised
	// position across Read calls, so two test servers alive at once — and
	// Shutdown does not wait for in-flight connection goroutines, so they
	// overlap — race on it. The race is in the library and only its server
	// side, which smokeng never runs in production; but it made `go test
	// -race` unusable for this package, and it aborted the one end-to-end test
	// written to catch a regression in the series recorder before that test
	// checked anything.
	cfg.Filler = irtt.NewDefaultPatternFiller()
	cfg.HMACKey = key
	ready := make(chan struct{})
	cfg.Handler = handlerFunc(func(e *irtt.Event) {
		if e.Code == irtt.ListenerStart {
			select {
			case <-ready:
			default:
				close(ready)
			}
		}
	})
	srv := irtt.NewServer(cfg)
	go func() { _ = srv.ListenAndServe() }()
	t.Cleanup(srv.Shutdown)
	select {
	case <-ready:
	case <-time.After(10 * time.Second):
		t.Fatal("irtt server never reported a listener")
	}
	return port
}

func irttOnce(t *testing.T, port int) store.Measurement {
	t.Helper()
	spec := TargetSpec{
		TargetID: 1, Host: "127.0.0.1", Family: "v4",
		IntervalS: 10, Pings: 4, Mode: "burst", BurstGapMS: 20,
		TimeoutMS: 1000, PacketSize: 64,
		ProbeType: "irtt", ProbePort: port,
	}
	var late atomic.Int64
	col := newCollector(spec.Pings, &late)
	probeIRTT(context.Background(), col, netip.MustParseAddr("127.0.0.1"), spec,
		time.Now().Add(3*time.Second))
	return col.finalize(spec, 0, conditions{})
}

// With the right key, an HMAC-required server measures normally. Every irtt
// test manipulates the process-wide key map, so each restores it.
func TestIRTTHMACMatchingKeyMeasures(t *testing.T) {
	t.Cleanup(func() { SetIRTTHMACKeys(nil) })
	key := []byte("shared-secret")
	port := irttServerWithKey(t, key)
	SetIRTTHMACKeys(map[string][]byte{
		net.JoinHostPort("127.0.0.1", strconv.Itoa(port)): key,
	})
	m := irttOnce(t, port)
	if m.Received == 0 {
		t.Fatalf("got %d/%d; an HMAC-required server rejected the correct key", m.Received, m.Sent)
	}
}

// Without a key, a server that requires one refuses the session — recorded as
// a flagged send failure, not clean loss. This is the whole point: an open
// server is closed to everyone but the holder of the key.
func TestIRTTHMACNoKeyIsRefused(t *testing.T) {
	quietLog(t)
	t.Cleanup(func() { SetIRTTHMACKeys(nil) })
	SetIRTTHMACKeys(nil) // this prober holds no key
	port := irttServerWithKey(t, []byte("shared-secret"))
	m := irttOnce(t, port)
	if m.Received != 0 {
		t.Fatalf("got %d received; a keyless prober measured an HMAC-required server", m.Received)
	}
	if m.Flags&store.FlagSendFailed == 0 {
		t.Fatalf("flags 0x%x: a refused session must read as a send failure", m.Flags)
	}
}

// The wrong key is refused just as a missing one is.
func TestIRTTHMACWrongKeyIsRefused(t *testing.T) {
	quietLog(t)
	t.Cleanup(func() { SetIRTTHMACKeys(nil) })
	port := irttServerWithKey(t, []byte("the-right-secret"))
	SetIRTTHMACKeys(map[string][]byte{
		net.JoinHostPort("127.0.0.1", strconv.Itoa(port)): []byte("the-wrong-secret"),
	})
	m := irttOnce(t, port)
	if m.Received != 0 {
		t.Fatalf("got %d received; the wrong HMAC key was accepted", m.Received)
	}
}
