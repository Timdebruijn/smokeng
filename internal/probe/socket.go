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

	"smokeng/internal/probe/timestamp"
)

const (
	protoICMPv4 = 1
	protoICMPv6 = 58
	// payload = magic + token; pings smaller than this are padded up.
	minPayload = 12
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
}

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

	c := &conn{
		fd:        fd,
		family:    family,
		raw:       raw,
		caps:      timestamp.EnableKernel(fd),
		pending:   map[uint16]*pendingPing{},
		byCounter: map[uint32]*pendingPing{},
		late:      late,
	}
	var idBytes [2]byte
	rand.Read(idBytes[:])
	c.rawID = binary.BigEndian.Uint16(idBytes[:])

	c.wg.Add(1)
	go c.recvLoop()
	if c.caps.KernelTX {
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
		c.forgetSeq(seq)
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
			if err == unix.EAGAIN || err == unix.EWOULDBLOCK || err == unix.EINTR {
				continue
			}
			return // socket closed or fatal
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

// errQueueLoop drains kernel TX timestamps (Linux). The error queue never
// blocks, so this polls on a short ticker; timestamps only need to arrive
// before the bucket finalizes.
func (c *conn) errQueueLoop() {
	defer c.wg.Done()
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	for !c.closed.Load() {
		<-tick.C
		stamps, err := timestamp.ReadErrQueue(c.fd)
		if err != nil {
			continue
		}
		for _, s := range stamps {
			c.mu.Lock()
			p, ok := c.byCounter[s.Counter]
			if ok {
				delete(c.byCounter, s.Counter)
			}
			c.mu.Unlock()
			if ok {
				p.col.onTXKernel(p.idx, s.At)
			}
		}
	}
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
