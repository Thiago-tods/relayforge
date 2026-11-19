package database_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Thiago-tods/relayforge/internal/delivery"
)

// opRow describes one delivery for the operational read-model tests. Zero times mean "not set":
// created defaults to now() and the other times to NULL (next_attempt_at to now()).
type opRow struct {
	org, dest, status, reason string
	created, completed        time.Time
	next, lease               time.Time
}

func nullableTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC()
}

// opDelivery inserts one delivery in an exact state, with the columns the read models depend on.
func (f *deliveryFixture) opDelivery(t *testing.T, r opRow) string {
	t.Helper()
	org, ep, dest := f.orgID, f.ep1, f.dest1
	if r.org != "" && r.org != f.orgID {
		org, ep, dest = r.org, f.ep2, f.dest2
	}
	if r.dest != "" {
		dest = r.dest
	}
	attempts, http := 1, any(nil)
	switch r.status {
	case "PENDING", "CANCELLED":
		attempts = 0
	case "SUCCEEDED":
		http = 200
	case "FAILED":
		http = 500
	}
	q, _ := f.env.Pool.Querier()
	var id string
	err := q.QueryRow(context.Background(), `INSERT INTO deliveries (organization_id, event_id, destination_id, status, attempt_count,
		next_attempt_at, lease_expires_at, claim_token, claimed_by, last_http_status, completed_at, created_at, failure_reason)
		VALUES ($1, $2, $3, $4, $5, COALESCE($6::timestamptz, now()), $7::timestamptz,
		        CASE WHEN $4 = 'IN_FLIGHT' THEN gen_random_uuid() END, CASE WHEN $4 = 'IN_FLIGHT' THEN 'worker' END,
		        $8::int, $9::timestamptz, COALESCE($10::timestamptz, now()), NULLIF($11, ''))
		RETURNING id::text`,
		org, f.newEvent(t, org, ep), dest, r.status, attempts, nullableTime(r.next), nullableTime(r.lease), http,
		nullableTime(r.completed), nullableTime(r.created), r.reason).Scan(&id)
	if err != nil {
		t.Fatalf("insert %+v: %v", r, err)
	}
	return id
}

// legacyFailed inserts a FAILED delivery without a failure reason, the way a delivery that failed
// before migration 00019 looks. The NOT VALID constraint that refuses new reasonless failures is
// dropped for the insert and added back exactly as the migration defines it.
func (f *deliveryFixture) legacyFailed(t *testing.T, r opRow) string {
	t.Helper()
	f.env.Exec(t, "ALTER TABLE deliveries DROP CONSTRAINT deliveries_failed_has_reason")
	r.status, r.reason = "FAILED", ""
	id := f.opDelivery(t, r)
	f.env.Exec(t, "ALTER TABLE deliveries ADD CONSTRAINT deliveries_failed_has_reason CHECK (status <> 'FAILED' OR failure_reason IS NOT NULL) NOT VALID")
	return id
}

// bulk inserts n deliveries (each with its own event) in one statement. values is the SQL value
// list for (status, attempt_count, last_http_status, completed_at, next_attempt_at, failure_reason).
// created_at is the database default.
func (f *deliveryFixture) bulk(t *testing.T, org string, n int, values string) {
	t.Helper()
	ep, dest := f.ep1, f.dest1
	if org != f.orgID {
		ep, dest = f.ep2, f.dest2
	}
	f.bulkTo(t, org, ep, dest, n, "status, attempt_count, last_http_status, completed_at, next_attempt_at, failure_reason", values, "now()")
}

// bulkTo inserts n deliveries of an organization (with its own endpoint and destination), one event
// each, in one statement. cols and values are the matching SQL column and value lists, and createdExpr
// is the SQL expression of created_at.
func (f *deliveryFixture) bulkTo(t *testing.T, org, ep, dest string, n int, cols, values, createdExpr string) {
	t.Helper()
	q, _ := f.env.Pool.Querier()
	_, err := q.Exec(context.Background(), `WITH ev AS (
		INSERT INTO webhook_events (id, organization_id, ingestion_endpoint_id, idempotency_key, payload, payload_sha256, content_type)
		SELECT gen_random_uuid(), $1, $2, 'bulk-' || $3::text || '-' || g, '{7d'::bytea, `+sha32+`, 'application/json'
		  FROM generate_series(1, $4) g
		RETURNING id)
		INSERT INTO deliveries (organization_id, event_id, destination_id, created_at, `+cols+`)
		SELECT $1, id, $5, `+createdExpr+`, `+values+` FROM ev`, org, ep, fmt.Sprint(f.keySeq.Add(1)), n, dest)
	if err != nil {
		t.Fatalf("bulk insert %d rows: %v", n, err)
	}
}

// extraOrg creates one more organization with an endpoint and an active destination, for tests that
// need several tenants in one table.
func (f *deliveryFixture) extraOrg(t *testing.T, slug string) (org, ep, dest string) {
	t.Helper()
	q, _ := f.env.Pool.Querier()
	ctx := context.Background()
	var user string
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(q.QueryRow(ctx, "INSERT INTO organizations (name, slug) VALUES ($1, $1) RETURNING id", slug).Scan(&org))
	must(q.QueryRow(ctx, "INSERT INTO users (email, normalized_email, password_hash, display_name) VALUES ($1, $1, 'h', 'U') RETURNING id", slug+"@x.io").Scan(&user))
	must(q.QueryRow(ctx, "INSERT INTO ingestion_endpoints (id, organization_id, public_id, name, signing_secret_ciphertext, encryption_key_version, created_by_user_id) "+
		"VALUES (gen_random_uuid(), $1, $2, 'ep', "+cipher+", 1, $3) RETURNING id", org, "ep_"+strings.Repeat("C", 22-len(slug))+slug, user).Scan(&ep))
	return org, ep, f.insertDestination(t, org, user, "Primary", validURL)
}

// plan returns the EXPLAIN output of a statement run with literal parameters (as a prepared
// statement, so the plan is the one the application's bind parameters get).
func (f *deliveryFixture) plan(t *testing.T, statement, params string) string {
	t.Helper()
	return f.planWith(t, "EXPLAIN", statement, params)
}

// analyze is plan with EXPLAIN (ANALYZE, BUFFERS): the statement really runs, and the output has the
// actual row counts, buffer reads, heap fetches and timings of every node.
func (f *deliveryFixture) analyze(t *testing.T, statement, params string) string {
	t.Helper()
	return f.planWith(t, "EXPLAIN (ANALYZE, BUFFERS, SUMMARY ON)", statement, params)
}

func (f *deliveryFixture) planWith(t *testing.T, explain, statement, params string) string {
	t.Helper()
	ctx := context.Background()
	var lines []string
	err := f.env.Pool.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "PREPARE opplan AS "+statement); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, explain+" EXECUTE opplan("+params+")")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				return err
			}
			lines = append(lines, line)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		rows.Close()
		_, err = tx.Exec(ctx, "DEALLOCATE opplan")
		return err
	})
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	return strings.Join(lines, "\n")
}

func (f *deliveryFixture) queue(t *testing.T, org string, now time.Time) delivery.QueueSnapshot {
	t.Helper()
	q, _ := f.env.Pool.Querier()
	out, err := delivery.QueueOf(context.Background(), q, org, now)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func (f *deliveryFixture) summary(t *testing.T, org string, from, to time.Time) delivery.DeliverySummary {
	t.Helper()
	q, _ := f.env.Pool.Querier()
	out, err := delivery.SummaryOf(context.Background(), q, org, from, to, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestOperationalIndexesExistAreValidAndAreUsedOnlyWhereIntended(t *testing.T) {
	f := newDeliveryFixture(t)
	q, _ := f.env.Pool.Querier()
	want := map[string][]string{
		"deliveries_org_pending_idx":   {"(organization_id, next_attempt_at)", "status = 'PENDING'"},
		"deliveries_org_completed_idx": {"(organization_id, completed_at)", "completed_at IS NOT NULL"},
		// The claim path keeps its global index, untouched.
		"deliveries_due_idx": {"(next_attempt_at, id)", "status = 'PENDING'"},
	}
	for name, fragments := range want {
		var def string
		var valid bool
		if err := q.QueryRow(context.Background(), "SELECT indexdef, (SELECT indisvalid FROM pg_index WHERE indexrelid = c.oid) FROM pg_indexes i JOIN pg_class c ON c.relname = i.indexname WHERE i.indexname = $1", name).Scan(&def, &valid); err != nil {
			t.Errorf("index %s: %v", name, err)
			continue
		}
		if !valid {
			t.Errorf("index %s is INVALID", name)
		}
		for _, fragment := range fragments {
			if !strings.Contains(def, fragment) {
				t.Errorf("index %s = %s, want it to contain %q", name, def, fragment)
			}
		}
	}
}

func TestMigration20RollsBackReappliesAndRepairsAnInterruptedRun(t *testing.T) {
	f := newDeliveryFixture(t)
	ctx := context.Background()
	db, err := f.env.Pool.SQLDB()
	if err != nil {
		t.Fatal(err)
	}
	provider := newGooseProvider(t, db)
	indexes := "SELECT count(*) FROM pg_indexes WHERE indexname IN ('deliveries_org_pending_idx', 'deliveries_org_completed_idx')"
	valid := "SELECT count(*) FROM pg_index WHERE indisvalid AND indexrelid IN ('deliveries_org_pending_idx'::regclass, 'deliveries_org_completed_idx'::regclass)"

	pendingID := f.mustDelivery(t, f.pending(t))
	if f.env.Count(t, indexes) != 2 || f.env.Count(t, valid) != 2 {
		t.Fatal("setup: the migration must have created two valid indexes")
	}

	if _, err := provider.DownTo(ctx, 19); err != nil {
		t.Fatalf("rollback to 19: %v", err)
	}
	if f.env.Count(t, indexes) != 0 {
		t.Fatal("the rollback must drop both indexes")
	}
	if f.env.Count(t, "SELECT count(*) FROM pg_indexes WHERE indexname = 'deliveries_due_idx'") != 1 {
		t.Fatal("the rollback must not touch the claim path's index")
	}
	if f.env.Count(t, "SELECT count(*) FROM deliveries WHERE id = $1", pendingID) != 1 {
		t.Fatal("the rollback must keep every delivery")
	}

	// An interrupted earlier run: an index of the same name exists with another definition.
	f.env.Exec(t, "CREATE INDEX deliveries_org_pending_idx ON deliveries (created_at)")
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("up must repair an interrupted run: %v", err)
	}
	if f.env.Count(t, indexes) != 2 || f.env.Count(t, valid) != 2 {
		t.Fatalf("after up: %d indexes, %d valid", f.env.Count(t, indexes), f.env.Count(t, valid))
	}
	if f.env.Count(t, "SELECT count(*) FROM pg_indexes WHERE indexname = 'deliveries_org_pending_idx' AND indexdef LIKE '%(organization_id, next_attempt_at) WHERE (status = ''PENDING''%'") != 1 {
		t.Fatal("the leftover index must be replaced by the real definition")
	}

	// Running it again is a no-op, and a second rollback and re-apply still works.
	if _, err := provider.DownTo(ctx, 19); err != nil {
		t.Fatalf("second rollback: %v", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("second up: %v", err)
	}
	if f.env.Count(t, valid) != 2 || f.env.Count(t, "SELECT count(*) FROM deliveries WHERE id = $1", pendingID) != 1 {
		t.Fatal("the indexes must be valid again and the data intact")
	}
}

func TestQueueHealthCountsPendingDueDelayedInFlightAndExpiredLeases(t *testing.T) {
	f := newDeliveryFixture(t)
	now := time.Now().UTC().Truncate(time.Second) // whole seconds: the ages below are then exact
	paused := f.insertDestination(t, f.orgID, f.userID, "Paused", validURL)
	f.env.Exec(t, "UPDATE destinations SET status = 'PAUSED' WHERE id = $1", paused)

	// Empty organization: every number is zero, including the age.
	if got := f.queue(t, f.orgID, now); got != (delivery.QueueSnapshot{GeneratedAt: now}) {
		t.Fatalf("empty queue: %+v", got)
	}

	f.opDelivery(t, opRow{status: "PENDING", next: now.Add(-10 * time.Minute)})
	f.opDelivery(t, opRow{status: "PENDING", next: now.Add(-time.Hour)}) // the oldest due
	f.opDelivery(t, opRow{status: "PENDING", next: now.Add(-5 * time.Minute), dest: paused})
	f.opDelivery(t, opRow{status: "PENDING", next: now.Add(time.Hour)})
	f.opDelivery(t, opRow{status: "PENDING", next: now.Add(24 * time.Hour)})
	f.opDelivery(t, opRow{status: "IN_FLIGHT", lease: now.Add(time.Minute)})
	f.opDelivery(t, opRow{status: "IN_FLIGHT", lease: now.Add(2 * time.Minute)})
	f.opDelivery(t, opRow{status: "IN_FLIGHT", lease: now.Add(-time.Minute)}) // expired, still in flight
	// Terminal rows and another organization's rows never count.
	f.opDelivery(t, opRow{status: "SUCCEEDED", completed: now})
	f.opDelivery(t, opRow{status: "FAILED", reason: "RETRIES_EXHAUSTED", completed: now})
	f.opDelivery(t, opRow{status: "CANCELLED", completed: now})
	f.opDelivery(t, opRow{org: f.org2, status: "PENDING", next: now.Add(-48 * time.Hour)})
	f.opDelivery(t, opRow{org: f.org2, status: "IN_FLIGHT", lease: now.Add(-time.Hour)})

	got := f.queue(t, f.orgID, now)
	if got.Pending != 5 || got.Due != 3 || got.Delayed != 2 || got.InFlight != 3 || got.ExpiredLeases != 1 || got.Capped {
		t.Fatalf("queue: %+v", got)
	}
	if got.Pending != got.Due+got.Delayed {
		t.Fatalf("below the cap pending must equal due + delayed: %+v", got)
	}
	// The oldest due delivery was due an hour ago. The paused destination's delivery counts as due.
	if got.OldestDueAgeSeconds != 3600 {
		t.Fatalf("oldest due age = %d, want exactly 3600", got.OldestDueAgeSeconds)
	}
	if !got.GeneratedAt.Equal(now) {
		t.Fatalf("generatedAt %v, want %v", got.GeneratedAt, now)
	}

	other := f.queue(t, f.org2, now)
	if other.Pending != 1 || other.Due != 1 || other.Delayed != 0 || other.InFlight != 1 || other.ExpiredLeases != 1 || other.OldestDueAgeSeconds != 48*3600 {
		t.Fatalf("the other organization sees its own rows only: %+v", other)
	}
	if none := f.queue(t, "00000000-0000-4000-8000-000000000000", now); none != (delivery.QueueSnapshot{GeneratedAt: now}) {
		t.Fatalf("an unknown organization has an empty queue: %+v", none)
	}

	// Only delayed deliveries: nothing is due, so the age is 0 rather than negative.
	f.env.Exec(t, "UPDATE deliveries SET next_attempt_at = $2 WHERE organization_id = $1 AND status = 'PENDING'", f.org2, now.Add(time.Hour))
	if later := f.queue(t, f.org2, now); later.Due != 0 || later.Delayed != 1 || later.OldestDueAgeSeconds != 0 {
		t.Fatalf("delayed only: %+v", later)
	}
}

func TestDeliverySummaryCountsTwoIndependentHalfOpenWindows(t *testing.T) {
	f := newDeliveryFixture(t)
	T := time.Now().UTC().Truncate(time.Second).Add(-48 * time.Hour)
	W := time.Hour
	to := T.Add(W)
	m := func(d time.Duration) time.Time { return T.Add(d) }

	f.opDelivery(t, opRow{status: "PENDING", created: T}) // created exactly at from: included
	f.opDelivery(t, opRow{status: "IN_FLIGHT", created: m(10 * time.Minute), lease: time.Now().Add(time.Minute)})
	f.opDelivery(t, opRow{status: "SUCCEEDED", created: m(20 * time.Minute), completed: m(25 * time.Minute)})
	for _, reason := range []string{"DESTINATION_REJECTED", "REDIRECT_REFUSED", "EGRESS_BLOCKED", "RETRIES_EXHAUSTED", "RETRIES_EXHAUSTED", "INTERNAL_ERROR"} {
		f.opDelivery(t, opRow{status: "FAILED", reason: reason, created: m(time.Minute), completed: m(2 * time.Minute)})
	}
	f.legacyFailed(t, opRow{created: m(time.Minute), completed: m(2 * time.Minute)})
	f.opDelivery(t, opRow{status: "CANCELLED", created: m(30 * time.Minute), completed: m(31 * time.Minute)})

	// Boundaries. Each row is in exactly the windows noted.
	f.opDelivery(t, opRow{status: "SUCCEEDED", created: m(-time.Hour), completed: to.Add(-time.Microsecond)})                         // completed only (last instant)
	f.opDelivery(t, opRow{status: "SUCCEEDED", created: m(5 * time.Minute), completed: to})                                           // created only (completed at `to`)
	f.opDelivery(t, opRow{status: "SUCCEEDED", created: to, completed: to.Add(5 * time.Minute)})                                      // neither (created at `to`)
	f.opDelivery(t, opRow{status: "SUCCEEDED", created: m(-10 * time.Minute), completed: T})                                          // completed only (exactly `from`)
	f.opDelivery(t, opRow{status: "SUCCEEDED", created: T.Add(-time.Microsecond), completed: T.Add(-time.Microsecond)})               // neither (just before `from`)
	f.opDelivery(t, opRow{status: "FAILED", reason: "RETRIES_EXHAUSTED", created: m(-3 * time.Hour), completed: m(30 * time.Minute)}) // completed only

	// Another organization's rows, in the same window, must not appear.
	for i := 0; i < 3; i++ {
		f.opDelivery(t, opRow{org: f.org2, status: "SUCCEEDED", created: m(time.Minute), completed: m(2 * time.Minute)})
	}
	f.opDelivery(t, opRow{org: f.org2, status: "FAILED", reason: "EGRESS_BLOCKED", created: m(time.Minute), completed: m(2 * time.Minute)})

	got := f.summary(t, f.orgID, T, to)
	if got.Capped {
		t.Fatal("nothing is near the cap")
	}
	wantCreated := delivery.CreatedCohort{Total: 12, ByStatus: delivery.StatusCounts{Pending: 1, InFlight: 1, Succeeded: 2, Failed: 7, Cancelled: 1}}
	if got.Created != wantCreated {
		t.Fatalf("created cohort:\n got %+v\nwant %+v", got.Created, wantCreated)
	}
	s := got.Completed
	wantReasons := delivery.ReasonCounts{DestinationRejected: 1, RedirectRefused: 1, EgressBlocked: 1, RetriesExhausted: 3, InternalError: 1, Unrecorded: 1}
	if s.Succeeded != 3 || s.Failed != 8 || s.Cancelled != 1 || s.ByFailureReason != wantReasons {
		t.Fatalf("completed cohort:\n got %+v\nwant succeeded 3 failed 8 cancelled 1 %+v", s, wantReasons)
	}
	// Arithmetic holds below the cap.
	c := got.Created.ByStatus
	if got.Created.Total != c.Pending+c.InFlight+c.Succeeded+c.Failed+c.Cancelled {
		t.Fatal("created total must equal the sum of its statuses")
	}
	r := s.ByFailureReason
	if s.Failed != r.DestinationRejected+r.RedirectRefused+r.EgressBlocked+r.RetriesExhausted+r.InternalError+r.Unrecorded {
		t.Fatal("failed must equal the sum of the failure reasons")
	}
	// 8 failed of 11 that succeeded or failed. The cancelled delivery is not in the denominator.
	if s.FailureRate == nil || *s.FailureRate != 8.0/11.0 {
		t.Fatalf("failure rate = %v, want 8/11", s.FailureRate)
	}
	if !got.From.Equal(T) || !got.To.Equal(to) {
		t.Fatalf("the response must echo the window: %v %v", got.From, got.To)
	}

	// The same window shifted by one hour sees none of these deliveries as created, and only the
	// ones completed inside it as completed.
	empty := f.summary(t, f.orgID, T.Add(24*time.Hour), T.Add(25*time.Hour))
	if empty.Created.Total != 0 || empty.Completed.Succeeded != 0 || empty.Completed.Failed != 0 || empty.Completed.FailureRate != nil || empty.Capped {
		t.Fatalf("an empty window: %+v", empty)
	}
	// A window with only cancelled deliveries has a zero denominator: no rate.
	only := f.summary(t, f.orgID, m(30*time.Minute+30*time.Second), m(32*time.Minute))
	if only.Completed.Cancelled != 1 || only.Completed.Succeeded != 0 || only.Completed.Failed != 0 || only.Completed.FailureRate != nil {
		t.Fatalf("a window with one cancelled delivery: %+v", only.Completed)
	}

	other := f.summary(t, f.org2, T, to)
	if other.Created.Total != 4 || other.Completed.Succeeded != 3 || other.Completed.Failed != 1 || other.Completed.ByFailureReason.EgressBlocked != 1 || other.Completed.ByFailureReason.Unrecorded != 0 {
		t.Fatalf("the other organization: %+v", other)
	}
}

// TestOperationalReadsStayConsistentWhileWorkersUpdate runs the two read models while deliveries
// are continuously claimed and put back to PENDING. Each response comes from one statement, so
// every invariant must hold in every response; with several statements, a delivery moving between
// them would be counted twice or not at all.
func TestOperationalReadsStayConsistentWhileWorkersUpdate(t *testing.T) {
	f := newDeliveryFixture(t)
	ctx := context.Background()
	const n = 40
	for i := 0; i < n; i++ {
		f.mustDelivery(t, dv{org: f.orgID, event: f.newEvent(t, f.orgID, f.ep1), dest: f.dest1, max: 50})
	}
	q, _ := f.env.Pool.Querier()

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for w := 0; w < 3; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for cycle := 0; cycle < 25; cycle++ {
				select {
				case <-stop:
					return
				default:
				}
				var id string
				err := q.QueryRow(ctx, `UPDATE deliveries SET status = 'IN_FLIGHT', attempt_count = attempt_count + 1,
					lease_expires_at = now() + interval '1 minute', claim_token = gen_random_uuid(), claimed_by = 'w'
					WHERE id = (SELECT id FROM deliveries WHERE organization_id = $1 AND status = 'PENDING' ORDER BY random() LIMIT 1 FOR UPDATE SKIP LOCKED)
					RETURNING id::text`, f.orgID).Scan(&id)
				if errors.Is(err, pgx.ErrNoRows) {
					continue // no pending row right now
				}
				if err != nil {
					t.Errorf("claim: %v", err)
					return
				}
				if _, err := q.Exec(ctx, `UPDATE deliveries SET status = 'PENDING', lease_expires_at = NULL, claim_token = NULL, claimed_by = NULL,
					next_attempt_at = now() WHERE id = $1`, id); err != nil {
					t.Errorf("put back: %v", err)
					return
				}
			}
		}()
	}

	for i := 0; i < 120; i++ {
		now := time.Now().UTC()
		got := f.queue(t, f.orgID, now)
		if got.Pending+got.InFlight != n {
			t.Fatalf("pending %d + in flight %d != %d: a delivery was lost or counted twice", got.Pending, got.InFlight, n)
		}
		if got.Due+got.Delayed != got.Pending || got.ExpiredLeases != 0 || got.Capped {
			t.Fatalf("inconsistent queue: %+v", got)
		}
		sum := f.summary(t, f.orgID, now.Add(-time.Hour), now.Add(time.Hour))
		c := sum.Created.ByStatus
		if sum.Created.Total != n || c.Pending+c.InFlight != n || c.Succeeded+c.Failed+c.Cancelled != 0 {
			t.Fatalf("inconsistent summary: %+v", sum.Created)
		}
	}
	close(stop)
	wg.Wait()
}

// TestOperationalReadModelsAtScale seeds one large tenant and one small one in a single database,
// and checks the count cap, the query plans and that the claim path still uses its global index.
func TestOperationalReadModelsAtScale(t *testing.T) {
	if testing.Short() {
		t.Skip("seeds about 200,000 rows")
	}
	f := newDeliveryFixture(t)
	ctx := context.Background()
	now := time.Now().UTC()
	from, to := now.Add(-time.Hour), now.Add(time.Hour)
	const cap = delivery.CountCap
	succeeded := "'SUCCEEDED', 1, 200, now(), now(), NULL"

	t.Run("just below the cap every count is exact", func(t *testing.T) {
		f.bulk(t, f.orgID, cap-2, succeeded)
		got := f.summary(t, f.orgID, from, to)
		if got.Capped || got.Created.Total != cap-2 || got.Created.ByStatus.Succeeded != cap-2 || got.Completed.Succeeded != cap-2 {
			t.Fatalf("%+v", got)
		}
		if got.Completed.FailureRate == nil || *got.Completed.FailureRate != 0 {
			t.Fatalf("no failures: the rate is 0, got %v", got.Completed.FailureRate)
		}
	})
	t.Run("a failure next to a large count still has a rate below the cap", func(t *testing.T) {
		f.opDelivery(t, opRow{status: "FAILED", reason: "RETRIES_EXHAUSTED", completed: now})
		got := f.summary(t, f.orgID, from, to)
		if got.Capped || got.Created.Total != cap-1 || got.Completed.Failed != 1 || got.Completed.FailureRate == nil ||
			*got.Completed.FailureRate != 1.0/float64(cap-1) {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("at the cap counters saturate, the response says so and the rate disappears", func(t *testing.T) {
		f.bulk(t, f.orgID, 3, succeeded) // succeeded is now cap + 1 in reality
		got := f.summary(t, f.orgID, from, to)
		if !got.Capped {
			t.Fatal("capped must be true")
		}
		if got.Completed.Succeeded != cap || got.Created.Total != cap || got.Created.ByStatus.Succeeded != cap {
			t.Fatalf("saturated counters must report exactly the cap: %+v", got)
		}
		if got.Completed.FailureRate != nil {
			t.Fatalf("a rate over a saturated count must be null, got %v", *got.Completed.FailureRate)
		}
		// Counters that did not reach the cap stay exact even when the response is capped.
		if got.Completed.Failed != 1 || got.Completed.ByFailureReason.RetriesExhausted != 1 || got.Created.ByStatus.Failed != 1 {
			t.Fatalf("an unsaturated counter must stay exact: %+v", got)
		}
	})
	t.Run("the queue saturates pending and due but counts delayed on its own", func(t *testing.T) {
		f.bulk(t, f.orgID, cap+1, "'PENDING', 0, NULL, NULL, now() - interval '1 hour', NULL")
		for i := 0; i < 3; i++ {
			f.opDelivery(t, opRow{status: "PENDING", next: now.Add(time.Hour)})
		}
		f.opDelivery(t, opRow{status: "IN_FLIGHT", lease: now.Add(time.Minute)})
		f.opDelivery(t, opRow{status: "IN_FLIGHT", lease: now.Add(-time.Minute)})
		got := f.queue(t, f.orgID, time.Now().UTC()) // a fresh clock: the rows were stamped by the database while seeding
		if !got.Capped || got.Pending != cap || got.Due != cap {
			t.Fatalf("pending and due must saturate: %+v", got)
		}
		// Subtracting two saturated counts would give 0; delayed is counted by its own predicate.
		if got.Delayed != 3 {
			t.Fatalf("delayed = %d, want 3 (independent of the saturated pending and due)", got.Delayed)
		}
		if got.InFlight != 2 || got.ExpiredLeases != 1 {
			t.Fatalf("in flight counts stay exact: %+v", got)
		}
		if got.OldestDueAgeSeconds < 3599 || got.OldestDueAgeSeconds > 3700 {
			t.Fatalf("the oldest due age stays exact under saturation, got %d", got.OldestDueAgeSeconds)
		}
	})

	// A small tenant next to the big one: this is where the organization-scoped indexes matter.
	f.bulk(t, f.org2, 200, "'PENDING', 0, NULL, NULL, now() - interval '5 minutes', NULL")
	f.bulk(t, f.org2, 200, succeeded)
	f.env.Exec(t, "ANALYZE deliveries")
	orgParam := "'" + f.org2 + "'"
	window := fmt.Sprintf("%s, '%s', '%s'", orgParam, from.Format(time.RFC3339Nano), to.Format(time.RFC3339Nano))

	t.Run("the small tenant's queue uses its organization index and no table scan", func(t *testing.T) {
		plan := f.plan(t, delivery.QueueStatement, orgParam+", '"+now.Format(time.RFC3339Nano)+"'")
		t.Logf("queue plan:\n%s", plan)
		if !strings.Contains(plan, "deliveries_org_pending_idx") {
			t.Errorf("the queue must use deliveries_org_pending_idx")
		}
		if strings.Contains(plan, "Seq Scan on deliveries") {
			t.Errorf("the queue must not scan the table")
		}
		if strings.Contains(plan, "deliveries_due_idx") {
			t.Errorf("the organization-scoped queue must not walk the global due index")
		}
	})
	t.Run("the small tenant's summary uses the created and completed indexes", func(t *testing.T) {
		plan := f.plan(t, delivery.SummaryStatement, window)
		t.Logf("summary plan:\n%s", plan)
		for _, idx := range []string{"deliveries_org_completed_idx", "deliveries_organization_idx"} {
			if !strings.Contains(plan, idx) {
				t.Errorf("the summary must use %s", idx)
			}
		}
		if strings.Contains(plan, "Seq Scan on deliveries") {
			t.Errorf("the summary must not scan the table")
		}
	})
	t.Run("the claim path keeps using the global due index", func(t *testing.T) {
		// The statement ClaimDue itself runs (delivery.ClaimDueStatement is the constant ClaimDue executes, not a
		// copy), planned with the same four parameters: now, limit, lease seconds, worker id.
		plan := f.plan(t, delivery.ClaimDueStatement, "now(), 8, 60, 'scale-test'")
		t.Logf("claim plan:\n%s", plan)
		if !strings.Contains(plan, "deliveries_due_idx") {
			t.Errorf("the claim must use deliveries_due_idx")
		}
		if strings.Contains(plan, "deliveries_org_pending_idx") {
			t.Errorf("the claim must not use the organization index")
		}
		// And the real function still claims, with the new indexes present.
		var claimed []delivery.Claimed
		err := f.env.Pool.InTx(ctx, func(tx pgx.Tx) error {
			var err error
			claimed, err = delivery.ClaimDue(ctx, tx, time.Now().UTC(), 8, time.Minute, "scale-test")
			return err
		})
		if err != nil || len(claimed) != 8 {
			t.Fatalf("ClaimDue claimed %d deliveries: %v", len(claimed), err)
		}
	})
}

// indexState is what the PostgreSQL catalog says about one index: whether it exists, whether it is
// usable (pg_index.indisvalid), its identity (pg_class.oid, to tell a rebuilt index from a kept one)
// and its definition as PostgreSQL prints it.
type indexState struct {
	exists, valid bool
	oid           uint32
	def           string
}

func (f *deliveryFixture) indexState(t *testing.T, name string) indexState {
	t.Helper()
	q, _ := f.env.Pool.Querier()
	var s indexState
	err := q.QueryRow(context.Background(), `SELECT c.oid::int8, i.indisvalid, pg_get_indexdef(c.oid)
		  FROM pg_class c JOIN pg_index i ON i.indexrelid = c.oid
		 WHERE c.relname = $1 AND c.relkind = 'i'`, name).Scan(&s.oid, &s.valid, &s.def)
	if errors.Is(err, pgx.ErrNoRows) {
		return indexState{}
	}
	if err != nil {
		t.Fatalf("catalog state of %s: %v", name, err)
	}
	s.exists = true
	return s
}

// TestMigration20RepairsInvalidAndPartiallyBuiltIndexes drives the real goose provider through the
// failure shapes of a CONCURRENTLY build and checks PostgreSQL's own catalog after every step.
//
// A CREATE INDEX CONCURRENTLY that fails midway leaves an INVALID index behind (pg_index.indisvalid =
// false). The tests produce exactly that: a unique index built concurrently over duplicate rows.
func TestMigration20RepairsInvalidAndPartiallyBuiltIndexes(t *testing.T) {
	const pending, completed = "deliveries_org_pending_idx", "deliveries_org_completed_idx"
	f := newDeliveryFixture(t)
	ctx := context.Background()
	db, err := f.env.Pool.SQLDB()
	if err != nil {
		t.Fatal(err)
	}
	provider := newGooseProvider(t, db)
	q, _ := f.env.Pool.Querier()

	// The golden definitions are what a clean migration produces, read back from the catalog.
	goldPending, goldCompleted := f.indexState(t, pending), f.indexState(t, completed)
	for name, g := range map[string]indexState{pending: goldPending, completed: goldCompleted} {
		if !g.exists || !g.valid {
			t.Fatalf("setup: %s after a clean migration: %+v", name, g)
		}
	}
	for name, fragments := range map[string][]string{
		pending:   {"CREATE INDEX " + pending, "USING btree (organization_id, next_attempt_at)", "WHERE (status = 'PENDING'"},
		completed: {"CREATE INDEX " + completed, "USING btree (organization_id, completed_at)", "WHERE (completed_at IS NOT NULL)"},
	} {
		def := f.indexState(t, name).def
		for _, fragment := range fragments {
			if !strings.Contains(def, fragment) {
				t.Errorf("%s definition %q lacks %q", name, def, fragment)
			}
		}
	}

	// Duplicates that make a unique concurrent build fail: two PENDING and two completed rows of one organization.
	now := time.Now().UTC()
	pendingRow := f.opDelivery(t, opRow{status: "PENDING", next: now})
	f.opDelivery(t, opRow{status: "PENDING", next: now})
	f.opDelivery(t, opRow{status: "SUCCEEDED", completed: now})
	f.opDelivery(t, opRow{status: "SUCCEEDED", completed: now})
	rows := f.env.Count(t, "SELECT count(*) FROM deliveries")

	rollBack := func() {
		t.Helper()
		if _, err := provider.DownTo(ctx, 19); err != nil {
			t.Fatalf("rollback to 19: %v", err)
		}
		if s := f.indexState(t, pending); s.exists {
			t.Fatalf("the rollback must drop %s: %+v", pending, s)
		}
		if s := f.indexState(t, completed); s.exists {
			t.Fatalf("the rollback must drop %s: %+v", completed, s)
		}
		if s := f.indexState(t, "deliveries_due_idx"); !s.exists || !s.valid {
			t.Fatalf("the rollback must leave the claim path's index alone: %+v", s)
		}
		if f.env.Count(t, "SELECT count(*) FROM deliveries") != rows {
			t.Fatal("the rollback must keep every delivery")
		}
	}
	// invalid leaves behind what an interrupted CREATE INDEX CONCURRENTLY leaves: an INVALID index.
	invalid := func(name, statement string) {
		t.Helper()
		if _, err := q.Exec(ctx, statement); err == nil {
			t.Fatalf("setup: %q should fail on the duplicate rows", statement)
		}
		s := f.indexState(t, name)
		if !s.exists || s.valid {
			t.Fatalf("setup: %s must exist and be INVALID, got %+v", name, s)
		}
		t.Logf("catalog before the repair: %s exists=%v indisvalid=%v", name, s.exists, s.valid)
	}
	// assertRepaired checks that both indexes are valid and have exactly the golden definitions.
	assertRepaired := func(label string) {
		t.Helper()
		for name, gold := range map[string]indexState{pending: goldPending, completed: goldCompleted} {
			s := f.indexState(t, name)
			t.Logf("catalog %s: %s exists=%v indisvalid=%v def=%s", label, name, s.exists, s.valid, s.def)
			if !s.exists || !s.valid || s.def != gold.def {
				t.Errorf("%s: %s = %+v, want a valid index defined as %q", label, name, s, gold.def)
			}
		}
	}

	t.Run("an invalid pending index and no completed index", func(t *testing.T) {
		rollBack()
		invalid(pending, "CREATE UNIQUE INDEX CONCURRENTLY "+pending+" ON deliveries (organization_id) WHERE status = 'PENDING'")
		if _, err := provider.Up(ctx); err != nil {
			t.Fatalf("up must repair an invalid index: %v", err)
		}
		assertRepaired("after repairing an invalid pending index")
	})

	t.Run("both indexes invalid", func(t *testing.T) {
		rollBack()
		invalid(pending, "CREATE UNIQUE INDEX CONCURRENTLY "+pending+" ON deliveries (organization_id) WHERE status = 'PENDING'")
		invalid(completed, "CREATE UNIQUE INDEX CONCURRENTLY "+completed+" ON deliveries (organization_id) WHERE completed_at IS NOT NULL")
		if _, err := provider.Up(ctx); err != nil {
			t.Fatalf("up must repair two invalid indexes: %v", err)
		}
		assertRepaired("after repairing two invalid indexes")
	})

	t.Run("only the pending index exists, the completed one is missing", func(t *testing.T) {
		rollBack()
		f.env.Exec(t, "CREATE INDEX CONCURRENTLY "+pending+" ON deliveries (organization_id, next_attempt_at) WHERE status = 'PENDING'")
		before := f.indexState(t, pending)
		if !before.exists || !before.valid || f.indexState(t, completed).exists {
			t.Fatalf("setup: pending exists and is valid, completed is missing: %+v", before)
		}
		if _, err := provider.Up(ctx); err != nil {
			t.Fatalf("up must finish a half-applied migration: %v", err)
		}
		assertRepaired("after finishing a half-applied migration")
		// The valid pending index is dropped and rebuilt by the rerun (the migration is a whole, idempotent
		// file): it is a new index, with the same definition. Only reads of the read models, never the claim
		// path, depend on it while it is rebuilt.
		if after := f.indexState(t, pending); after.oid == before.oid {
			t.Errorf("the rerun rebuilds the pending index (same oid %d)", after.oid)
		}
	})

	t.Run("only the completed index exists, the pending one is missing", func(t *testing.T) {
		rollBack()
		f.env.Exec(t, "CREATE INDEX CONCURRENTLY "+completed+" ON deliveries (organization_id, completed_at) WHERE completed_at IS NOT NULL")
		if _, err := provider.Up(ctx); err != nil {
			t.Fatalf("up must finish a half-applied migration: %v", err)
		}
		assertRepaired("after finishing a half-applied migration (completed first)")
	})

	t.Run("up then down then up, and a rerun on a finished migration is a no-op", func(t *testing.T) {
		assertRepaired("starting point")
		if _, err := provider.Up(ctx); err != nil {
			t.Fatalf("up with nothing to do: %v", err)
		}
		oid := f.indexState(t, pending).oid
		assertRepaired("a rerun of a finished migration")
		if f.indexState(t, pending).oid != oid {
			t.Error("a finished migration must not rebuild anything")
		}
		rollBack()
		if _, err := provider.Up(ctx); err != nil {
			t.Fatalf("up after down: %v", err)
		}
		assertRepaired("after down and up")
		if f.env.Count(t, "SELECT count(*) FROM deliveries WHERE id = $1", pendingRow) != 1 {
			t.Fatal("the data must be intact")
		}
	})
}
