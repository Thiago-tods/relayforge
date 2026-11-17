package delivery

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Thiago-tods/relayforge/internal/egress"
)

// maxRetryAfter bounds how large a parsed Retry-After can be. The retry policy
// clamps it further to its own cap; this only keeps the arithmetic safe.
const maxRetryAfter = 30 * 24 * time.Hour

// ParseRetryAfter reads a Retry-After header value: either delta-seconds
// (digits only) or an HTTP-date in any of the three formats RFC 9110 requires.
// It returns 0, meaning "no usable hint", for malformed values, dates that are
// already past and zero. It never fails and never returns a negative or unbounded duration.
func ParseRetryAfter(value string, now time.Time) time.Duration {
	v := strings.TrimSpace(value)
	if v == "" || len(v) > 64 {
		if len(v) > 64 && isDigits(v) {
			return maxRetryAfter // an absurdly long number is a very large number
		}
		return 0
	}
	if isDigits(v) {
		secs, err := strconv.ParseInt(v, 10, 64)
		if err != nil || secs > int64(maxRetryAfter/time.Second) {
			return maxRetryAfter
		}
		return time.Duration(secs) * time.Second
	}
	t, err := http.ParseTime(v)
	if err != nil {
		return 0
	}
	d := t.Sub(now)
	switch {
	case d <= 0:
		return 0
	case d > maxRetryAfter:
		return maxRetryAfter
	}
	return d
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// ResultFromResponse classifies a destination's HTTP response.
//
//	2xx          success
//	3xx          final failure: redirects are never followed, Location is ignored
//	408, 425, 429, 5xx   retryable (a Retry-After hint is honored within the policy cap)
//	other 4xx    final failure: the destination rejected the request
func ResultFromResponse(code int, retryAfter string, now time.Time) AttemptResult {
	return AttemptResult{
		Outcome:    OutcomeFromHTTPStatus(code),
		HTTPStatus: code,
		RetryAfter: ParseRetryAfter(retryAfter, now),
	}
}

// Classified is the classification of a failed request.
type Classified struct {
	Result AttemptResult
	// Interrupted is true when the caller's context ended (for example during
	// shutdown). Nothing is known about the destination, so the attempt must not be
	// recorded as a failure: the claim simply expires and lease recovery handles it.
	Interrupted bool
}

// ResultFromRequestError classifies a transport-level failure by its egress
// error class.
//
//	blocked_address, invalid_url, invalid_request   final: BLOCKED_BY_POLICY
//	dns_error, connect_error, protocol_error        retryable: NETWORK_ERROR
//	tls_error                                       retryable: NETWORK_ERROR. An expired or
//	                                                mismatched certificate is usually fixed by
//	                                                the destination's owner, so it is retried
//	                                                with backoff, bounded by max attempts.
//	dns_timeout, connect_timeout, timeout           retryable: TIMEOUT
//	canceled                                        interrupted, not a result
//
// Unknown classes retry, bounded by the maximum number of attempts.
func ResultFromRequestError(class egress.ErrorClass) Classified {
	r := AttemptResult{ErrorClass: string(class)}
	switch class {
	case egress.ClassCanceled:
		return Classified{Interrupted: true}
	case egress.ClassBlockedAddress, egress.ClassInvalidURL, egress.ClassInvalidRequest:
		r.Outcome = OutcomeBlockedByPolicy
	case egress.ClassDNSTimeout, egress.ClassConnectTimeout, egress.ClassTimeout:
		r.Outcome = OutcomeTimeout
	default:
		r.Outcome = OutcomeNetworkError
	}
	return Classified{Result: r}
}
