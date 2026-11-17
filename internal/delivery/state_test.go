package delivery

import (
	"errors"
	"testing"
)

var allStatuses = []Status{StatusPending, StatusInFlight, StatusSucceeded, StatusFailed, StatusCancelled}

func TestStatusValidityAndTerminality(t *testing.T) {
	for _, s := range allStatuses {
		if !s.Valid() {
			t.Errorf("%s must be valid", s)
		}
	}
	for _, bad := range []Status{"", "pending", "RETRY_WAIT", "DONE", "PENDING "} {
		if bad.Valid() {
			t.Errorf("%q must be invalid", bad)
		}
	}
	terminal := map[Status]bool{StatusSucceeded: true, StatusFailed: true, StatusCancelled: true}
	for _, s := range allStatuses {
		if s.IsTerminal() != terminal[s] {
			t.Errorf("%s.IsTerminal() = %v", s, s.IsTerminal())
		}
	}
}

// TestTransitionTable lists every one of the 25 (from, to) pairs explicitly, so a
// change to the state machine must change this table.
func TestTransitionTable(t *testing.T) {
	allowed := map[[2]Status]bool{
		{StatusPending, StatusInFlight}:   true, // claim
		{StatusPending, StatusCancelled}:  true, // destination revoked before the first or next attempt
		{StatusInFlight, StatusSucceeded}: true, // 2xx recorded
		{StatusInFlight, StatusPending}:   true, // retry scheduled, or lease expired
		{StatusInFlight, StatusFailed}:    true, // non-retryable result or retries exhausted
	}
	for _, from := range allStatuses {
		for _, to := range allStatuses {
			err := CheckTransition(from, to)
			if allowed[[2]Status{from, to}] {
				if err != nil {
					t.Errorf("%s -> %s must be allowed: %v", from, to, err)
				}
				continue
			}
			if !errors.Is(err, ErrIllegalTransition) {
				t.Errorf("%s -> %s must be illegal, got %v", from, to, err)
			}
		}
	}
}

func TestTerminalStatesNeverTransition(t *testing.T) {
	for _, from := range []Status{StatusSucceeded, StatusFailed, StatusCancelled} {
		if len(AllowedTransitions(from)) != 0 {
			t.Errorf("%s must have no outgoing transitions", from)
		}
		for _, to := range allStatuses {
			if CheckTransition(from, to) == nil {
				t.Errorf("terminal %s -> %s was allowed", from, to)
			}
		}
	}
}

func TestInvalidStatusesAreNeverAllowed(t *testing.T) {
	for _, bad := range []Status{"", "pending", "UNKNOWN"} {
		for _, s := range allStatuses {
			if CheckTransition(bad, s) == nil || CheckTransition(s, bad) == nil {
				t.Errorf("transition involving invalid status %q was allowed", bad)
			}
		}
	}
}

func TestOnlyInFlightDeliveriesCanBeCompleted(t *testing.T) {
	// A delivery that was never claimed cannot succeed or fail: results must come from a claimed attempt.
	for _, to := range []Status{StatusSucceeded, StatusFailed} {
		if CheckTransition(StatusPending, to) == nil {
			t.Errorf("PENDING -> %s must be illegal", to)
		}
	}
	if got := AllowedTransitions(StatusInFlight); len(got) != 3 {
		t.Errorf("IN_FLIGHT must have exactly three exits, got %v", got)
	}
}
