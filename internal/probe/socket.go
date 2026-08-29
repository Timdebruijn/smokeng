package probe

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"log"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
	"golang.org/x/sys/unix"

	"github.com/timdebruijn/smokeng/internal/probe/timestamp"
)

const (
	protoICMPv4 = 1
	protoICMPv6 = 58
	// payload = magic + token; pings smaller than this are padded up.
	minPayload = 12
	// One socket carries every target of a family, and bursts from many
	// targets can land at once: 100 targets × 20 replies is ~2000 packets in
	// a few milliseconds, which overruns the usual ~200KB default. Ask for
	// enough headroom that overflow means something is genuinely wrong.
	wantRcvBuf = 4 << 20
)

var payloadMagic = [4]byte{'s', 'm', 'n', 'g'}

// conn is one ICMP socket for a (family, dscp) pair, shared by all targets
// with that combination (DESIGN.md §5.1). Datagram ICMP first; raw-socket
// fallback is recorded so measurements carry the degradation flag.
type conn struct {
	fd     int
	family string
	raw    bool
	caps   timestamp.Caps
	rawID  uint16 // our echo ID when raw (kernel owns it in datagram mode)

	mu        sync.Mutex
	seq       uint16
	txCount   uint32
	pending   map[uint16]*pendingPing
	byCounter map[uint32]*pendingPing

	closed atomic.Bool
	late   *atomic.Int64
	strays atomic.Int64
	wg     sync.WaitGroup

	// Cumulative kernel drop counter, polled from /proc/net. Measurements
	// taken while this moves carry FlagSocketOverflow.
	rxDrops   atomic.Uint64
	dropsPath string
	dropsNode string

	reportsICMPErrors bool
	icmpErrors        atomic.Int64
}

// ICMPErrors counts ICMP errors attributed to our pings on this socket.
func (c *conn) ICMPErrors() int64 { return c.icmpErrors.Load() }

// drops returns the cumulative count of replies the kernel discarded because
// our receive queue was full. Always 0 where the kernel cannot report it.
func (c *conn) drops() uint64 { return c.rxDrops.Load() }

type pendingPing struct {
	col     *collector
	idx     int
	counter uint32
}

func openConn(family string, dscp int, late *atomic.Int64) (*conn, error) {
	domain, proto := unix.AF_INET, unix.IPPROTO_ICMP
	if family == "v6" {
		domain, proto = unix.AF_INET6, unix.IPPROTO_ICMPV6
	}
	raw := false
	fd, err := unix.Socket(domain, unix.SOCK_DGRAM, proto)
	if err != nil {
		fd, err = unix.Socket(domain, unix.SOCK_RAW, proto)
		if err != nil {
			return nil, fmt.Errorf(
				"probe: cannot open %s ICMP socket (datagram and raw both failed): %w — "+
					"on Linux, allow unprivileged ping sockets with: sysctl -w net.ipv4.ping_group_range=\"0 2147483647\"",
				family, err)
		}
		raw = true
		log.Printf("probe: %s datagram ICMP socket not permitted, using raw-socket fallback (degraded, flagged)", family)
	}

	// Bind explicitly. A datagram ICMP socket is otherwise only added to the
	// kernel's ping table on its first send, and until then it does not
	// appear in /proc/net/icmp — which is where the drop counter lives. For
	// these sockets the "port" is the ICMP identifier; 0 lets the kernel
	// assign one.
	var bindAddr unix.Sockaddr = &unix.SockaddrInet4{}
	if family == "v6" {
		bindAddr = &unix.SockaddrInet6{}
	}
	if err := unix.Bind(fd, bindAddr); err != nil && !raw {
		unix.Close(fd)
		return nil, fmt.Errorf("probe: bind %s ICMP socket: %w", family, err)
	}

	if dscp != 0 {
		tos := dscp << 2
		var soErr error
		if family == "v6" {
			soErr = unix.SetsockoptInt(fd, unix.IPPROTO_IPV6, unix.IPV6_TCLASS, tos)
		} else {
			soErr = unix.SetsockoptInt(fd, unix.IPPROTO_IP, unix.IP_TOS, tos)
		}
		if soErr != nil {
			unix.Close(fd)
			return nil, fmt.Errorf("probe: set DSCP %d on %s socket: %w", dscp, family, soErr)
		}
	}
	// 1s receive timeout: the receive loop wakes to check for shutdown.
	if err := unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO,
		&unix.Timeval{Sec: 1}); err != nil {
		unix.Close(fd)
		return nil, err
	}

	// Enlarge the receive queue, then read back what the kernel granted: it
	// silently caps at net.core.rmem_max, and an operator needs to know when
	// the ceiling is lower than the load warrants.
	_ = unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RCVBUF, wantRcvBuf)
	if got, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RCVBUF); err == nil && got < wantRcvBuf {
		log.Printf("probe: %s socket receive buffer is %d bytes, asked for %d "+
			"(raise net.core.rmem_max to allow more); replies may be dropped under load",
			family, got, wantRcvBuf)
	}

	c := &conn{
		fd:        fd,
		family:    family,
		raw:       raw,
		caps:      timestamp.EnableKernel(fd),
		pending:   map[uint16]*pendingPing{},
		byCounter: map[uint32]*pendingPing{},
		late:      late,
	}
	if inode, ok := socketInode(fd); ok {
		path := procNetPath(family, raw)
		if _, ok := readSocketDrops(path, inode); ok {
			c.dropsPath, c.dropsNode = path, inode
		}
	}
	if c.dropsPath == "" {
		log.Printf("probe: %s socket drop counter unavailable; loss on this socket "+
			"may include replies we failed to read in time, and cannot be told apart", family)
	} else {
		c.wg.Add(1)
		go c.dropPollLoop()
	}
	var idBytes [2]byte
	rand.Read(idBytes[:])
	c.rawID = binary.BigEndian.Uint16(idBytes[:])

	// Routes ICMP errors for our packets onto the error queue instead of
	// letting them be discarded, so a refused probe is distinguishable from
	// an unanswered one.
	c.reportsICMPErrors = timestamp.EnableICMPErrors(fd, family == "v6")

	c.wg.Add(1)
	go c.recvLoop()
	if c.caps.KernelTX || c.reportsICMPErrors {
		c.wg.Add(1)
		go c.errQueueLoop()
	}
	return c, nil
}

func (c *conn) close() {
	if c.closed.Swap(true) {
		return
	}
	unix.Close(c.fd)
	c.wg.Wait()
}

// send transmits one echo request for ping idx of col. The userspace TX
// timestamp is always recorded; the kernel one supersedes it when the error
// queue delivers it (Linux).
func (c *conn) send(col *collector, idx int, dst netip.Addr, packetSize int) error {
	token := col.pings[idx].token
	size := max(packetSize, minPayload)
	payload := make([]byte, size)
	copy(payload, payloadMagic[:])
	copy(payload[4:], token[:])

	c.mu.Lock()
	// Allocate a free sequence number (65536 in-flight is far beyond real load).
	for range 65536 {
		c.seq++
		if _, taken := c.pending[c.seq]; !taken {
			break
		}
	}
	seq := c.seq
	// The counter must track the kernel's sk_tskey, which it increments per
	// packet it actually queues. Anything that fails before the kernel is
	// reached must not advance it, or every later stamp lands on the wrong
	// ping — see the marshal path below, which rolls it back.
	p := &pendingPing{col: col, idx: idx, counter: c.txCount}
	c.pending[seq] = p
	if c.caps.KernelTX {
		c.byCounter[c.txCount] = p
	}
	c.txCount++
	c.mu.Unlock()

	var typ icmp.Type = ipv4.ICMPTypeEcho
	if c.family == "v6" {
		typ = ipv6.ICMPTypeEchoRequest
	}
	msg := icmp.Message{
		Type: typ,
		Body: &icmp.Echo{ID: int(c.rawID), Seq: int(seq), Data: payload},
	}
	wire, err := msg.Marshal(nil)
	if err != nil {
		// The socket was never touched, so the kernel's counter did not move
		// and ours must not either.
		c.forgetSeqAndCounter(seq)
		return err
	}

	var sa unix.Sockaddr
	if c.family == "v6" {
		sa = &unix.SockaddrInet6{Addr: dst.As16()}
	} else {
		sa = &unix.SockaddrInet4{Addr: dst.As4()}
	}
	col.markSent(idx, seq, time.Now())
	if err := unix.Sendto(c.fd, wire, 0, sa); err != nil {
		// Deliberately does not wind the counter back. Some errors are
		// returned before the kernel assigns a timestamp id and some after —
		// ENOBUFS at the queue has already consumed one — so guessing would be
		// wrong half the time. finalize() checks each kernel stamp against the
		// ping it is attributed to instead, which catches a drift from any
		// cause and degrades to userspace timestamps, flagged, rather than
		// reporting a confident wrong number.
		c.forgetSeq(seq)
		col.markSendFailed(idx)
		return err
	}
	return nil
}

func (c *conn) recvLoop() {
	defer c.wg.Done()
	buf := make([]byte, 65536)
	oob := make([]byte, 512)
	for !c.closed.Load() {
		var n int
		var err error
		rx := time.Time{}
		kernel := false
		if c.caps.KernelRX {
			var oobn int
			n, oobn, _, _, err = unix.Recvmsg(c.fd, buf, oob, 0)
			if err == nil {
				rx, kernel = timestamp.FromOOB(oob[:oobn])
			}
		} else {
			n, _, err = unix.Recvfrom(c.fd, buf, 0)
		}
		if err != nil {
			// With IP_RECVERR enabled the kernel reports pending ICMP errors
			// here too (EHOSTUNREACH and friends). Those are data about a
			// probe, not a broken socket, so only a closed descriptor ends
			// the loop — anything else would silently stop all measurement.
			if err == unix.EBADF {
				return
			}
			continue
		}
		if !kernel {
			rx = time.Now()
		}
		c.handlePacket(buf[:n], rx, kernel)
	}
}

func (c *conn) handlePacket(b []byte, rx time.Time, kernel bool) {
	proto := protoICMPv4
	if c.family == "v6" {
		proto = protoICMPv6
	} else if len(b) > 0 && b[0]>>4 == 4 {
		// Raw v4 sockets (and macOS datagram sockets) deliver the IP header;
		// an ICMP echo reply's first byte (type 0) can never look like 0x4x.
		ihl := int(b[0]&0x0f) * 4
		if len(b) < ihl {
			return
		}
		b = b[ihl:]
	}
	msg, err := icmp.ParseMessage(proto, b)
	if err != nil {
		return
	}
	if msg.Type != ipv4.ICMPTypeEchoReply && msg.Type != ipv6.ICMPTypeEchoReply {
		return // TimeExceeded/Unreachable etc.: the ping simply stays lost
	}
	echo, ok := msg.Body.(*icmp.Echo)
	if !ok || len(echo.Data) < minPayload {
		c.strays.Add(1)
		return
	}
	if c.raw && uint16(echo.ID) != c.rawID {
		return // raw sockets see every process's replies
	}

	c.mu.Lock()
	p, ok := c.pending[uint16(echo.Seq)]
	if ok {
		delete(c.pending, uint16(echo.Seq))
	}
	c.mu.Unlock()
	if !ok {
		c.late.Add(1)
		return
	}
	if [4]byte(echo.Data[:4]) != payloadMagic ||
		[8]byte(echo.Data[4:12]) != p.col.pings[p.idx].token {
		c.strays.Add(1)
		return
	}
	p.col.onRX(p.idx, rx, kernel)
}

// dropPollLoop keeps the socket's drop counter fresh. Polling beats reading
// it per bucket: one file read per socket per tick serves every target on
// that socket, however many there are.
func (c *conn) dropPollLoop() {
	defer c.wg.Done()
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for !c.closed.Load() {
		<-tick.C
		if drops, ok := readSocketDrops(c.dropsPath, c.dropsNode); ok {
			c.rxDrops.Store(drops)
		}
	}
}

// errQueueLoop drains kernel TX timestamps (Linux). The error queue never
// blocks, so this polls on a short ticker; timestamps only need to arrive
// before the bucket finalizes.
func (c *conn) errQueueLoop() {
	defer c.wg.Done()
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	for !c.closed.Load() {
		<-tick.C
		entries, err := timestamp.ReadErrQueue(c.fd)
		if err != nil {
			continue
		}
		for _, e := range entries {
			switch {
			case e.TXStamp != nil:
				c.mu.Lock()
				p, ok := c.byCounter[e.TXStamp.Counter]
				if ok {
					delete(c.byCounter, e.TXStamp.Counter)
				}
				c.mu.Unlock()
				if ok {
					p.col.onTXKernel(p.idx, e.TXStamp.At)
				}
			case e.ICMPError != nil:
				c.handleICMPError(e.ICMPError)
			}
		}
	}
}

// handleICMPError attributes an ICMP error to the ping that provoked it. The
// error queue returns the offending datagram — for a ping socket, our own
// echo request — so its sequence number identifies the ping exactly.
func (c *conn) handleICMPError(e *timestamp.ICMPError) {
	proto := protoICMPv4
	if c.family == "v6" {
		proto = protoICMPv6
	}
	msg, err := icmp.ParseMessage(proto, e.Payload)
	if err != nil {
		return
	}
	echo, ok := msg.Body.(*icmp.Echo)
	if !ok {
		return
	}
	c.mu.Lock()
	p, ok := c.pending[uint16(echo.Seq)]
	if ok {
		delete(c.pending, uint16(echo.Seq))
		delete(c.byCounter, p.counter)
	}
	c.mu.Unlock()
	if !ok {
		return // already timed out, or not ours
	}
	c.icmpErrors.Add(1)
	p.col.onICMPError(p.idx, e.Type, e.Code)
}

// forgetSeqAndCounter drops a ping that never reached the kernel, winding the
// transmit counter back so it keeps step with sk_tskey. Safe only while no
// other send has been allocated since — which holds here, because the caller
// is still inside the same send and the counter is only advanced under the
// lock this takes.
func (c *conn) forgetSeqAndCounter(seq uint16) {
	c.mu.Lock()
	if p, ok := c.pending[seq]; ok {
		delete(c.pending, seq)
		delete(c.byCounter, p.counter)
		if p.counter == c.txCount-1 {
			c.txCount--
		}
	}
	c.mu.Unlock()
}

func (c *conn) forgetSeq(seq uint16) {
	c.mu.Lock()
	if p, ok := c.pending[seq]; ok {
		delete(c.pending, seq)
		delete(c.byCounter, p.counter)
	}
	c.mu.Unlock()
}

// forget drops any state still held for a finalized collector.
func (c *conn) forget(col *collector) {
	c.mu.Lock()
	for seq, p := range c.pending {
		if p.col == col {
			delete(c.pending, seq)
			delete(c.byCounter, p.counter)
		}
	}
	for counter, p := range c.byCounter {
		if p.col == col {
			delete(c.byCounter, counter)
		}
	}
	c.mu.Unlock()
}
