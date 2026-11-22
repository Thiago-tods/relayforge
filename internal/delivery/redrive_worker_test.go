package delivery_test

import (
	"testing"
	"time"

	"github.com/Thiago-tods/relayforge/internal/app"
	"github.com/Thiago-tods/relayforge/internal/delivery"
)

// ---- W1 and redrive through the real worker (ADR 34) ----

func (f *fixture) redrive(t *testing.T, id string, expected int) (app.RedriveResult, error) {
	t.Helper()
	return f.env.Svc.RedriveDelivery(bg, f.owner.Principal, f.owner.Org.ID, id, func() (app.RedriveInput, error) {
		return app.RedriveInput{Reason: "the receiver is back", ExpectedAttemptCount: expected}, nil
	})
}

// drain runs the worker until the delivery stops being PENDING, advancing the fake clock beyond any backoff.
func (f *fixture) drain(t *testing.T, w *delivery.Worker, id string) {
	t.Helper()
	for i := 0; i < 80; i++ {
		if st, _, _ := f.state(t, id); st != "PENDING" {
			return
		}
		f.runOnce(t, w)
		f.env.Clock.Advance(7 * time.Hour)
	}
	t.Fatal("the delivery never left PENDING")
}

func (f *fixture) reasonOf(t *testing.T, id string) (status string, max int, reason *string) {
	t.Helper()
	q, _ := f.env.Pool.Querier()
	if err := q.QueryRow(bg, "SELECT status, max_attempts, failure_reason FROM deliveries WHERE id = $1", id).Scan(&status, &max, &reason); err != nil {
		t.Fatal(err)
	}
	return
}

func (f *fixture) attemptNumbers(t *testing.T, id string) []int {
	t.Helper()
	var out []int
	for _, a := range f.attempts(t, id) {
		out = append(out, a.Number)
	}
	return out
}

func requireContiguous(t *testing.T, got []int, n int) {
	t.Helper()
	if len(got) != n {
		t.Fatalf("%d attempt rows, want %d: %v", len(got), n, got)
	}
	for i, v := range got {
		if v != i+1 {
			t.Fatalf("attempt numbers must be 1..%d without gaps or reuse: %v", n, got)
		}
	}
}

func TestOrdinaryDeliveriesKeepTheDefaultBudgetOfEight(t *testing.T) {
	f := newFixture(t)
	org, user := f.owner.Org.ID, f.owner.Principal.User.ID
	d := f.destination(t, org, user, "D")
	q, _ := f.env.Pool.Querier()
	var id string
	// max_attempts is not set: the column default is what fan-out relies on.
	if err := q.QueryRow(bg, `INSERT INTO deliveries (organization_id, event_id, destination_id, next_attempt_at)
		VALUES ($1, $2, $3, $4) RETURNING id::text`, org, f.event(t, `{"x":1}`), d.ID, f.env.Clock.Now().Add(-time.Second)).Scan(&id); err != nil {
		t.Fatal(err)
	}
	s := &fakeSender{respond: status(500)}
	f.drain(t, f.worker(t, s, nil), id)
	st, max, reason := f.reasonOf(t, id)
	if st != "FAILED" || max != 8 || reason == nil || *reason != "RETRIES_EXHAUSTED" || s.count() != 8 {
		t.Fatalf("%s max %d reason %v after %d requests, want FAILED, 8, RETRIES_EXHAUSTED, 8", st, max, reason, s.count())
	}
	requireContiguous(t, f.attemptNumbers(t, id), 8)
}

func TestARowBudgetAboveTheDefaultIsHonoredByTheWorker(t *testing.T) {
	f := newFixture(t)
	org, user := f.owner.Org.ID, f.owner.Principal.User.ID
	d := f.destination(t, org, user, "D")
	id := f.pending(t, org, f.event(t, `{"x":1}`), d.ID, 13)
	s := &fakeSender{respond: status(500)}
	f.drain(t, f.worker(t, s, nil), id)
	if st, max, _ := f.reasonOf(t, id); st != "FAILED" || max != 13 || s.count() != 13 {
		t.Fatalf("%s max %d after %d requests, want FAILED after exactly 13", st, max, s.count())
	}
	requireContiguous(t, f.attemptNumbers(t, id), 13)
}

func TestARowBudgetAboveTheDefaultIsHonoredByLeaseRecovery(t *testing.T) {
	f := newFixture(t)
	org, user := f.owner.Org.ID, f.owner.Principal.User.ID
	d := f.destination(t, org, user, "D")
	id := f.pending(t, org, f.event(t, `{}`), d.ID, 13)
	w := f.worker(t, &fakeSender{}, nil)
	for round := 1; round <= 13; round++ {
		c := claimOne(t, f, "crashing")
		if c.Attempt != round || c.MaxAttempts != 13 {
			t.Fatalf("round %d claimed attempt %d of %d", round, c.Attempt, c.MaxAttempts)
		}
		f.env.Clock.Advance(3 * time.Minute)
		if n, err := w.RecoverExpired(bg); err != nil || n != 1 {
			t.Fatalf("round %d recovery: %d %v", round, n, err)
		}
		if round == 8 {
			// Under the old limit of 8 this is where recovery gave up.
			if st, _, _ := f.state(t, id); st != "PENDING" {
				t.Fatalf("after attempt 8 of 13 the delivery is %s, want PENDING", st)
			}
		}
		f.env.Clock.Advance(7 * time.Hour)
	}
	if st, _, _ := f.state(t, id); st != "FAILED" {
		t.Fatalf("after attempt 13 of 13: %s", st)
	}
	requireContiguous(t, f.attemptNumbers(t, id), 13)
}

func TestRedrivenDeliveryGetsFiveMoreAttemptsThroughTheRealWorker(t *testing.T) {
	f := newFixture(t)
	org, user := f.owner.Org.ID, f.owner.Principal.User.ID
	d := f.destination(t, org, user, "D")
	id := f.pending(t, org, f.event(t, `{"x":1}`), d.ID, 8)
	s := &fakeSender{respond: status(500)}
	w := f.worker(t, s, nil)
	f.drain(t, w, id)
	if st, _, _ := f.state(t, id); st != "FAILED" || s.count() != 8 {
		t.Fatalf("setup: %s after %d requests", st, s.count())
	}

	res, err := f.redrive(t, id, 8)
	if err != nil {
		t.Fatalf("redrive: %v", err)
	}
	if res.PreviousMaxAttempts != 8 || res.Delivery.MaxAttempts != 13 || res.Delivery.Status != "PENDING" || res.Delivery.AttemptCount != 8 || res.Delivery.FailureReason != nil {
		t.Fatalf("redrive result: %+v", res)
	}
	// A real worker run: exactly five more attempts (9 to 13), then RETRIES_EXHAUSTED again.
	f.drain(t, w, id)
	st, max, reason := f.reasonOf(t, id)
	if st != "FAILED" || max != 13 || reason == nil || *reason != "RETRIES_EXHAUSTED" || s.count() != 13 {
		t.Fatalf("%s max %d reason %v after %d requests, want FAILED, 13, RETRIES_EXHAUSTED, 13", st, max, reason, s.count())
	}
	requireContiguous(t, f.attemptNumbers(t, id), 13)
	// Two terminal attempts now carry a reason, one per failure episode.
	if n := f.env.Count(t, "SELECT count(*) FROM delivery_attempts WHERE delivery_id = $1 AND terminal_reason = 'RETRIES_EXHAUSTED'", id); n != 2 {
		t.Fatalf("%d terminal attempts, want 2 (attempt 8 and attempt 13)", n)
	}
	// The earlier attempts are untouched, and a late duplicate of the first request is refused as stale.
	if _, err := f.redrive(t, id, 8); err == nil || err.Error() != app.ErrDeliveryChanged.Error() {
		t.Fatalf("a stale request: %v, want %v", err, app.ErrDeliveryChanged)
	}
	// A second redrive of the second failure episode is allowed: 13 -> 18.
	res, err = f.redrive(t, id, 13)
	if err != nil || res.PreviousMaxAttempts != 13 || res.Delivery.MaxAttempts != 18 {
		t.Fatalf("second redrive: %+v %v", res, err)
	}
	if n := f.env.Count(t, "SELECT count(*) FROM audit_events WHERE resource_id = $1 AND action = 'delivery.redriven'", id); n != 2 {
		t.Fatalf("%d audit events, want 2", n)
	}
}

func TestRedrivenPartialBudgetKeepsTheWholeRaisedBudget(t *testing.T) {
	// A permanent failure at attempt 1 of 8 is redriven to 13 (old + 5): the delivery may use attempts 2 to 13.
	f := newFixture(t)
	org, user := f.owner.Org.ID, f.owner.Principal.User.ID
	d := f.destination(t, org, user, "D")
	id := f.pending(t, org, f.event(t, `{"x":1}`), d.ID, 8)
	s := &fakeSender{respond: status(410)}
	w := f.worker(t, s, nil)
	f.drain(t, w, id)
	if st, attempts, _ := f.state(t, id); st != "FAILED" || attempts != 1 {
		t.Fatalf("setup: %s after %d attempts", st, attempts)
	}
	if _, err := f.redrive(t, id, 1); err != nil {
		t.Fatal(err)
	}
	s.respond = status(500)
	f.drain(t, w, id)
	if st, max, _ := f.reasonOf(t, id); st != "FAILED" || max != 13 || s.count() != 13 {
		t.Fatalf("%s max %d after %d requests, want FAILED, 13, 13", st, max, s.count())
	}
	requireContiguous(t, f.attemptNumbers(t, id), 13)
}

func TestRedrivenDeliveryOfAPausedDestinationWaitsUntilItIsActivated(t *testing.T) {
	f := newFixture(t)
	org, user := f.owner.Org.ID, f.owner.Principal.User.ID
	d := f.destination(t, org, user, "D")
	id := f.pending(t, org, f.event(t, `{"x":1}`), d.ID, 8)
	s := &fakeSender{respond: status(410)}
	w := f.worker(t, s, nil)
	f.drain(t, w, id)
	if _, err := f.env.Svc.PauseDestination(bg, f.owner.Principal, org, d.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.redrive(t, id, 1); err != nil {
		t.Fatalf("a paused destination may be redriven: %v", err)
	}
	s.respond = status(204)
	for i := 0; i < 3; i++ {
		f.runOnce(t, w)
		f.env.Clock.Advance(time.Hour)
	}
	if st, attempts, _ := f.state(t, id); st != "PENDING" || attempts != 1 || s.count() != 1 {
		t.Fatalf("a paused destination must keep the delivery PENDING and unclaimed: %s, %d attempts, %d requests", st, attempts, s.count())
	}
	if _, err := f.env.Svc.ActivateDestination(bg, f.owner.Principal, org, d.ID); err != nil {
		t.Fatal(err)
	}
	f.drain(t, w, id)
	if st, attempts, _ := f.state(t, id); st != "SUCCEEDED" || attempts != 2 {
		t.Fatalf("after activation: %s after %d attempts, want SUCCEEDED after 2", st, attempts)
	}
}
