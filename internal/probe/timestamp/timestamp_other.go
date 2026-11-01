//go:build !linux

package timestamp

import "time"

func EnableKernel(int) Caps                     { return Caps{} }
func EnableICMPErrors(int, bool) bool           { return false }
func FromOOB([]byte) (time.Time, bool)          { return time.Time{}, false }
func ReadErrQueue(int) ([]ErrQueueEntry, error) { return nil, nil }
