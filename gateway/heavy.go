package gateway

import (
	"net/http"
	"strconv"
)

// Heavy reports (#97, #111). The lending, chargeback and decision reports read
// a whole period of every organisation whoever asks, and the last one typesets
// a PDF: one organisation's administrator must not be able to exhaust the
// process for the others by looping on them. At most heavyReports of them run
// at once; the next caller is told to come back (429 with Retry-After) instead
// of queuing memory. The store reads run under ReportTimeout, like every
// report.

// heavyReports is the number of heavy reports served at once.
const heavyReports = 2

// heavyRetryAfter is what a refused caller is told, in seconds.
const heavyRetryAfter = 5

var heavySem = make(chan struct{}, heavyReports)

// heavy takes a slot for a heavy report, or answers 429 itself.
func heavy(w http.ResponseWriter) (release func(), ok bool) {
	select {
	case heavySem <- struct{}{}:
		return func() { <-heavySem }, true
	default:
		w.Header().Set("Retry-After", strconv.Itoa(heavyRetryAfter))
		apiError(w, http.StatusTooManyRequests, "rate_limit_error", "too many reports are being built; retry shortly")
		return nil, false
	}
}
