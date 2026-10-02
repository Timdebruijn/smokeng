package api

// rangeRows is the most rows [from, to) could hold, at the densest a series can
// be. The subtraction is done in uint64: to-from overflows int64 for a window
// that starts at a very negative from and ends at a large to, and the wrapped
// result is negative, so a guard written as (to-from)/n > limit read the widest
// window there is as an empty one and let it through.
func rangeRows(from, to int64) uint64 {
	if to <= from {
		return 0
	}
	return (uint64(to) - uint64(from)) / minIntervalS
}
