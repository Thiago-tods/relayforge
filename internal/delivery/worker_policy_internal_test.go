package delivery

import (
	"testing"
	"time"
)

// TestPolicyForMakesTheDeliveryRowAuthoritative pins W1 (ADR 34): the attempt limit of a decision is the row's own
// max_attempts, above or below the policy default, so that a redrive which raises the row's budget really grants
// attempts. Claim finalization and lease recovery both call policyFor, so both are covered by it.
func TestPolicyForMakesTheDeliveryRowAuthoritative(t *testing.T) {
	w := &Worker{d: WorkerDeps{Policy: DefaultRetryPolicy()}}
	if got := w.d.Policy.MaxAttempts; got != 8 {
		t.Fatalf("the default policy limit is %d, the premise of this test is 8", got)
	}
	for _, c := range []struct{ row, want int }{
		{1, 1}, {3, 3}, {8, 8}, // ordinary rows: unchanged (the column default is 8)
		{13, 13}, {18, 18}, {50, 50}, // redriven rows: the row grants more than the default
		{0, 8}, {-1, 8}, // not a valid row (the schema forbids it): the policy is only the fallback
	} {
		if got := w.policyFor(c.row).MaxAttempts; got != c.want {
			t.Errorf("policyFor(%d).MaxAttempts = %d, want %d", c.row, got, c.want)
		}
	}
	if w.d.Policy.MaxAttempts != 8 {
		t.Fatal("policyFor must not modify the worker's own policy")
	}
	// Everything but the limit is the worker's policy, so the backoff is the ordinary attempt-number backoff.
	p := w.policyFor(13)
	d := DefaultRetryPolicy()
	if p.Base != d.Base || p.Factor != d.Factor || p.Cap != d.Cap || p.Jitter != d.Jitter {
		t.Errorf("only the limit may differ: %+v vs %+v", p, d)
	}
}

// TestDecisionsWithARaisedBudget walks the retry decision of a redriven delivery, including partially consumed budgets.
func TestDecisionsWithARaisedBudget(t *testing.T) {
	w := &Worker{d: WorkerDeps{Policy: DefaultRetryPolicy()}}
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	rnd := func() float64 { return 0.5 }
	retryable := ResultFromResponse(500, "", now)
	permanent := ResultFromResponse(410, "", now)
	success := ResultFromResponse(204, "", now)

	for _, c := range []struct {
		name    string
		attempt int
		max     int
		res     AttemptResult
		status  Status
		reason  string
	}{
		{"exhausted delivery redriven to 13: attempt 9 retries", 9, 13, retryable, StatusPending, ReasonRetry},
		{"attempt 12 of 13 still retries", 12, 13, retryable, StatusPending, ReasonRetry},
		{"attempt 13 of 13 is exhausted", 13, 13, retryable, StatusFailed, ReasonExhausted},
		{"the same attempt 9 under the old limit of 8 would be exhausted", 9, 8, retryable, StatusFailed, ReasonExhausted},
		{"partial budget: failed at attempt 1 of 8, redriven to 13, attempt 2 retries", 2, 13, retryable, StatusPending, ReasonRetry},
		{"partial budget: attempt 11 of 13 retries", 11, 13, retryable, StatusPending, ReasonRetry},
		{"a permanent failure ends the delivery whatever the budget", 9, 13, permanent, StatusFailed, ReasonNonRetryable},
		{"a success ends the delivery whatever the budget", 12, 13, success, StatusSucceeded, ReasonSucceeded},
		{"budget 50: attempt 49 retries, attempt 50 is exhausted", 49, 50, retryable, StatusPending, ReasonRetry},
		{"budget 50, attempt 50", 50, 50, retryable, StatusFailed, ReasonExhausted},
	} {
		d := w.policyFor(c.max).Decide(c.attempt, c.res, now, rnd)
		if d.Status != c.status || d.Reason != c.reason {
			t.Errorf("%s: %s/%s, want %s/%s", c.name, d.Status, d.Reason, c.status, c.reason)
		}
		// The schema requires a PENDING delivery to have attempts left: a retry is never scheduled at or beyond the row's limit.
		if d.Status == StatusPending && c.attempt >= c.max {
			t.Errorf("%s: a retry was scheduled with no attempt left", c.name)
		}
	}
}
