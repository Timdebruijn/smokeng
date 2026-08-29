//go:build !linux

package trace

import "context"

// Path discovery reads the router's ICMP time-exceeded reply off the socket
// error queue, which is a Linux facility. Elsewhere no path is reported
// rather than a wrong one, and the caller records the absence — the same rule
// the timestamping fallback follows.
func traceroute(context.Context, Options) (Path, error) { return nil, ErrUnsupported }
