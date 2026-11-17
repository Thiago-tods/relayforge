package delivery

import (
	"errors"
	"math"
	"math/rand/v2"
	"time"
)

// Outcome classifies what happened in one delivery attempt.
type Outcome string

// Attempt outcomes.
const (
	OutcomeSuccess         Outcome = "SUCCESS"           // 2xx response
	OutcomeHTTPError       Outcome = "HTTP_ERROR"        // 4xx or 5xx response
	OutcomeRedirect        Outcome = "REDIRECT"          // 3xx response; redirects are never followed
	OutcomeNetworkError    Outcome = "NETWORK_ERROR"     // connection, TLS or protocol failure
	OutcomeTimeout         Outcome = "TIMEOUT"           // a deadline expired
	OutcomeBlockedByPolicy Outcome = "BLOCKED_BY_POLICY" // the egress guard refused the address
	OutcomeAbandoned       Outcome = "ABANDONED"         // the worker died; the lease expired
)

// OutcomeFromHTTPStatus maps a final HTTP status code to an outcome. Codes
// outside 200 to 599 are not valid final responses and count as transport anomalies.
func OutcomeFromHTTPStatus(code int) Outcome {
	switch {
	case code >= 200 && code <= 299:
		return OutcomeSuccess
	case code >= 300 && code <= 399:
		return OutcomeRedirect
	case code >= 400 && code <= 599:
		return OutcomeHTTPError
	default:
		return OutcomeNetworkError
	}
}

// AttemptResult is what one attempt produced.
type AttemptResult struct {
	Outcome    Outcome
	HTTPStatus int
	// RetryAfter is the destination's Retry-After hint, zero if absent.
	RetryAfter time.Duration
	// ErrorClass is a short machine-readable failure class (for example a
	// transport error class), empty for HTTP responses. It is safe to store and log.
	ErrorClass string
}

// Retryable reports whether another attempt may change the result. Transient
// failures retry: network errors, timeouts, abandoned attempts, 408 (Request
// Timeout), 425 (Too Early), 429 (Too Many Requests) and 5xx. Everything else is final: other 4xx (the destination rejected the
// request), redirects (never followed), policy blocks and unknown outcomes.
func (r AttemptResult) Retryable() bool {
	switch r.Outcome {
	case OutcomeNetworkError, OutcomeTimeout, OutcomeAbandoned:
		return true
	case OutcomeHTTPError:
		return r.HTTPStatus == 408 || r.HTTPStatus == 425 || r.HTTPStatus == 429 || (r.HTTPStatus >= 500 && r.HTTPStatus <= 599)
	default:
		return false
	}
}

// RetryPolicy configures bounded exponential backoff.
type RetryPolicy struct {
	// Base is the delay after the first failed attempt.
	Base time.Duration
	// Factor multiplies the delay after each further failure.
	Factor float64
	// Cap is the longest possible delay, jitter included.
	Cap time.Duration
	// Jitter spreads each delay by up to +/- this fraction, in [0, 1).
	Jitter float64
	// MaxAttempts is the total number of attempts, the first one included.
	MaxAttempts int
}

// DefaultRetryPolicy: 30 s, doubling, capped at 6 h, +/-20% jitter, 8 attempts.
// With these values the retries span roughly 4 hours after the first attempt.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{Base: 30 * time.Second, Factor: 2, Cap: 6 * time.Hour, Jitter: 0.2, MaxAttempts: 8}
}

const (
	maxFactor      = 10
	maxCap         = 7 * 24 * time.Hour
	maxMaxAttempts = 50
)

// Validate rejects policies that could not be applied safely.
func (p RetryPolicy) Validate() error {
	switch {
	case p.Base <= 0:
		return errors.New("retry base delay must be positive")
	case p.Cap < p.Base || p.Cap > maxCap:
		return errors.New("retry cap must be at least the base delay and at most 7 days")
	case p.Factor < 1 || p.Factor > maxFactor || math.IsNaN(p.Factor):
		return errors.New("retry factor must be between 1 and 10")
	case p.Jitter < 0 || p.Jitter >= 1 || math.IsNaN(p.Jitter):
		return errors.New("retry jitter must be in [0, 1)")
	case p.MaxAttempts < 1 || p.MaxAttempts > maxMaxAttempts:
		return errors.New("max attempts must be between 1 and 50")
	}
	return nil
}

// Backoff returns the delay to wait after the given failed attempt (1-based;
// values below 1 count as 1). The result is always in (0, Cap]. rnd returns a
// number in [0, 1); nil selects the package random source. The delay is
// Base * Factor^(attempt-1), spread by +/- Jitter, then clamped to Cap.
func (p RetryPolicy) Backoff(attempt int, rnd func() float64) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if rnd == nil {
		rnd = rand.Float64
	}
	delay := float64(p.Base)
	limit := float64(p.Cap)
	for i := 1; i < attempt && delay < limit; i++ { // stops growing at the cap: no overflow, bounded loop
		delay *= p.Factor
	}
	if delay > limit {
		delay = limit
	}
	delay *= 1 + p.Jitter*(2*rnd()-1)
	if delay > limit {
		delay = limit
	}
	if delay < 1 {
		delay = 1
	}
	return time.Duration(delay)
}

// Decision reasons.
const (
	ReasonSucceeded    = "succeeded"
	ReasonRetry        = "retry"
	ReasonExhausted    = "exhausted"
	ReasonNonRetryable = "non_retryable"
)

// Decision is the next state of a delivery after an attempt.
type Decision struct {
	Status Status
	// NextAttemptAt is set only when Status is PENDING.
	NextAttemptAt time.Time
	Reason        string
}

// Decide computes the next state after attempt number `attempt` (1-based) ended
// with res. The result is always reachable from IN_FLIGHT.
//
// A destination's Retry-After is honored when it is longer than the backoff,
// and never beyond Cap, so a destination cannot park a delivery indefinitely.
func (p RetryPolicy) Decide(attempt int, res AttemptResult, now time.Time, rnd func() float64) Decision {
	if res.Outcome == OutcomeSuccess {
		return Decision{Status: StatusSucceeded, Reason: ReasonSucceeded}
	}
	if !res.Retryable() {
		return Decision{Status: StatusFailed, Reason: ReasonNonRetryable}
	}
	if attempt >= p.MaxAttempts {
		return Decision{Status: StatusFailed, Reason: ReasonExhausted}
	}
	delay := p.Backoff(attempt, rnd)
	if res.RetryAfter > delay {
		delay = min(res.RetryAfter, p.Cap)
	}
	return Decision{Status: StatusPending, NextAttemptAt: now.Add(delay), Reason: ReasonRetry}
}
