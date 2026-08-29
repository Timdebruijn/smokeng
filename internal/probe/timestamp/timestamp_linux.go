//go:build linux

package timestamp

import (
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

const tsFlags = unix.SOF_TIMESTAMPING_RX_SOFTWARE |
	unix.SOF_TIMESTAMPING_TX_SOFTWARE |
	unix.SOF_TIMESTAMPING_SOFTWARE |
	unix.SOF_TIMESTAMPING_OPT_ID |
	unix.SOF_TIMESTAMPING_OPT_TSONLY

func enableKernel(fd int) Caps {
	if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_TIMESTAMPING, tsFlags); err == nil {
		return Caps{KernelRX: true, KernelTX: true}
	}
	// Try RX-only before giving up entirely.
	rx := unix.SOF_TIMESTAMPING_RX_SOFTWARE | unix.SOF_TIMESTAMPING_SOFTWARE
	if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_TIMESTAMPING, rx); err == nil {
		return Caps{KernelRX: true}
	}
	return Caps{}
}

// scmTimestamping mirrors struct scm_timestamping: three timespecs, of which
// the first carries the software timestamp.
type scmTimestamping struct {
	TS [3]unix.Timespec
}

// Both structures we reinterpret from raw control-message bytes must match
// the kernel ABI exactly; a silent layout mismatch would yield plausible-
// looking but wrong timestamps. sock_extended_err is fixed-width on every
// Linux architecture, and scm_timestamping is three timespecs. These asserts
// fail the build rather than the measurements.
const (
	_ = uint(unsafe.Sizeof(unix.SockExtendedErr{}) - 16)
	_ = uint(16 - unsafe.Sizeof(unix.SockExtendedErr{}))
	_ = uint(unsafe.Sizeof(scmTimestamping{}) - 3*unsafe.Sizeof(unix.Timespec{}))
	_ = uint(3*unsafe.Sizeof(unix.Timespec{}) - unsafe.Sizeof(scmTimestamping{}))
)

func fromOOB(oob []byte) (time.Time, bool) {
	cmsgs, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		return time.Time{}, false
	}
	for _, m := range cmsgs {
		if m.Header.Level == unix.SOL_SOCKET && m.Header.Type == unix.SCM_TIMESTAMPING &&
			len(m.Data) >= int(unsafe.Sizeof(scmTimestamping{})) {
			ts := (*scmTimestamping)(unsafe.Pointer(&m.Data[0])).TS[0]
			if ts.Sec == 0 && ts.Nsec == 0 {
				continue
			}
			return time.Unix(int64(ts.Sec), int64(ts.Nsec)), true
		}
	}
	return time.Time{}, false
}

func enableICMPErrors(fd int, ipv6 bool) bool {
	if ipv6 {
		return unix.SetsockoptInt(fd, unix.IPPROTO_IPV6, unix.IPV6_RECVERR, 1) == nil
	}
	return unix.SetsockoptInt(fd, unix.IPPROTO_IP, unix.IP_RECVERR, 1) == nil
}

func readErrQueue(fd int) ([]ErrQueueEntry, error) {
	var out []ErrQueueEntry
	// Timestamp entries carry no payload (OPT_TSONLY), but ICMP error entries
	// return the offending datagram, which is what identifies the ping.
	buf := make([]byte, 1500)
	oob := make([]byte, 1024)
	for {
		n, oobn, _, _, err := unix.Recvmsg(fd, buf, oob, unix.MSG_ERRQUEUE|unix.MSG_DONTWAIT)
		if err == unix.EAGAIN || err == unix.EWOULDBLOCK {
			return out, nil
		}
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return out, err
		}
		cmsgs, err := unix.ParseSocketControlMessage(oob[:oobn])
		if err != nil {
			continue
		}
		var at time.Time
		var haveTS bool
		var se *unix.SockExtendedErr
		for _, m := range cmsgs {
			switch {
			case m.Header.Level == unix.SOL_SOCKET && m.Header.Type == unix.SCM_TIMESTAMPING:
				if t, ok := fromOOBCmsg(m.Data); ok {
					at, haveTS = t, true
				}
			case (m.Header.Level == unix.IPPROTO_IP && m.Header.Type == unix.IP_RECVERR) ||
				(m.Header.Level == unix.IPPROTO_IPV6 && m.Header.Type == unix.IPV6_RECVERR):
				if len(m.Data) >= int(unsafe.Sizeof(unix.SockExtendedErr{})) {
					se = (*unix.SockExtendedErr)(unsafe.Pointer(&m.Data[0]))
				}
			}
		}
		if se == nil {
			continue
		}
		switch se.Origin {
		case unix.SO_EE_ORIGIN_TIMESTAMPING:
			// ee_data carries the OPT_ID counter of the packet sent.
			if haveTS {
				out = append(out, ErrQueueEntry{TXStamp: &TXStamp{Counter: se.Data, At: at}})
			}
		case unix.SO_EE_ORIGIN_ICMP, unix.SO_EE_ORIGIN_ICMP6:
			payload := make([]byte, n)
			copy(payload, buf[:n])
			out = append(out, ErrQueueEntry{
				ICMPError: &ICMPError{Type: se.Type, Code: se.Code, Payload: payload},
			})
		}
	}
}

func fromOOBCmsg(data []byte) (time.Time, bool) {
	if len(data) < int(unsafe.Sizeof(scmTimestamping{})) {
		return time.Time{}, false
	}
	ts := (*scmTimestamping)(unsafe.Pointer(&data[0])).TS[0]
	if ts.Sec == 0 && ts.Nsec == 0 {
		return time.Time{}, false
	}
	return time.Unix(int64(ts.Sec), int64(ts.Nsec)), true
}
