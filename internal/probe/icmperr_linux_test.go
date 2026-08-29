//go:build linux

package probe

import (
	"os/exec"
	"testing"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/sys/unix"

	"smokeng/internal/probe/timestamp"
)

// TestICMPErrorReachesTheErrorQueue provokes a real ICMP error and follows it
// all the way through: onto the error queue, back out with the offending
// datagram attached, and correlated to the exact ping by sequence number.
//
// The correlation is worth testing rather than assuming: it only works
// because the kernel returns our own echo request as the error's payload.
// The test installs a firewall rule, so it needs root and cleans up after
// itself.
func TestICMPErrorReachesTheErrorQueue(t *testing.T) {
	const victim = "192.0.2.1" // TEST-NET-1, safe to blackhole
	add := exec.Command("iptables", "-I", "OUTPUT", "-d", victim, "-p", "icmp",
		"-j", "REJECT", "--reject-with", "icmp-host-prohibited")
	if out, err := add.CombinedOutput(); err != nil {
		t.Skipf("cannot install the iptables rule (needs root): %v: %s", err, out)
	}
	t.Cleanup(func() {
		exec.Command("iptables", "-D", "OUTPUT", "-d", victim, "-p", "icmp",
			"-j", "REJECT", "--reject-with", "icmp-host-prohibited").Run()
	})

	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, unix.IPPROTO_ICMP)
	if err != nil {
		t.Skipf("datagram ICMP socket not permitted: %v", err)
	}
	defer unix.Close(fd)
	if err := unix.Bind(fd, &unix.SockaddrInet4{}); err != nil {
		t.Fatal(err)
	}
	if !timestamp.EnableICMPErrors(fd, false) {
		t.Skip("IP_RECVERR unavailable")
	}

	const wantSeq = 4242
	msg := icmp.Message{
		Type: ipv4.ICMPTypeEcho,
		Body: &icmp.Echo{ID: 0, Seq: wantSeq, Data: []byte("smokeng-icmp-error-probe")},
	}
	wire, err := msg.Marshal(nil)
	if err != nil {
		t.Fatal(err)
	}
	// The send itself may be refused locally; the error still lands on the
	// queue, which is the path a remote router's error also takes.
	_ = unix.Sendto(fd, wire, 0, &unix.SockaddrInet4{Addr: [4]byte{192, 0, 2, 1}})

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		entries, err := timestamp.ReadErrQueue(fd)
		if err != nil {
			t.Fatalf("ReadErrQueue: %v", err)
		}
		for _, e := range entries {
			if e.ICMPError == nil {
				continue
			}
			// ICMP type 3 code 10: host administratively prohibited.
			if e.ICMPError.Type != 3 || e.ICMPError.Code != 10 {
				t.Errorf("got ICMP type %d code %d, want 3/10",
					e.ICMPError.Type, e.ICMPError.Code)
			}
			parsed, perr := icmp.ParseMessage(protoICMPv4, e.ICMPError.Payload)
			if perr != nil {
				t.Fatalf("payload does not parse as ICMP: %v", perr)
			}
			echo, ok := parsed.Body.(*icmp.Echo)
			if !ok {
				t.Fatalf("payload is not an echo request: %T", parsed.Body)
			}
			if echo.Seq != wantSeq {
				t.Fatalf("payload seq = %d, want %d; errors cannot be attributed to a ping",
					echo.Seq, wantSeq)
			}
			t.Logf("ICMP type %d code %d attributed to seq %d",
				e.ICMPError.Type, e.ICMPError.Code, echo.Seq)
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("no ICMP error arrived on the error queue")
}
