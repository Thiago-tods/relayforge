package delivery_test

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Thiago-tods/relayforge/internal/delivery"
)

// poisonOn installs a trigger that makes the delivery insert fail with the given SQLSTATE
// condition name when the condition holds. It stands for a deterministic defect (a data
// or integrity error) or, with a transient condition, for an outage.
func (f *fixture) poisonOn(t *testing.T, condition, errcode string) {
	t.Helper()
	f.env.Exec(t, fmt.Sprintf(`CREATE FUNCTION inject_poison() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN IF %s THEN RAISE EXCEPTION 'injected failure with a value: SECRET-VALUE-42' USING ERRCODE = '%s'; END IF; RETURN NEW; END $$`, condition, errcode))
	f.env.Exec(t, "CREATE TRIGGER inject_poison BEFORE INSERT ON deliveries FOR EACH ROW EXECUTE FUNCTION inject_poison()")
}

func (f *fixture) clearPoison(t *testing.T) {
	t.Helper()
	f.env.Exec(t, "DROP TRIGGER inject_poison ON deliveries")
	f.env.Exec(t, "DROP FUNCTION inject_poison()")
}

// lostEvents creates n events whose fan-out was lost, oldest first, in the live lane.
func (f *fixture) lostEvents(t *testing.T, n int) []string {
	t.Helper()
	var out []string
	for i := 0; i < n; i++ {
		ev := f.event(t, fmt.Sprintf(`{"lost":%d}`, i))
		f.backdate(t, ev, time.Now().UTC().Add(-time.Duration(n-i+10)*time.Minute))
		out = append(out, ev)
	}
	return out
}

func TestHealthyRowsProcessAroundAPoisonRow(t *testing.T) {
	f := newFixture(t)
	org, user := f.owner.Org.ID, f.owner.Principal.User.ID
	d := f.destination(t, org, user, "D")
	f.subscribeLongAgo(t, org, d.ID)
	events := f.lostEvents(t, 6)
	poison := events[0] // the oldest: first in line, where it used to block everything
	f.poisonOn(t, fmt.Sprintf("NEW.event_id = '%s'::uuid", poison), "integrity_constraint_violation")

	res, err := reconcileLane(f, delivery.LaneLive, 100)
	if err != nil {
		t.Fatalf("an isolated row failure must not fail the sweep: %v", err)
	}
	if res.Examined != 6 || res.Events != 5 || res.Deliveries != 5 || len(res.Failed) != 1 {
		t.Fatalf("result %+v", res)
	}
	failed := res.Failed[0]
	if failed.EventID != poison || failed.OrganizationID != org || failed.OutboxID == "" || failed.Class != "sqlstate:23000" {
		t.Fatalf("failure report %+v", failed)
	}
	if strings.Contains(fmt.Sprintf("%+v", failed), "SECRET-VALUE-42") {
		t.Fatal("the report carries a value from the database error")
	}
	// The poison row stays unmarked, so it is never presented as fanned out or discarded.
	if f.marker(t, poison) != nil || f.deliveryCount(t, poison) != 0 {
		t.Fatal("a failed row must stay unmarked and leave no delivery")
	}
	for _, ev := range events[1:] {
		if f.marker(t, ev) == nil || f.deliveryCount(t, ev) != 1 {
			t.Fatalf("healthy row %s was held up by the poison row", ev)
		}
	}
}

func TestAFailedRowLeavesNoPartialDeliveries(t *testing.T) {
	f := newFixture(t)
	org, user := f.owner.Org.ID, f.owner.Principal.User.ID
	d1, d2 := f.destination(t, org, user, "D1"), f.destination(t, org, user, "D2")
	f.subscribeLongAgo(t, org, d1.ID)
	f.subscribeLongAgo(t, org, d2.ID)
	events := f.lostEvents(t, 3)
	// The event fails for ONE of its two destinations: whatever was inserted for the
	// other one before the error must be rolled back with it.
	f.poisonOn(t, fmt.Sprintf("NEW.event_id = '%s'::uuid AND NEW.destination_id = '%s'::uuid", events[1], d2.ID), "check_violation")

	res, err := reconcileLane(f, delivery.LaneLive, 100)
	if err != nil || len(res.Failed) != 1 || res.Events != 2 || res.Deliveries != 4 {
		t.Fatalf("%+v %v", res, err)
	}
	if res.Failed[0].Class != "sqlstate:23514" {
		t.Fatalf("class %q", res.Failed[0].Class)
	}
	if f.deliveryCount(t, events[1]) != 0 {
		t.Fatalf("%d partial deliveries survived the failed row", f.deliveryCount(t, events[1]))
	}
}

func TestDataErrorsAreIsolatedToo(t *testing.T) {
	f := newFixture(t)
	org, user := f.owner.Org.ID, f.owner.Principal.User.ID
	d := f.destination(t, org, user, "D")
	f.subscribeLongAgo(t, org, d.ID)
	events := f.lostEvents(t, 3)
	f.poisonOn(t, fmt.Sprintf("NEW.event_id = '%s'::uuid", events[2]), "data_exception") // SQLSTATE class 22

	res, err := reconcileLane(f, delivery.LaneLive, 100)
	if err != nil || len(res.Failed) != 1 || res.Failed[0].Class != "sqlstate:22000" || res.Events != 2 {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestRemovingTheFaultLetsTheNextSweepRecoverTheRow(t *testing.T) {
	f := newFixture(t)
	org, user := f.owner.Org.ID, f.owner.Principal.User.ID
	d := f.destination(t, org, user, "D")
	f.subscribeLongAgo(t, org, d.ID)
	events := f.lostEvents(t, 4)
	f.poisonOn(t, fmt.Sprintf("NEW.event_id = '%s'::uuid", events[0]), "integrity_constraint_violation")

	for sweep := 0; sweep < 3; sweep++ { // the row keeps failing, sweep after sweep, and costs nothing else
		res, err := reconcileLane(f, delivery.LaneLive, 100)
		if err != nil || len(res.Failed) != 1 {
			t.Fatalf("sweep %d: %+v %v", sweep+1, res, err)
		}
	}
	if f.marker(t, events[0]) != nil {
		t.Fatal("a failing row was marked")
	}
	f.clearPoison(t)
	res, err := reconcileLane(f, delivery.LaneLive, 100)
	if err != nil || res.Events != 1 || res.Deliveries != 1 || len(res.Failed) != 0 {
		t.Fatalf("recovery sweep: %+v %v", res, err)
	}
	if f.marker(t, events[0]) == nil || f.deliveryCount(t, events[0]) != 1 {
		t.Fatal("the row did not recover once the fault was fixed")
	}
}

func TestSkippedRowsAreNotSelectedAgainInTheSameSweep(t *testing.T) {
	f := newFixture(t)
	org, user := f.owner.Org.ID, f.owner.Principal.User.ID
	d := f.destination(t, org, user, "D")
	f.subscribeLongAgo(t, org, d.ID)
	events := f.lostEvents(t, 4)
	f.poisonOn(t, fmt.Sprintf("NEW.event_id = '%s'::uuid", events[0]), "integrity_constraint_violation")

	first, err := reconcileLane(f, delivery.LaneLive, 2) // the poison row and one healthy row
	if err != nil || first.Examined != 2 || len(first.Failed) != 1 || first.Events != 1 {
		t.Fatalf("%+v %v", first, err)
	}
	second, err := reconcileLane(f, delivery.LaneLive, 2, first.Failed[0].OutboxID)
	if err != nil || second.Examined != 2 || len(second.Failed) != 0 || second.Events != 2 {
		t.Fatalf("with the failed row skipped the next batch moves on: %+v %v", second, err)
	}
}

// ---- errors that must NOT be isolated ----

func TestTransientErrorsAbortTheWholeSweep(t *testing.T) {
	for _, errcode := range []string{
		"serialization_failure", // 40001
		"deadlock_detected",     // 40P01
		"query_canceled",        // 57014: statement timeout
		"lock_not_available",    // 55P03
		"connection_failure",    // 08006
		"admin_shutdown",        // 57P01
	} {
		t.Run(errcode, func(t *testing.T) {
			f := newFixture(t)
			org, user := f.owner.Org.ID, f.owner.Principal.User.ID
			d := f.destination(t, org, user, "D")
			f.subscribeLongAgo(t, org, d.ID)
			events := f.lostEvents(t, 4)
			// Only one row triggers it, so isolating it would let three healthy rows through.
			f.poisonOn(t, fmt.Sprintf("NEW.event_id = '%s'::uuid", events[2]), errcode)

			res, err := reconcileLane(f, delivery.LaneLive, 100)
			if err == nil {
				t.Fatalf("a transient error must fail the whole sweep, got %+v", res)
			}
			if len(res.Failed) != 0 {
				t.Fatalf("a transient error was reported as a row failure: %+v", res.Failed)
			}
			if f.unmarked(t) != 4 || f.env.Count(t, "SELECT count(*) FROM deliveries") != 0 {
				t.Fatal("an aborted sweep must leave every row untouched")
			}
		})
	}
}

// ---- concurrency ----

func TestConcurrentReconcilersStayCorrectWithAPoisonRow(t *testing.T) {
	f := newFixture(t)
	org, user := f.owner.Org.ID, f.owner.Principal.User.ID
	d := f.destination(t, org, user, "D")
	f.subscribeLongAgo(t, org, d.ID)
	const total = 40
	events := f.lostEvents(t, total)
	f.poisonOn(t, fmt.Sprintf("NEW.event_id = '%s'::uuid", events[3]), "integrity_constraint_violation")

	var marked, failures atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for w := 0; w < 6; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			var skip []string
			for idle := 0; idle < 3; {
				res, err := reconcileLane(f, delivery.LaneLive, 4, skip...)
				if err != nil {
					t.Error(err)
					return
				}
				marked.Add(res.Events)
				failures.Add(int64(len(res.Failed)))
				for _, fr := range res.Failed {
					skip = append(skip, fr.OutboxID)
				}
				if res.Examined == 0 {
					idle++
				} else {
					idle = 0
				}
			}
		}()
	}
	close(start)
	wg.Wait()

	if marked.Load() != total-1 {
		t.Fatalf("the reconcilers marked %d rows, want exactly %d", marked.Load(), total-1)
	}
	if failures.Load() < 1 || f.marker(t, events[3]) != nil || f.unmarked(t) != 1 {
		t.Fatalf("the poison row must stay the only unmarked row (failures reported %d, unmarked %d)", failures.Load(), f.unmarked(t))
	}
	if got := f.env.Count(t, "SELECT count(*) FROM deliveries"); got != total-1 {
		t.Fatalf("%d deliveries, want %d", got, total-1)
	}
}

// TestIsolationCostIsBoundedByTheBatch measures the fallback path: one poison row in a
// full batch costs one extra statement per row, once per sweep.
func TestIsolationCostIsBoundedByTheBatch(t *testing.T) {
	f := newFixture(t)
	org, user := f.owner.Org.ID, f.owner.Principal.User.ID
	d := f.destination(t, org, user, "D")
	f.subscribeLongAgo(t, org, d.ID)
	const batch = 500
	f.seedRecentBacklog(t, org, batch)
	var oldest string
	q, _ := f.env.Pool.Querier()
	if err := q.QueryRow(bg, "SELECT event_id::text FROM outbox WHERE fanned_out_at IS NULL ORDER BY created_at, id LIMIT 1").Scan(&oldest); err != nil {
		t.Fatal(err)
	}
	f.poisonOn(t, fmt.Sprintf("NEW.event_id = '%s'::uuid", oldest), "integrity_constraint_violation")

	start := time.Now()
	res, err := reconcileLane(f, delivery.LaneLive, batch)
	elapsed := time.Since(start)
	if err != nil || res.Examined != batch || res.Events != batch-1 || len(res.Failed) != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	t.Logf("fallback path for one poison row in a batch of %d: %v (set-based attempt + %d single-row statements)", batch, elapsed, batch)
}

// seedRecentBacklog inserts n unmarked events in the live lane, directly in SQL.
func (f *fixture) seedRecentBacklog(t *testing.T, org string, n int) {
	t.Helper()
	f.env.Exec(t, `
		WITH ev AS (
			INSERT INTO webhook_events (id, organization_id, ingestion_endpoint_id, idempotency_key, payload, payload_sha256, content_type, received_at)
			SELECT id, $1::uuid, $2::uuid, id::text, '\x78'::bytea, decode(repeat('00', 32), 'hex'), 'application/json', at
			  FROM (SELECT gen_random_uuid() AS id, now() - interval '10 minutes' - g * interval '1 second' AS at FROM generate_series(1, $3::int) g) s
			RETURNING id, received_at)
		INSERT INTO outbox (organization_id, event_id, message_type, payload, created_at, available_at, published_at)
		SELECT $1::uuid, id, 'webhook_event.accepted', '{}', received_at, received_at, received_at FROM ev`, org, f.ep, n)
}
