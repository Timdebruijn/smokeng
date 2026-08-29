//go:build linux

package probe

import (
	"testing"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/sys/unix"
)

// TestDropsAreDetected forces the very condition the flag exists for: a
// receive queue too small for the replies arriving, so the kernel discards
// them. Without a working counter those drops are indistinguishable from
// network packet loss, and a busy host reads as a lossy link.
//
// This test is why the SO_RXQ_OVFL approach was abandoned: it was accepted by
// setsockopt and then reported nothing, which no amount of checking the
// setsockopt return value would have revealed.
func TestDropsAreDetected(t *testing.T) {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, unix.IPPROTO_ICMP)
	if err != nil {
		t.Skipf("datagram ICMP socket not permitted (see net.ipv4.ping_group_range): %v", err)
	}
	defer unix.Close(fd)

	// A ping socket only joins the kernel's table — and so /proc/net/icmp —
	// once bound, which is why openConn binds too.
	if err := unix.Bind(fd, &unix.SockaddrInet4{}); err != nil {
		t.Fatal(err)
	}
	inode, ok := socketInode(fd)
	if !ok {
		t.Skip("cannot resolve the socket inode from /proc/self/fd")
	}
	path := procNetPath("v4", false)
	before, ok := readSocketDrops(path, inode)
	if !ok {
		t.Skipf("socket not listed in %s", path)
	}

	// Shrink the queue to its floor, then send far more than it can hold.
	if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RCVBUF, 1024); err != nil {
		t.Fatal(err)
	}
	payload := make([]byte, 1024) // large packets fill the queue faster
	dst := &unix.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}
	for seq := 1; seq <= 2000; seq++ {
		msg := icmp.Message{
			Type: ipv4.ICMPTypeEcho,
			Body: &icmp.Echo{ID: 0, Seq: seq, Data: payload},
		}
		wire, err := msg.Marshal(nil)
		if err != nil {
			t.Fatal(err)
		}
		// Deliberately never read, so the replies pile up and overflow.
		_ = unix.Sendto(fd, wire, 0, dst)
	}
	time.Sleep(300 * time.Millisecond)

	after, ok := readSocketDrops(path, inode)
	if !ok {
		t.Fatalf("socket vanished from %s", path)
	}
	t.Logf("drops before=%d after=%d", before, after)
	if after <= before {
		t.Fatal("no drops reported after overrunning the receive queue; " +
			"overflow detection would stay silent exactly when it matters")
	}
}

// The drop counter must be found for a normally configured socket, or
// FlagSocketOverflow can never be set in production.
func TestSocketIsListedInProcNet(t *testing.T) {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, unix.IPPROTO_ICMP)
	if err != nil {
		t.Skipf("datagram ICMP socket not permitted: %v", err)
	}
	defer unix.Close(fd)
	if err := unix.Bind(fd, &unix.SockaddrInet4{}); err != nil {
		t.Fatal(err)
	}

	inode, ok := socketInode(fd)
	if !ok {
		t.Fatal("could not resolve the socket inode")
	}
	if _, ok := readSocketDrops(procNetPath("v4", false), inode); !ok {
		t.Fatalf("socket inode %s not found in %s", inode, procNetPath("v4", false))
	}
}
