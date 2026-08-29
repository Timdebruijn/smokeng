//go:build !linux

package timestamp

import "time"

func enableKernel(int) Caps                     { return Caps{} }
func enableICMPErrors(int, bool) bool           { return false }
func fromOOB([]byte) (time.Time, bool)          { return time.Time{}, false }
func readErrQueue(int) ([]ErrQueueEntry, error) { return nil, nil }
