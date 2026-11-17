package delivery_test

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Thiago-tods/relayforge/internal/delivery"
)

// The event-time rule under test: a subscription applies to an event if
//
//	subscription.created_at <= event.received_at AND
//	(subscription.revoked_at IS NULL OR subscription.revoked_at > event.received_at)
//
// Later revocation must never remove an event that was already accepted, and
// FanOut and Reconcile must apply exactly the same rule.

// sub is one subscription interval in minutes relative to the event's acceptance (0).
// revoked is nil for a subscription that is still active.
type sub struct {
	created int
	revoked *int
}

func rev(m int) *int { return &m }

// insertSub inserts a subscription row with an explicit lifecycle interval.
func (f *fixture) insertSub(t *testing.T, org, destID string, anchor time.Time, s sub) string {
	t.Helper()
	q, _ := f.env.Pool.Querier()
	created := anchor.Add(time.Duration(s.created) * time.Minute)
	var id string
	var err error
	if s.revoked == nil {
		err = q.QueryRow(bg, `INSERT INTO subscriptions (organization_id, ingestion_endpoint_id, destination_id, created_by_user_id, created_at)
			VALUES ($1, $2, $3, $4, $5) RETURNING id::text`, org, f.ep, destID, f.owner.Principal.User.ID, created).Scan(&id)
	} else {
		err = q.QueryRow(bg, `INSERT INTO subscriptions (organization_id, ingestion_endpoint_id, destination_id, created_by_user_id, created_at, status, revoked_at)
			VALUES ($1, $2, $3, $4, $5, 'REVOKED', $6) RETURNING id::text`, org, f.ep, destID, f.owner.Principal.User.ID, created,
			anchor.Add(time.Duration(*s.revoked)*time.Minute)).Scan(&id)
	}
	if err != nil {
		t.Fatalf("insert subscription %+v: %v", s, err)
	}
	return id
}

// eventAccepted ingests an event and moves its acceptance time to anchor.
func (f *fixture) eventAccepted(t *testing.T, payload string, anchor time.Time) string {
	t.Helper()
	id := f.event(t, payload)
	f.env.Exec(t, "UPDATE webhook_events SET received_at = $2 WHERE id = $1", id, anchor)
	return id
}

func reconcileOnce(f *fixture) (int64, error) {
	var n int64
	err := f.env.Pool.InTx(bg, func(tx pgx.Tx) error {
		var err error
		n, err = delivery.Reconcile(bg, tx, time.Now().UTC(), 2*time.Minute, 7*24*time.Hour, 100)
		return err
	})
	return n, err
}

var temporalScenarios = []struct {
	name  string
	event int // acceptance time, minutes relative to the anchor
	subs  []sub
	want  int // deliveries expected for the single destination
}{
	{"revoked after acceptance but before fan-out keeps the event", 0, []sub{{-120, rev(5)}}, 1},
	{"created after acceptance gets nothing", 0, []sub{{5, nil}}, 0},
	{"revoked before acceptance gets nothing", 0, []sub{{-120, rev(-60)}}, 0},
	{"created and revoked entirely before acceptance gets nothing", 0, []sub{{-30, rev(-20)}}, 0},
	{"active during acceptance is delivered", 0, []sub{{-60, nil}}, 1},
	{"boundary: revoked exactly at acceptance is excluded", 0, []sub{{-60, rev(0)}}, 0},
	{"boundary: created exactly at acceptance is included", 0, []sub{{0, nil}}, 1},
	{"resubscribed: event in the new interval", 0, []sub{{-300, rev(-200)}, {-100, nil}}, 1},
	{"resubscribed: event in the gap between intervals", 0, []sub{{-300, rev(-200)}, {10, nil}}, 0},
	{"resubscribed: event in the earlier interval keeps eligibility", -250, []sub{{-300, rev(-200)}, {-100, nil}}, 1},
	{"back-to-back intervals meeting at the boundary give exactly one delivery", 0, []sub{{-60, rev(0)}, {0, nil}}, 1},
}

func TestEventTimeEligibilityForFanOutAndReconcile(t *testing.T) {
	for _, mode := range []string{"fanout", "reconcile"} {
		for _, sc := range temporalScenarios {
			t.Run(mode+"/"+sc.name, func(t *testing.T) {
				f := newFixture(t)
				org, user := f.owner.Org.ID, f.owner.Principal.User.ID
				d := f.destination(t, org, user, "D")
				anchor := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
				for _, s := range sc.subs {
					f.insertSub(t, org, d.ID, anchor, s)
				}
				event := f.eventAccepted(t, `{"t":1}`, anchor.Add(time.Duration(sc.event)*time.Minute))

				var err error
				if mode == "fanout" {
					_, err = fanOut(f, org, event)
				} else {
					_, err = reconcileOnce(f)
				}
				if err != nil {
					t.Fatal(err)
				}
				if got := f.deliveryCount(t, event); got != sc.want {
					t.Fatalf("%d deliveries, want %d", got, sc.want)
				}
			})
		}
	}
}

func TestFanOutAndReconcileAgreeAndNeverDuplicate(t *testing.T) {
	f := newFixture(t)
	org, user := f.owner.Org.ID, f.owner.Principal.User.ID
	d := f.destination(t, org, user, "D")
	anchor := time.Now().UTC().Add(-time.Hour)
	f.insertSub(t, org, d.ID, anchor, sub{-60, rev(5)})
	event := f.eventAccepted(t, `{"x":1}`, anchor)

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			var err error
			if i%2 == 0 {
				_, err = fanOut(f, org, event)
			} else {
				_, err = reconcileOnce(f)
			}
			if err != nil {
				t.Error(err)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	if got := f.deliveryCount(t, event); got != 1 {
		t.Fatalf("%d deliveries after concurrent fan-out and reconciliation, want exactly 1", got)
	}
}

func TestReconcileNeverCrossesTenantsEvenWithRevokedHistory(t *testing.T) {
	f := newFixture(t)
	orgA, userA := f.owner.Org.ID, f.owner.Principal.User.ID
	other := f.env.Register(t, "other@example.com")
	destB := f.destination(t, other.Org.ID, other.Principal.User.ID, "B")
	destA := f.destination(t, orgA, userA, "A")
	anchor := time.Now().UTC().Add(-time.Hour)
	f.insertSub(t, orgA, destA.ID, anchor, sub{-60, rev(5)})
	event := f.eventAccepted(t, `{"x":1}`, anchor)
	q, _ := f.env.Pool.Querier()
	if _, err := q.Exec(bg, `INSERT INTO subscriptions (organization_id, ingestion_endpoint_id, destination_id, created_by_user_id)
		VALUES ($1, $2, $3, $4)`, orgA, f.ep, destB.ID, userA); err == nil {
		t.Fatal("a cross-tenant subscription was accepted")
	}
	if _, err := reconcileOnce(f); err != nil {
		t.Fatal(err)
	}
	if f.deliveryCount(t, event) != 1 || f.env.Count(t, "SELECT count(*) FROM deliveries WHERE destination_id = $1", destB.ID) != 0 {
		t.Fatal("reconciliation crossed a tenant boundary")
	}
}

// Subscription eligibility and destination eligibility are separate concerns.

func TestRevokedDestinationKeepsAnAuditableCancelledDeliveryAndNothingIsSent(t *testing.T) {
	f := newFixture(t)
	org, user := f.owner.Org.ID, f.owner.Principal.User.ID
	d := f.destination(t, org, user, "D")
	anchor := time.Now().UTC().Add(-time.Hour)
	f.insertSub(t, org, d.ID, anchor, sub{-60, nil})
	event := f.eventAccepted(t, `{"x":1}`, anchor)
	// The destination is revoked after the event was accepted and before fan-out ran.
	f.env.Exec(t, "UPDATE destinations SET status = 'REVOKED', revoked_at = $2 WHERE id = $1", d.ID, anchor.Add(time.Minute))

	if n, err := fanOut(f, org, event); err != nil || n != 1 {
		t.Fatalf("fan-out created %d (%v), want 1: the event was accepted while the destination was active", n, err)
	}
	var claimed []delivery.Claimed
	if err := f.env.Pool.InTx(bg, func(tx pgx.Tx) error {
		var err error
		claimed, err = delivery.ClaimDue(bg, tx, time.Now().UTC().Add(time.Hour), 10, 2*time.Minute, "w")
		return err
	}); err != nil || len(claimed) != 0 {
		t.Fatalf("a revoked destination must never be claimed: %d %v", len(claimed), err)
	}
	q, _ := f.env.Pool.Querier()
	if n, err := delivery.CancelForRevokedDestinations(bg, q, time.Now().UTC()); err != nil || n != 1 {
		t.Fatalf("cancelled %d (%v), want 1", n, err)
	}
	if got := f.env.Count(t, "SELECT count(*) FROM deliveries WHERE event_id = $1 AND status = 'CANCELLED'", event); got != 1 {
		t.Fatalf("the delivery must remain as a CANCELLED record, found %d", got)
	}
}

func TestEventAcceptedAfterTheDestinationWasRevokedCreatesNoDelivery(t *testing.T) {
	f := newFixture(t)
	org, user := f.owner.Org.ID, f.owner.Principal.User.ID
	d := f.destination(t, org, user, "D")
	anchor := time.Now().UTC().Add(-time.Hour)
	f.insertSub(t, org, d.ID, anchor, sub{-60, nil})
	f.env.Exec(t, "UPDATE destinations SET status = 'REVOKED', revoked_at = $2 WHERE id = $1", d.ID, anchor.Add(-time.Minute))
	event := f.eventAccepted(t, `{"x":1}`, anchor)
	if n, err := fanOut(f, org, event); err != nil || n != 0 {
		t.Fatalf("created %d (%v), want 0", n, err)
	}
}

func TestPausedDestinationKeepsItsPendingDeliveryUntilActivated(t *testing.T) {
	f := newFixture(t)
	org, user := f.owner.Org.ID, f.owner.Principal.User.ID
	d := f.destination(t, org, user, "D")
	anchor := time.Now().UTC().Add(-time.Hour)
	f.insertSub(t, org, d.ID, anchor, sub{-60, nil})
	f.env.Exec(t, "UPDATE destinations SET status = 'PAUSED' WHERE id = $1", d.ID)
	event := f.eventAccepted(t, `{"x":1}`, anchor)
	if n, err := fanOut(f, org, event); err != nil || n != 1 {
		t.Fatalf("created %d (%v), want 1", n, err)
	}
	claim := func() int {
		var claimed []delivery.Claimed
		if err := f.env.Pool.InTx(bg, func(tx pgx.Tx) error {
			var err error
			claimed, err = delivery.ClaimDue(bg, tx, time.Now().UTC().Add(time.Minute), 10, 2*time.Minute, "w")
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return len(claimed)
	}
	if claim() != 0 {
		t.Fatal("a paused destination must not be claimed")
	}
	q, _ := f.env.Pool.Querier()
	if n, err := delivery.CancelForRevokedDestinations(bg, q, time.Now().UTC()); err != nil || n != 0 {
		t.Fatalf("pausing must not cancel anything: %d %v", n, err)
	}
	f.env.Exec(t, "UPDATE destinations SET status = 'ACTIVE' WHERE id = $1", d.ID)
	if claim() != 1 {
		t.Fatal("the pending delivery must be delivered after activation")
	}
	_ = fmt.Sprint
}
