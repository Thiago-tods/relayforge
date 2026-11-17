// Package delivery defines the outbound delivery domain: the delivery state
// machine, attempt outcomes and the retry policy.
//
// This package is pure: it performs no database or network access, so its rules
// can be tested exhaustively. Delivery is at-least-once. A destination can
// receive the same delivery more than once (for example after a crash between
// receiving a response and recording it), so every request carries the stable
// delivery identifier for receivers to deduplicate.
package delivery

import (
	"errors"
	"fmt"
)

// Status is the lifecycle state of one delivery (one event to one destination).
type Status string

// Delivery statuses.
const (
	// StatusPending: waiting for its next attempt, due at next_attempt_at.
	StatusPending Status = "PENDING"
	// StatusInFlight: claimed by a worker under a lease; an attempt is running.
	StatusInFlight Status = "IN_FLIGHT"
	// StatusSucceeded: the destination answered with a 2xx status. Terminal.
	StatusSucceeded Status = "SUCCEEDED"
	// StatusFailed: a non-retryable result or no attempts left. Terminal.
	StatusFailed Status = "FAILED"
	// StatusCancelled: abandoned before an attempt, for example because the
	// destination was revoked. Terminal.
	StatusCancelled Status = "CANCELLED"
)

var knownStatuses = []Status{StatusPending, StatusInFlight, StatusSucceeded, StatusFailed, StatusCancelled}

// Valid reports whether s is a known status.
func (s Status) Valid() bool {
	for _, v := range knownStatuses {
		if s == v {
			return true
		}
	}
	return false
}

// IsTerminal reports whether no further transition is possible from s.
func (s Status) IsTerminal() bool {
	return s == StatusSucceeded || s == StatusFailed || s == StatusCancelled
}

// transitions is the complete set of legal moves.
//
//	PENDING   -> IN_FLIGHT   a worker claims the delivery under a lease
//	PENDING   -> CANCELLED   the destination was revoked
//	IN_FLIGHT -> SUCCEEDED   a 2xx response was received and recorded
//	IN_FLIGHT -> PENDING     a retryable failure, or the lease expired
//	IN_FLIGHT -> FAILED      a non-retryable result, or the attempts are used up
//
// A delivery is never completed without first being claimed, so every recorded
// result belongs to a claimed attempt.
var transitions = map[Status][]Status{
	StatusPending:  {StatusInFlight, StatusCancelled},
	StatusInFlight: {StatusSucceeded, StatusPending, StatusFailed},
}

// ErrIllegalTransition is wrapped by every rejected status change.
var ErrIllegalTransition = errors.New("illegal delivery status transition")

// AllowedTransitions returns the statuses reachable from s in one step.
func AllowedTransitions(s Status) []Status {
	return append([]Status(nil), transitions[s]...)
}

// CheckTransition returns nil if moving from one status to another is legal.
func CheckTransition(from, to Status) error {
	if !from.Valid() || !to.Valid() {
		return fmt.Errorf("%w: unknown status", ErrIllegalTransition)
	}
	for _, next := range transitions[from] {
		if next == to {
			return nil
		}
	}
	return fmt.Errorf("%w: %s -> %s", ErrIllegalTransition, from, to)
}
