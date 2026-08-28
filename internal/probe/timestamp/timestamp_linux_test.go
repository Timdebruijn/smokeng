//go:build linux

package timestamp

import (
	"fmt"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/sys/unix"
)

// TestKernelTimestampingRoundTrip exercises the whole Linux path against the
// loopback interface: enable SO_TIMESTAMPING on an unprivileged datagram ICMP
// socket, send one echo request, and read back both a TX timestamp from the
// socket error queue and an RX timestamp from the reply's control messages.
// This is the code that cannot be verified anywhere but on Linux.
func TestKernelTimestampingRoundTrip(t *testing.T) {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, unix.IPPROTO_ICMP)
	if err != nil {
		t.Skipf("datagram ICMP socket not permitted (see net.ipv4.ping_group_range): %v", err)
	}
	defer unix.Close(fd)

	caps := EnableKernel(fd)
	if !caps.KernelRX || !caps.KernelTX {
		// A capability the environment does not offer is a skip, not a
		// failure — and smokeng itself records the degradation per
		// measurement, so this never passes silently in production. Report
		// the kernel's own reason: "no timestamping" is not a diagnosis.
		// (qemu-user, for one, answers ENOPROTOOPT here.)
		err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_TIMESTAMPING, tsFlags)
		t.Skipf("kernel timestamping unavailable here: EnableKernel = %+v, setsockopt(SO_TIMESTAMPING) says %v", caps, err)
	}

	if err := unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO,
		&unix.Timeval{Sec: 2}); err != nil {
		t.Fatal(err)
	}

	// A single packet is a poor oracle. Linux gates RX timestamping behind a
	// static key that goes cold when nothing has asked for timestamps for a
	// while; the first packets after that reliably arrive with no
	// SCM_TIMESTAMPING at all, and only then does the key take effect. The
	// prober handles this by design — those measurements get FlagUserspaceRX
	// — so the test asserts that the mechanism works, spacing its retries
	// widely enough to outlast the key's propagation rather than firing all
	// of them inside the same window.
	const attempts = 5
	var lastReason string
	for attempt := range attempts {
		if attempt > 0 {
			time.Sleep(100 * time.Millisecond)
		}
		msg := icmp.Message{
			Type: ipv4.ICMPTypeEcho,
			Body: &icmp.Echo{ID: 0, Seq: attempt + 1, Data: []byte("smokeng-timestamp-test")},
		}
		wire, err := msg.Marshal(nil)
		if err != nil {
			t.Fatal(err)
		}
		before := time.Now()
		if err := unix.Sendto(fd, wire, 0, &unix.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
			t.Fatalf("sendto loopback: %v", err)
		}

		// TX timestamp: the kernel queues it on the error queue, so poll.
		var tx []TXStamp
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			stamps, err := ReadErrQueue(fd)
			if err != nil {
				t.Fatalf("ReadErrQueue: %v", err)
			}
			tx = append(tx, stamps...)
			if len(tx) > 0 {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		if len(tx) == 0 {
			lastReason = "no TX timestamp arrived on the error queue"
			continue
		}
		// OPT_ID counts sends since EnableKernel, so it tracks the attempt.
		if got := tx[0].Counter; got != uint32(attempt) {
			t.Errorf("TX counter = %d, want %d on attempt %d", got, attempt, attempt)
		}
		if tx[0].At.Before(before.Add(-time.Second)) || tx[0].At.After(time.Now().Add(time.Second)) {
			t.Errorf("TX timestamp %v is not near wall-clock now (%v)", tx[0].At, before)
		}

		// RX timestamp: read the echo reply and pull SCM_TIMESTAMPING off it.
		buf := make([]byte, 1500)
		oob := make([]byte, 512)
		n, oobn, _, _, err := unix.Recvmsg(fd, buf, oob, 0)
		if err != nil {
			lastReason = "no loopback echo reply: " + err.Error()
			continue
		}
		if n == 0 {
			lastReason = "empty reply"
			continue
		}
		rx, ok := FromOOB(oob[:oobn])
		if !ok {
			lastReason = "no RX timestamp in the reply's control messages: " + describeCmsgs(oob[:oobn])
			continue
		}
		if rx.Before(tx[0].At) {
			t.Errorf("RX timestamp %v precedes TX timestamp %v", rx, tx[0].At)
		}
		if d := rx.Sub(tx[0].At); d > time.Second {
			t.Errorf("loopback RTT of %v is implausible; timestamps are probably wrong", d)
		}
		t.Logf("kernel-timestamped loopback RTT: %v (attempt %d)", rx.Sub(tx[0].At), attempt+1)
		return
	}
	t.Fatalf("no kernel-timestamped round trip in %d attempts; last: %s", attempts, lastReason)
}

// describeCmsgs renders received control messages for diagnosis: which ones
// arrived, and what the timestamp slots actually held.
func describeCmsgs(oob []byte) string {
	cmsgs, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		return fmt.Sprintf("%d oob bytes, unparseable: %v", len(oob), err)
	}
	if len(cmsgs) == 0 {
		return fmt.Sprintf("%d oob bytes, no control messages", len(oob))
	}
	parts := make([]string, 0, len(cmsgs))
	for _, m := range cmsgs {
		p := fmt.Sprintf("level=%d type=%d len=%d", m.Header.Level, m.Header.Type, len(m.Data))
		if m.Header.Level == unix.SOL_SOCKET && m.Header.Type == unix.SCM_TIMESTAMPING &&
			len(m.Data) >= int(unsafe.Sizeof(scmTimestamping{})) {
			ts := (*scmTimestamping)(unsafe.Pointer(&m.Data[0])).TS
			p += fmt.Sprintf(" slots=[sw:%d.%09d legacy:%d.%09d hw:%d.%09d]",
				ts[0].Sec, ts[0].Nsec, ts[1].Sec, ts[1].Nsec, ts[2].Sec, ts[2].Nsec)
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, "; ")
}

// TestReadErrQueueEmpty verifies the non-blocking drain returns cleanly when
// nothing is queued — the common case on every poll tick.
func TestReadErrQueueEmpty(t *testing.T) {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, unix.IPPROTO_ICMP)
	if err != nil {
		t.Skipf("datagram ICMP socket not permitted: %v", err)
	}
	defer unix.Close(fd)
	EnableKernel(fd)
	stamps, err := ReadErrQueue(fd)
	if err != nil {
		t.Fatalf("ReadErrQueue on an idle socket: %v", err)
	}
	if len(stamps) != 0 {
		t.Fatalf("got %d stamps from an idle socket", len(stamps))
	}
}
