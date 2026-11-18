package delivery

// FailureReason says why a delivery ended as FAILED. It is a closed, machine-readable
// taxonomy chosen by code from the attempt's classification: it is never derived from a
// response body, a header or an error message, so it is always safe to store, log and return.
//
// The same values are stored on deliveries.failure_reason and on the terminal attempt's
// delivery_attempts.terminal_reason (migration 00019 keeps both in step).
type FailureReason string

// Failure reasons.
const (
	// FailureDestinationRejected: the destination answered with a final 4xx (any 4xx other
	// than 408, 425 and 429, which are retried).
	FailureDestinationRejected FailureReason = "DESTINATION_REJECTED"
	// FailureRedirectRefused: the destination answered with a 3xx. Redirects are never followed.
	FailureRedirectRefused FailureReason = "REDIRECT_REFUSED"
	// FailureEgressBlocked: the egress guard refused the destination address or URL.
	FailureEgressBlocked FailureReason = "EGRESS_BLOCKED"
	// FailureRetriesExhausted: the attempt budget ran out while results were still retryable.
	// The final attempt's outcome and error class say what the last failure looked like.
	FailureRetriesExhausted FailureReason = "RETRIES_EXHAUSTED"
	// FailureInternalError: a platform fault, or a final result no other reason describes.
	FailureInternalError FailureReason = "INTERNAL_ERROR"
)

// AllFailureReasons returns every reason, in a stable order. It returns a new slice on every
// call, so callers cannot change the package's authoritative taxonomy.
func AllFailureReasons() []FailureReason {
	return []FailureReason{
		FailureDestinationRejected, FailureRedirectRefused, FailureEgressBlocked, FailureRetriesExhausted, FailureInternalError,
	}
}

// Valid reports whether r is one of the known reasons.
func (r FailureReason) Valid() bool {
	switch r {
	case FailureDestinationRejected, FailureRedirectRefused, FailureEgressBlocked, FailureRetriesExhausted, FailureInternalError:
		return true
	}
	return false
}

// String returns the reason as stored.
func (r FailureReason) String() string { return string(r) }

// ErrorClassDataMissing labels the attempt written when a claimed delivery's event or
// destination could not be loaded. It is defensive: foreign keys keep that data from being
// deleted, so it should never occur.
const ErrorClassDataMissing = "data_missing"

// nonRetryableReason classifies a final result that is not retryable. Anything that is not
// recognized is INTERNAL_ERROR, so a new outcome can never produce a FAILED row without a reason.
func nonRetryableReason(res AttemptResult) FailureReason {
	switch res.Outcome {
	case OutcomeRedirect:
		return FailureRedirectRefused
	case OutcomeHTTPError:
		return FailureDestinationRejected
	case OutcomeBlockedByPolicy:
		if res.ErrorClass == ErrorClassDataMissing {
			return FailureInternalError
		}
		return FailureEgressBlocked
	default:
		return FailureInternalError
	}
}
