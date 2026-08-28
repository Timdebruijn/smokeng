//go:build linux

package timestamp

import (
	"testing"
	"time"

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
	if !caps.KernelRX {
		t.Fatal("EnableKernel reported no kernel RX timestamping")
	}
	if !caps.KernelTX {
		t.Fatal("EnableKernel reported no kernel TX timestamping")
	}

	if err := unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO,
		&unix.Timeval{Sec: 2}); err != nil {
		t.Fatal(err)
	}

	msg := icmp.Message{
		Type: ipv4.ICMPTypeEcho,
		Body: &icmp.Echo{ID: 0, Seq: 1, Data: []byte("smokeng-timestamp-test")},
	}
	wire, err := msg.Marshal(nil)
	if err != nil {
		t.Fatal(err)
	}
	before := time.Now()
	if err := unix.Sendto(fd, wire, 0, &unix.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		t.Fatalf("sendto loopback: %v", err)
	}

	// TX timestamp: the kernel queues it on the error queue, so poll briefly.
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
		time.Sleep(10 * time.Millisecond)
	}
	if len(tx) == 0 {
		t.Fatal("no TX timestamp arrived on the error queue")
	}
	// OPT_ID counts from 0 for the first packet sent after EnableKernel.
	if tx[0].Counter != 0 {
		t.Errorf("TX counter = %d, want 0 for the first send", tx[0].Counter)
	}
	if tx[0].At.Before(before.Add(-time.Second)) || tx[0].At.After(time.Now().Add(time.Second)) {
		t.Errorf("TX timestamp %v is not near wall-clock now (%v)", tx[0].At, before)
	}

	// RX timestamp: read the echo reply and pull SCM_TIMESTAMPING off it.
	buf := make([]byte, 1500)
	oob := make([]byte, 512)
	n, oobn, _, _, err := unix.Recvmsg(fd, buf, oob, 0)
	if err != nil {
		t.Fatalf("recvmsg (no loopback echo reply?): %v", err)
	}
	if n == 0 {
		t.Fatal("empty reply")
	}
	rx, ok := FromOOB(oob[:oobn])
	if !ok {
		t.Fatal("no RX timestamp in the reply's control messages")
	}
	if rx.Before(tx[0].At) {
		t.Errorf("RX timestamp %v precedes TX timestamp %v", rx, tx[0].At)
	}
	if d := rx.Sub(tx[0].At); d > time.Second {
		t.Errorf("loopback RTT of %v is implausible; timestamps are probably wrong", d)
	}
	t.Logf("kernel-timestamped loopback RTT: %v", rx.Sub(tx[0].At))
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
