package store

import (
	"errors"
	"fmt"
)

// What one read returns. A range is the caller's to choose, and a request
// bounded by the width of its window is not bounded by what the window holds: an
// interval can carry up to 65,535 samples, so 200,000 of them are tens of
// billions. These count what is actually read, rather than guessing from the
// width, so a month-long availability report (a legitimate request, and a big
// one) is not refused by a worst-case rule written for something else.
const (
	// MaxSamplesPerRead bounds the samples one QueryRange decodes: 80 MB of
	// uint32 is far past any plot, and the day of 100-ping, one-second data that
	// would reach a tenth of it is not something a browser can draw either.
	MaxSamplesPerRead = 20_000_000
	// MaxAvailabilityRows bounds the intervals one availability read returns:
	// 3.8 years at 60 s, 23 days at 1 s.
	MaxAvailabilityRows = 2_000_000
	// MaxPathRows bounds the route changes one read returns.
	MaxPathRows = 100_000
)

// The same limits as variables, so a test can lower them instead of writing
// twenty million samples to find out whether they are enforced.
var (
	maxSamplesPerRead   = MaxSamplesPerRead
	maxAvailabilityRows = MaxAvailabilityRows
	maxPathRows         = MaxPathRows
)

// ErrRangeTooLarge is what a read returns when the range holds more than one
// read will hand back. It wraps the specifics; callers test for it with
// errors.Is and ask for a narrower window.
var ErrRangeTooLarge = errors.New("store: that range holds more than one read returns")

func tooLarge(what string, limit int) error {
	return fmt.Errorf("%w: more than %d %s", ErrRangeTooLarge, limit, what)
}
