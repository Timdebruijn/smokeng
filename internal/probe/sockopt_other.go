//go:build !linux

package probe

// The kernel's per-socket drop counter is read from /proc/net on Linux (see
// sockopt_linux.go). Elsewhere it is unavailable, so drops cannot be
// distinguished from network loss and the enlarged receive buffer set in
// openConn is the only mitigation.
func procNetPath(string, bool) string { return "" }

func socketInode(int) (string, bool) { return "", false }

func readSocketDrops(string, string) (uint64, bool) { return 0, false }
