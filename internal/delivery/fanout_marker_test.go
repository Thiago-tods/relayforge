package delivery_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Thiago-tods/relayforge/internal/delivery"
)

// ---- helpers ----

// backdate moves an event's acceptance time. outbox.created_at and
// webhook_events.received_at are one instant in production (one ingestion
// transaction), so tests move both.
func (f *fixture) backdate(t *testing.T, eventID string, at time.Time) {
	t.Helper()
	f.env.Exec(t, "UPDATE webhook_events SET received_at = $2 WHERE id = $1", eventID, at)
	f.env.Exec(t, "UPDATE outbox SET created_at = $2 WHERE event_id = $1", eventID, at)
}

func (f *fixture) marker(t *testing.T, eventID string) *time.Time {
	t.Helper()
	q, _ := f.env.Pool.Querier()
	var at *time.Time
	if err := q.QueryRow(bg, "SELECT fanned_out_at FROM outbox WHERE event_id = $1", eventID).Scan(&at); err != nil {
		t.Fatal(err)
	}
	return at
}

func (f *fixture) unmarked(t *testing.T) int {
	return f.env.Count(t, "SELECT count(*) FROM outbox WHERE fanned_out_at IS NULL")
}

// reconcileLane runs one batch of one lane in its own transaction.
func reconcileLane(f *fixture, lane delivery.Lane, limit int, skip ...string) (delivery.ReconcileResult, error) {
	return reconcileLaneAt(f, lane, time.Now().UTC(), limit, skip...)
}

func reconcileLaneAt(f *fixture, lane delivery.Lane, now time.Time, limit int, skip ...string) (delivery.ReconcileResult, error) {
	var res delivery.ReconcileResult
	err := f.env.Pool.InTx(bg, func(tx pgx.Tx) error {
		var err error
		res, err = delivery.Reconcile(bg, tx, delivery.ReconcileParams{
			Now: now, Lane: lane, Grace: 2 * time.Minute, LiveWindow: delivery.DefaultLiveWindow, Limit: limit, Skip: skip,
		})
		return err
	})
	return res, err
}

// reconcileBatch runs one batch of each lane and adds the results up, for tests that do not
// care which lane an event is in.
func reconcileBatch(f *fixture, limit int) (delivery.ReconcileResult, error) {
	var sum delivery.ReconcileResult
	for _, lane := range []delivery.Lane{delivery.LaneLive, delivery.LaneHistory} {
		res, err := reconcileLane(f, lane, limit)
		if err != nil {
			return sum, err
		}
		sum.Examined += res.Examined
		sum.Events += res.Events
		sum.Deliveries += res.Deliveries
		sum.Failed = append(sum.Failed, res.Failed...)
	}
	return sum, nil
}

// subscribeLongAgo subscribes a destination 60 days ago, so events of any age in these
// tests are inside the subscription interval.
func (f *fixture) subscribeLongAgo(t *testing.T, org, destID string) {
	t.Helper()
	f.insertSub(t, org, destID, time.Now().UTC(), sub{-60 * 24 * 60, nil})
}

// ---- the marker is written by FanOut, in the same transaction ----

func TestFanOutMarksTheOutboxRowAndARepeatKeepsTheFirstMarker(t *testing.T) {
	f := newFixture(t)
	org, user := f.owner.Org.ID, f.owner.Principal.User.ID
	d := f.destination(t, org, user, "D")
	f.subscribe(t, org, d.ID)
	event := f.event(t, `{"m":1}`)
	if f.marker(t, event) != nil {
		t.Fatal("a freshly accepted event must be unmarked")
	}

	n, err := fanOut(f, org, event)
	if err != nil || n != 1 {
		t.Fatalf("fan-out created %d (%v)", n, err)
	}
	first := f.marker(t, event)
	if first == nil || first.Sub(f.env.Clock.Now()).Abs() > time.Millisecond {
		t.Fatalf("the marker must be the commit time of the fan-out transaction, got %v", first)
	}

	// A redelivered message (crash after commit, before the acknowledgment) is a no-op.
	f.env.Clock.Advance(time.Hour)
	if n, err := fanOut(f, org, event); err != nil || n != 0 {
		t.Fatalf("duplicate fan-out created %d (%v)", n, err)
	}
	if again := f.marker(t, event); again == nil || !again.Equal(*first) {
		t.Fatalf("a repeat must keep the first marker: %v then %v", first, again)
	}
	if f.deliveryCount(t, event) != 1 {
		t.Fatal("duplicate fan-out changed the deliveries")
	}
}

func TestFanOutMarksEventsWithoutEligibleSubscriptionsToo(t *testing.T) {
	f := newFixture(t)
	event := f.event(t, `{"m":2}`) // no subscription at all
	n, err := fanOut(f, f.owner.Org.ID, event)
	if err != nil || n != 0 {
		t.Fatalf("created %d (%v)", n, err)
	}
	if f.marker(t, event) == nil {
		t.Fatal("the marker means 'fan-out ran', not 'deliveries exist'")
	}
}

func TestFanOutForAnotherOrganizationMarksNothing(t *testing.T) {
	f := newFixture(t)
	other := f.env.Register(t, "other@example.com")
	event := f.event(t, `{"m":3}`)
	if n, err := fanOut(f, other.Org.ID, event); err != nil || n != 0 {
		t.Fatalf("created %d (%v)", n, err)
	}
	if f.marker(t, event) != nil {
		t.Fatal("a message naming the wrong organization must not mark another tenant's outbox row")
	}
}

func TestRolledBackFanOutLeavesNoMarkerAndNoDeliveries(t *testing.T) {
	f := newFixture(t)
	org, user := f.owner.Org.ID, f.owner.Principal.User.ID
	d := f.destination(t, org, user, "D")
	f.subscribe(t, org, d.ID)
	event := f.event(t, `{"m":4}`)

	crash := errors.New("process died before commit")
	err := f.env.Pool.InTx(bg, func(tx pgx.Tx) error {
		if n, err := delivery.FanOut(bg, tx, org, event, f.env.Clock.Now()); err != nil || n != 1 {
			t.Fatalf("fan-out in the transaction: %d %v", n, err)
		}
		return crash
	})
	if !errors.Is(err, crash) {
		t.Fatalf("expected the injected failure, got %v", err)
	}
	if f.marker(t, event) != nil || f.deliveryCount(t, event) != 0 {
		t.Fatal("a rolled-back transaction must leave neither the marker nor the deliveries")
	}
	// The retry (message redelivery) does the work.
	if n, err := fanOut(f, org, event); err != nil || n != 1 || f.marker(t, event) == nil {
		t.Fatalf("retry: %d %v", n, err)
	}
}

// failOn installs a trigger that raises on the given statement, simulating a failure at
// that step. The trigger is dropped when the test ends.
func (f *fixture) failOn(t *testing.T, table, event, when string) {
	t.Helper()
	f.env.Exec(t, fmt.Sprintf(`CREATE FUNCTION inject_failure() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN IF %s THEN RAISE EXCEPTION 'injected failure'; END IF; RETURN NEW; END $$`, when))
	f.env.Exec(t, fmt.Sprintf("CREATE TRIGGER inject_failure BEFORE %s ON %s FOR EACH ROW EXECUTE FUNCTION inject_failure()", event, table))
}

func (f *fixture) stopFailing(t *testing.T, table string) {
	t.Helper()
	f.env.Exec(t, fmt.Sprintf("DROP TRIGGER inject_failure ON %s", table))
	f.env.Exec(t, "DROP FUNCTION inject_failure()")
}

func TestAFailedMarkerUpdateCannotLeaveDeliveriesBehind(t *testing.T) {
	f := newFixture(t)
	org, user := f.owner.Org.ID, f.owner.Principal.User.ID
	d := f.destination(t, org, user, "D")
	f.subscribe(t, org, d.ID)
	event := f.event(t, `{"m":5}`)

	f.failOn(t, "outbox", "UPDATE", "NEW.fanned_out_at IS NOT NULL")
	if _, err := fanOut(f, org, event); err == nil {
		t.Fatal("the failed marker update must fail the fan-out")
	}
	if f.deliveryCount(t, event) != 0 || f.marker(t, event) != nil {
		t.Fatal("deliveries and marker are one transaction: neither may survive a failure of the other")
	}
	f.env.Exec(t, "UPDATE outbox SET created_at = now() - interval '1 hour' WHERE event_id = $1", event)
	if _, err := reconcileBatch(f, 10); err == nil {
		t.Fatal("reconciliation must fail too when it cannot mark the row")
	}
	if f.deliveryCount(t, event) != 0 {
		t.Fatal("reconciliation left deliveries without their marker")
	}

	f.stopFailing(t, "outbox")
	if n, err := fanOut(f, org, event); err != nil || n != 1 {
		t.Fatalf("after the fault is gone: %d %v", n, err)
	}
}

func TestAFailedDeliveryInsertAlsoRollsBackTheMarker(t *testing.T) {
	f := newFixture(t)
	org, user := f.owner.Org.ID, f.owner.Principal.User.ID
	d := f.destination(t, org, user, "D")
	f.subscribe(t, org, d.ID)
	event := f.event(t, `{"m":6}`)

	f.failOn(t, "deliveries", "INSERT", "true")
	if _, err := fanOut(f, org, event); err == nil {
		t.Fatal("expected the delivery insert to fail")
	}
	// FanOut writes the marker first; the failure that follows must take it back with it.
	if f.marker(t, event) != nil {
		t.Fatal("the marker survived a rolled-back fan-out: this event would never be reconciled")
	}
	f.stopFailing(t, "deliveries")
}

// ---- reconciliation works from the marker ----

func TestReconcileRepairsALostFanOutOlderThanTheFormerLookback(t *testing.T) {
	f := newFixture(t)
	org, user := f.owner.Org.ID, f.owner.Principal.User.ID
	d1, d2 := f.destination(t, org, user, "D1"), f.destination(t, org, user, "D2")
	f.subscribeLongAgo(t, org, d1.ID)
	f.subscribeLongAgo(t, org, d2.ID)
	lost := f.event(t, `{"m":"lost"}`)
	f.backdate(t, lost, time.Now().UTC().Add(-30*24*time.Hour)) // far beyond 48 h and beyond JetStream's retention

	res, err := reconcileBatch(f, 100)
	if err != nil || res.Events != 1 || res.Deliveries != 2 {
		t.Fatalf("result %+v (%v): a 30-day-old lost fan-out must be repaired", res, err)
	}
	if f.deliveryCount(t, lost) != 2 || f.marker(t, lost) == nil {
		t.Fatal("deliveries or marker missing after the repair")
	}
	if res, err := reconcileBatch(f, 100); err != nil || res.Events != 0 || res.Deliveries != 0 {
		t.Fatalf("the repaired row must not be revisited: %+v %v", res, err)
	}
}

func TestHealthyReconciliationHasNoOutstandingWork(t *testing.T) {
	f := newFixture(t)
	org, user := f.owner.Org.ID, f.owner.Principal.User.ID
	d := f.destination(t, org, user, "D")
	f.subscribe(t, org, d.ID)
	for i := 0; i < 8; i++ {
		ev := f.event(t, fmt.Sprintf(`{"i":%d}`, i))
		if _, err := fanOut(f, org, ev); err != nil {
			t.Fatal(err)
		}
		f.backdate(t, ev, time.Now().UTC().Add(-time.Duration(i+1)*24*time.Hour))
	}
	before := f.env.Count(t, "SELECT count(*) FROM deliveries")
	res, err := reconcileBatch(f, 100)
	if err != nil || res.Events != 0 || res.Deliveries != 0 {
		t.Fatalf("a healthy system has nothing to reconcile: %+v %v", res, err)
	}
	if f.env.Count(t, "SELECT count(*) FROM deliveries") != before || f.unmarked(t) != 0 {
		t.Fatal("a healthy sweep changed something")
	}
}

func TestReconcileLeavesRecentEventsToTheNormalPath(t *testing.T) {
	f := newFixture(t)
	org, user := f.owner.Org.ID, f.owner.Principal.User.ID
	d := f.destination(t, org, user, "D")
	f.subscribe(t, org, d.ID)
	fresh := f.event(t, `{"m":"fresh"}`)
	if res, err := reconcileBatch(f, 100); err != nil || res.Events != 0 {
		t.Fatalf("an event inside the grace period must be left alone: %+v %v", res, err)
	}
	if f.marker(t, fresh) != nil || f.deliveryCount(t, fresh) != 0 {
		t.Fatal("the grace period was not honored")
	}
}

func TestReconcileMarksEventsWithoutSubscriptionsAndNeverGrantsHistory(t *testing.T) {
	f := newFixture(t)
	org, user := f.owner.Org.ID, f.owner.Principal.User.ID
	d := f.destination(t, org, user, "D")
	event := f.event(t, `{"m":"nobody"}`)
	f.backdate(t, event, time.Now().UTC().Add(-3*time.Hour))

	if res, err := reconcileBatch(f, 100); err != nil || res.Events != 1 || res.Deliveries != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	// A subscription created AFTER the event never receives it (ADR 26)...
	f.insertSub(t, org, d.ID, time.Now().UTC(), sub{-60, nil})
	// ...not even when the row is verified again, which is what the backfill after the
	// migration does to every historical row.
	f.env.Exec(t, "UPDATE outbox SET fanned_out_at = NULL WHERE event_id = $1", event)
	if res, err := reconcileBatch(f, 100); err != nil || res.Events != 1 || res.Deliveries != 0 {
		t.Fatalf("re-verification must apply the same event-time rule: %+v %v", res, err)
	}
	if f.deliveryCount(t, event) != 0 {
		t.Fatal("a later subscription received an earlier event")
	}
}

func TestBackfillVerifiesHistoricalRowsInBoundedBatchesOldestFirst(t *testing.T) {
	f := newFixture(t)
	org, user := f.owner.Org.ID, f.owner.Principal.User.ID
	d := f.destination(t, org, user, "D")
	f.subscribeLongAgo(t, org, d.ID)
	var events []string
	for i := 0; i < 5; i++ {
		ev := f.event(t, fmt.Sprintf(`{"i":%d}`, i))
		// Historical rows as the migration leaves them: unmarked, some already fanned out.
		f.backdate(t, ev, time.Now().UTC().Add(-time.Duration(10-i)*24*time.Hour))
		events = append(events, ev)
	}
	// Event 1 had its fan-out before the migration: deliveries exist, marker is NULL.
	f.env.Exec(t, "INSERT INTO deliveries (organization_id, event_id, destination_id) VALUES ($1, $2, $3)", org, events[1], d.ID)

	want := []struct{ events, deliveries int64 }{{2, 1}, {2, 2}, {1, 1}, {0, 0}} // limit 2: oldest first
	for i, w := range want {
		res, err := reconcileBatch(f, 2)
		if err != nil || res.Events != w.events || res.Deliveries != w.deliveries {
			t.Fatalf("batch %d: %+v (%v), want %+v", i+1, res, err, w)
		}
		if i == 0 && (f.marker(t, events[0]) == nil || f.marker(t, events[1]) == nil || f.marker(t, events[2]) != nil) {
			t.Fatal("the first batch must take the two oldest rows")
		}
	}
	for _, ev := range events {
		if f.deliveryCount(t, ev) != 1 {
			t.Fatalf("event %s has %d deliveries, want exactly 1", ev, f.deliveryCount(t, ev))
		}
	}
}

func TestReconcileSkipsARowAnotherTransactionIsHandling(t *testing.T) {
	f := newFixture(t)
	org, user := f.owner.Org.ID, f.owner.Principal.User.ID
	d := f.destination(t, org, user, "D")
	f.subscribeLongAgo(t, org, d.ID)
	busy, free := f.event(t, `{"m":"busy"}`), f.event(t, `{"m":"free"}`)
	f.backdate(t, busy, time.Now().UTC().Add(-time.Hour))
	f.backdate(t, free, time.Now().UTC().Add(-time.Hour))

	locked, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- f.env.Pool.InTx(bg, func(tx pgx.Tx) error {
			// What a concurrent FanOut holds while it works on this event.
			if _, err := tx.Exec(bg, "UPDATE outbox SET fanned_out_at = now() WHERE event_id = $1", busy); err != nil {
				return err
			}
			close(locked)
			<-release
			return errors.New("rolled back on purpose")
		})
	}()
	<-locked
	res, err := reconcileBatch(f, 10)
	if err != nil || res.Events != 1 || f.marker(t, free) == nil || f.marker(t, busy) != nil {
		t.Fatalf("reconcile must skip the locked row and take the other: %+v %v", res, err)
	}
	close(release)
	<-done
	if res, err := reconcileBatch(f, 10); err != nil || res.Events != 1 || f.marker(t, busy) == nil {
		t.Fatalf("the skipped row is picked up once it is free: %+v %v", res, err)
	}
}

// ---- concurrency ----

func TestConcurrentReconcilersTakeDisjointRowsAndNeverDeadlockWithFanOut(t *testing.T) {
	f := newFixture(t)
	org, user := f.owner.Org.ID, f.owner.Principal.User.ID
	d1, d2 := f.destination(t, org, user, "D1"), f.destination(t, org, user, "D2")
	f.subscribeLongAgo(t, org, d1.ID)
	f.subscribeLongAgo(t, org, d2.ID)
	const total = 60
	events := make([]string, total)
	for i := range events {
		events[i] = f.event(t, fmt.Sprintf(`{"i":%d}`, i))
		f.backdate(t, events[i], time.Now().UTC().Add(-time.Hour))
	}

	var reconciled atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for w := 0; w < 6; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for {
				res, err := reconcileBatch(f, 4)
				if err != nil {
					t.Error(err)
					return
				}
				reconciled.Add(res.Events)
				if res.Events == 0 {
					return
				}
			}
		}()
	}
	// Meanwhile the normal path handles every third event, the same rows reconcilers want.
	for w := 0; w < 3; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := w; i < total; i += 3 {
				if _, err := fanOut(f, org, events[i]); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	close(start)
	wg.Wait()

	if f.unmarked(t) != 0 {
		t.Fatalf("%d rows are still unmarked", f.unmarked(t))
	}
	if got := f.env.Count(t, "SELECT count(*) FROM deliveries"); got != total*2 {
		t.Fatalf("%d deliveries, want %d (one per event and destination)", got, total*2)
	}
	if got := f.env.Count(t, "SELECT count(*) FROM (SELECT 1 FROM deliveries GROUP BY event_id, destination_id HAVING count(*) > 1) x"); got != 0 {
		t.Fatalf("%d duplicated (event, destination) pairs", got)
	}
	// A row is examined by exactly one reconciler or by the normal path, never twice.
	if reconciled.Load() > total {
		t.Fatalf("reconcilers examined %d rows for %d events", reconciled.Load(), total)
	}
}

func TestReconcileHonorsItsContext(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(bg)
	cancel()
	err := f.env.Pool.InTx(ctx, func(tx pgx.Tx) error {
		_, err := delivery.Reconcile(ctx, tx, delivery.ReconcileParams{
			Now: time.Now().UTC(), Lane: delivery.LaneLive, Grace: time.Minute, LiveWindow: time.Hour, Limit: 10,
		})
		return err
	})
	if err == nil {
		t.Fatal("a cancelled context must stop the sweep")
	}
}
