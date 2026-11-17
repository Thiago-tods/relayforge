package delivery_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Thiago-tods/relayforge/internal/delivery"
)

// subscribe links the fixture's ingestion endpoint to a destination. It back-dates
// the subscription so events ingested a moment ago are covered.
func (f *fixture) subscribe(t *testing.T, org, destID string) string {
	t.Helper()
	q, _ := f.env.Pool.Querier()
	var id string
	err := q.QueryRow(bg, `INSERT INTO subscriptions (organization_id, ingestion_endpoint_id, destination_id, created_by_user_id, created_at)
		VALUES ($1, $2, $3, $4, now() - interval '1 hour') RETURNING id`, org, f.ep, destID, f.owner.Principal.User.ID).Scan(&id)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	return id
}

func (f *fixture) deliveryCount(t *testing.T, eventID string) int {
	return f.env.Count(t, "SELECT count(*) FROM deliveries WHERE event_id = $1", eventID)
}

func fanOut(f *fixture, org, eventID string) (int64, error) {
	var n int64
	err := f.env.Pool.InTx(bg, func(tx pgx.Tx) error {
		var err error
		n, err = delivery.FanOut(bg, tx, org, eventID, f.env.Clock.Now())
		return err
	})
	return n, err
}

func TestFanOutCreatesOneDeliveryPerActiveSubscription(t *testing.T) {
	f := newFixture(t)
	org, user := f.owner.Org.ID, f.owner.Principal.User.ID
	a, b := f.destination(t, org, user, "A"), f.destination(t, org, user, "B")
	paused, revoked, unsubscribed := f.destination(t, org, user, "P"), f.destination(t, org, user, "R"), f.destination(t, org, user, "U")
	for _, d := range []dest{a, b, paused, revoked} {
		f.subscribe(t, org, d.ID)
	}
	f.env.Exec(t, "UPDATE destinations SET status = 'PAUSED' WHERE id = $1", paused.ID)
	f.env.Exec(t, "UPDATE destinations SET status = 'REVOKED', revoked_at = now() WHERE id = $1", revoked.ID)
	_ = unsubscribed
	event := f.event(t, `{"fan":"out"}`)

	n, err := fanOut(f, org, event)
	if err != nil || n != 3 {
		t.Fatalf("created %d deliveries (%v), want 3: active, active and paused", n, err)
	}
	q, _ := f.env.Pool.Querier()
	rows, err := q.Query(bg, "SELECT destination_id::text, status, max_attempts FROM deliveries WHERE event_id = $1", event)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var id, status string
		var max int
		if err := rows.Scan(&id, &status, &max); err != nil {
			t.Fatal(err)
		}
		seen[id] = true
		if status != "PENDING" || max != 8 {
			t.Errorf("delivery to %s: %s max %d", id, status, max)
		}
	}
	for _, want := range []string{a.ID, b.ID, paused.ID} {
		if !seen[want] {
			t.Errorf("no delivery for %s", want)
		}
	}
	if seen[revoked.ID] || seen[unsubscribed.ID] {
		t.Error("revoked or unsubscribed destinations must receive nothing")
	}
}

func TestFanOutIsIdempotentAndSafeUnderConcurrency(t *testing.T) {
	f := newFixture(t)
	org, user := f.owner.Org.ID, f.owner.Principal.User.ID
	d := f.destination(t, org, user, "D")
	f.subscribe(t, org, d.ID)
	event := f.event(t, `{}`)

	var created atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 16 { // duplicate messages processed at the same time
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			n, err := fanOut(f, org, event)
			if err != nil {
				t.Error(err)
				return
			}
			created.Add(n)
		}()
	}
	close(start)
	wg.Wait()
	if created.Load() != 1 || f.deliveryCount(t, event) != 1 {
		t.Fatalf("%d rows created, %d deliveries exist, want exactly 1", created.Load(), f.deliveryCount(t, event))
	}
	if n, err := fanOut(f, org, event); err != nil || n != 0 {
		t.Fatalf("a replayed message must create nothing: %d %v", n, err)
	}
}

func TestFanOutNeverCrossesTenants(t *testing.T) {
	f := newFixture(t)
	orgA, userA := f.owner.Org.ID, f.owner.Principal.User.ID
	other := f.env.Register(t, "other@example.com")
	destB := f.destination(t, other.Org.ID, other.Principal.User.ID, "B")
	epB := f.env.CreateEndpoint(t, other)
	q, _ := f.env.Pool.Querier()
	// Tenant B subscribes its own destination to its own endpoint.
	if _, err := q.Exec(bg, `INSERT INTO subscriptions (organization_id, ingestion_endpoint_id, destination_id, created_by_user_id, created_at)
		VALUES ($1, $2, $3, $4, now() - interval '1 hour')`, other.Org.ID, epB.Endpoint.ID, destB.ID, other.Principal.User.ID); err != nil {
		t.Fatal(err)
	}
	// The schema refuses a subscription that links endpoint A with destination B.
	destA := f.destination(t, orgA, userA, "A")
	if _, err := q.Exec(bg, `INSERT INTO subscriptions (organization_id, ingestion_endpoint_id, destination_id, created_by_user_id)
		VALUES ($1, $2, $3, $4)`, orgA, f.ep, destB.ID, userA); err == nil {
		t.Fatal("a cross-tenant subscription was accepted")
	}
	f.subscribe(t, orgA, destA.ID)

	eventA := f.event(t, `{"tenant":"A"}`)
	// A message that carries the wrong organization finds no event and creates nothing.
	if n, err := fanOut(f, other.Org.ID, eventA); err != nil || n != 0 {
		t.Fatalf("a message naming another organization must create nothing: %d %v", n, err)
	}
	if n, err := fanOut(f, orgA, eventA); err != nil || n != 1 {
		t.Fatalf("own organization: %d %v", n, err)
	}
	if f.env.Count(t, "SELECT count(*) FROM deliveries WHERE destination_id = $1", destB.ID) != 0 {
		t.Fatal("tenant B's destination received tenant A's event")
	}
}

func TestSubscriptionsOnlyApplyToLaterEvents(t *testing.T) {
	f := newFixture(t)
	org, user := f.owner.Org.ID, f.owner.Principal.User.ID
	d := f.destination(t, org, user, "D")
	old := f.event(t, `{"old":true}`)
	q, _ := f.env.Pool.Querier()
	if _, err := q.Exec(bg, `INSERT INTO subscriptions (organization_id, ingestion_endpoint_id, destination_id, created_by_user_id)
		VALUES ($1, $2, $3, $4)`, org, f.ep, d.ID, user); err != nil {
		t.Fatal(err)
	}
	if n, err := fanOut(f, org, old); err != nil || n != 0 {
		t.Fatalf("an event received before the subscription must not be delivered: %d %v", n, err)
	}
	f.env.Exec(t, "SELECT pg_sleep(0.05)")
	recent := f.event(t, `{"old":false}`)
	if n, err := fanOut(f, org, recent); err != nil || n != 1 {
		t.Fatalf("an event received after the subscription: %d %v", n, err)
	}
}

func TestReconcileRepairsMissingDeliveriesWithoutDuplicates(t *testing.T) {
	f := newFixture(t)
	org, user := f.owner.Org.ID, f.owner.Principal.User.ID
	d1, d2 := f.destination(t, org, user, "D1"), f.destination(t, org, user, "D2")
	f.subscribe(t, org, d1.ID)
	f.subscribe(t, org, d2.ID)

	lost := f.event(t, `{"message":"lost"}`)    // the broker message was lost: no fan-out ever ran
	partial := f.event(t, `{"message":"half"}`) // fan-out ran for one destination only
	handled := f.event(t, `{"message":"done"}`)
	fresh := f.event(t, `{"message":"fresh"}`) // too recent to judge
	if _, err := fanOut(f, org, handled); err != nil {
		t.Fatal(err)
	}
	f.env.Exec(t, "INSERT INTO deliveries (organization_id, event_id, destination_id) VALUES ($1, $2, $3)", org, partial, d1.ID)
	// Age every event except the fresh one past the grace period.
	f.env.Exec(t, "UPDATE webhook_events SET received_at = now() - interval '10 minutes' WHERE id <> $1", fresh)
	handledBefore := f.deliveryCount(t, handled)

	run := func() int64 {
		var n int64
		if err := f.env.Pool.InTx(bg, func(tx pgx.Tx) error {
			var err error
			n, err = delivery.Reconcile(bg, tx, time.Now().UTC(), 2*time.Minute, 7*24*time.Hour, 100)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := run(); n != 3 {
		t.Fatalf("reconcile created %d deliveries, want 3 (2 for the lost event, 1 for the partial one)", n)
	}
	if f.deliveryCount(t, lost) != 2 || f.deliveryCount(t, partial) != 2 || f.deliveryCount(t, handled) != handledBefore {
		t.Fatalf("deliveries: lost=%d partial=%d handled=%d", f.deliveryCount(t, lost), f.deliveryCount(t, partial), f.deliveryCount(t, handled))
	}
	if f.deliveryCount(t, fresh) != 0 {
		t.Fatal("an event younger than the grace period must be left to the normal path")
	}
	if n := run(); n != 0 {
		t.Fatalf("a second sweep created %d deliveries, want 0", n)
	}
}

func TestReconcileIgnoresEventsOutsideTheLookbackAndHonorsTheLimit(t *testing.T) {
	f := newFixture(t)
	org, user := f.owner.Org.ID, f.owner.Principal.User.ID
	d := f.destination(t, org, user, "D")
	f.subscribe(t, org, d.ID)
	var ids []string
	for i := 0; i < 5; i++ {
		ids = append(ids, f.event(t, fmt.Sprintf(`{"i":%d}`, i)))
	}
	f.env.Exec(t, "UPDATE webhook_events SET received_at = now() - interval '10 minutes'")
	f.env.Exec(t, "UPDATE webhook_events SET received_at = now() - interval '30 days' WHERE id = $1", ids[0])
	f.env.Exec(t, "UPDATE subscriptions SET created_at = now() - interval '60 days'")

	var n int64
	if err := f.env.Pool.InTx(context.Background(), func(tx pgx.Tx) error {
		var err error
		n, err = delivery.Reconcile(bg, tx, time.Now().UTC(), 2*time.Minute, 7*24*time.Hour, 2)
		return err
	}); err != nil || n != 2 {
		t.Fatalf("the limit of 2 must bound one sweep: %d %v", n, err)
	}
	if f.deliveryCount(t, ids[0]) != 0 {
		t.Fatal("an event older than the lookback window must not be touched")
	}
}
