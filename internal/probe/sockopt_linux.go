//go:build linux

package probe

import (
	"os"
	"strconv"
	"strings"
)

// Reading the kernel's per-socket drop counter.
//
// The obvious mechanism, SO_RXQ_OVFL, is a dead end here: setsockopt accepts
// it on a ping socket, but ping_recvmsg calls sock_recv_timestamp rather than
// the variant that attaches the drop counter, so no packet ever carries one.
// The option would have looked enabled and reported nothing — silence that is
// indistinguishable from "no drops". The counter is however published per
// socket in /proc/net/{icmp,icmp6,raw,raw6}, which is what we read instead.
//
// The value is sk_drops: replies the kernel discarded because our receive
// queue was full. It is the only way to tell "the network lost it" from "we
// were too slow to read it".

// procNetPath names the file listing sockets of this kind.
func procNetPath(family string, raw bool) string {
	switch {
	case raw && family == "v6":
		return "/proc/net/raw6"
	case raw:
		return "/proc/net/raw"
	case family == "v6":
		return "/proc/net/icmp6"
	default:
		return "/proc/net/icmp"
	}
}

// socketInode resolves the socket's inode number, which identifies its row in
// the proc listing.
func socketInode(fd int) (string, bool) {
	link, err := os.Readlink("/proc/self/fd/" + strconv.Itoa(fd))
	if err != nil {
		return "", false
	}
	// The link reads "socket:[12345]".
	inode, ok := strings.CutPrefix(link, "socket:[")
	if !ok {
		return "", false
	}
	inode, ok = strings.CutSuffix(inode, "]")
	return inode, ok
}

// readSocketDrops returns the cumulative sk_drops for the socket with this
// inode, from the given proc listing. The drops column is last on each row;
// the inode is the tenth field.
func readSocketDrops(path, inode string) (uint64, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	for line := range strings.Lines(string(data)) {
		fields := strings.Fields(line)
		if len(fields) < 13 || fields[9] != inode {
			continue
		}
		drops, err := strconv.ParseUint(fields[len(fields)-1], 10, 64)
		if err != nil {
			return 0, false
		}
		return drops, true
	}
	return 0, false
}
