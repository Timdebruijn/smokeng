//go:build !linux

package timestamp

import "time"

func enableKernel(int) Caps               { return Caps{} }
func fromOOB([]byte) (time.Time, bool)    { return time.Time{}, false }
func readErrQueue(int) ([]TXStamp, error) { return nil, nil }
