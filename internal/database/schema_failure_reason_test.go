package database_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Thiago-tods/relayforge/internal/delivery"
)

var failureReasons = []string{"DESTINATION_REJECTED", "REDIRECT_REFUSED", "EGRESS_BLOCKED", "RETRIES_EXHAUSTED", "INTERNAL_ERROR"}

func TestTheDatabaseTaxonomyMatchesTheGoTaxonomy(t *testing.T) {
	if len(failureReasons) != len(delivery.AllFailureReasons()) {
		t.Fatalf("the test list and delivery.AllFailureReasons() differ in size")
	}
	for i, r := range delivery.AllFailureReasons() {
		if string(r) != failureReasons[i] {
			t.Fatalf("reason %d: Go %q, test list %q", i, r, failureReasons[i])
		}
	}
	f := newDeliveryFixture(t)
	// Every Go reason is accepted by both columns; the CHECK lists carry no extra value.
	for _, r := range delivery.AllFailureReasons() {
		f.mustDelivery(t, dv{org: f.orgID, event: f.newEvent(t, f.orgID, f.ep1), dest: f.dest1, status: "FAILED", attempts: 1, httpStatus: 500, completed: "now()", reason: "'" + string(r) + "'"})
	}
	for _, table := range []string{"deliveries", "delivery_attempts"} {
		var def string
		col := map[string]string{"deliveries": "failure_reason", "delivery_attempts": "terminal_reason"}[table]
		q, _ := f.env.Pool.Querier()
		if err := q.QueryRow(context.Background(), `SELECT string_agg(pg_get_constraintdef(c.oid), ' ') FROM pg_constraint c
			JOIN pg_class r ON r.oid = c.conrelid WHERE r.relname = $1 AND c.contype = 'c' AND pg_get_constraintdef(c.oid) LIKE '%'||$2||'%'`, table, col).Scan(&def); err != nil {
			t.Fatal(err)
		}
		if got := strings.Count(def, "'") / 2; got < len(failureReasons) {
			t.Errorf("%s: the constraints list %d literals, want at least %d: %s", table, got, len(failureReasons), def)
		}
		for _, r := range failureReasons {
			if !strings.Contains(def, "'"+r+"'") {
				t.Errorf("%s: the constraint does not list %s", table, r)
			}
		}
	}
}

func TestFailureReasonColumnsAndConstraints(t *testing.T) {
	f := newDeliveryFixture(t)
	count := func(sql string, args ...any) int { return f.env.Count(t, sql, args...) }

	for _, c := range [][2]string{{"deliveries", "failure_reason"}, {"delivery_attempts", "terminal_reason"}} {
		if count(`SELECT count(*) FROM information_schema.columns WHERE table_name = $1 AND column_name = $2
			AND data_type = 'text' AND is_nullable = 'YES' AND column_default IS NULL`, c[0], c[1]) != 1 {
			t.Errorf("%s.%s must be a nullable text column without a default", c[0], c[1])
		}
	}
	// The consistency constraints are validated; the legacy-tolerant one is intentionally not.
	for name, validated := range map[string]bool{
		"deliveries_failure_reason_valid": true, "deliveries_failure_reason_only_when_failed": true, "deliveries_failed_has_reason": false,
		"delivery_attempts_terminal_reason_valid": true, "delivery_attempts_terminal_reason_not_success": true,
	} {
		if count("SELECT count(*) FROM pg_constraint WHERE conname = $1 AND convalidated = $2", name, validated) != 1 {
			t.Errorf("constraint %s must exist with convalidated=%v", name, validated)
		}
	}

	newDv := func(status string, mutate func(*dv)) dv {
		d := dv{org: f.orgID, event: f.newEvent(t, f.orgID, f.ep1), dest: f.dest1, status: status, attempts: 1}
		switch status {
		case "FAILED":
			d.httpStatus, d.completed = 500, "now()"
		case "SUCCEEDED":
			d.httpStatus, d.completed = 204, "now()"
		case "CANCELLED":
			d.completed = "now()"
		case "IN_FLIGHT":
			d.lease, d.token, d.by = inFlightLease, tokenExpr, "'w'"
		case "PENDING":
			d.attempts = 0
		}
		if mutate != nil {
			mutate(&d)
		}
		return d
	}

	for _, r := range failureReasons {
		f.mustDelivery(t, newDv("FAILED", func(d *dv) { d.reason = "'" + r + "'" }))
	}
	f.rejectDelivery(t, "an invented reason", newDv("FAILED", func(d *dv) { d.reason = "'BECAUSE'" }), "deliveries_failure_reason_valid")
	f.rejectDelivery(t, "a lowercase reason", newDv("FAILED", func(d *dv) { d.reason = "'internal_error'" }), "deliveries_failure_reason_valid")
	f.rejectDelivery(t, "an empty reason", newDv("FAILED", func(d *dv) { d.reason = "''" }), "deliveries_failure_reason_valid")
	f.rejectDelivery(t, "a new FAILED delivery without a reason", newDv("FAILED", func(d *dv) { d.legacy = true }), "deliveries_failed_has_reason")
	for _, status := range []string{"PENDING", "IN_FLIGHT", "SUCCEEDED", "CANCELLED"} {
		f.rejectDelivery(t, status+" with a reason", newDv(status, func(d *dv) { d.reason = "'INTERNAL_ERROR'" }), "deliveries_failure_reason_only_when_failed")
		f.mustDelivery(t, newDv(status, nil)) // and without one it is fine
	}

	// Attempts: the terminal reason is validated, never on a success, and immutable.
	attemptWith := "INSERT INTO delivery_attempts (organization_id, delivery_id, attempt_number, started_at, finished_at, outcome, http_status, duration_ms, terminal_reason) " +
		"VALUES ($1, $2, $3, now() - interval '1 second', now(), $4, $5, 5, $6)"
	inflight := func() string { return f.inFlightAt(t, f.orgID, f.dest1, f.ep1, 1) }
	f.reject(t, "an invented terminal reason", attemptWith, f.orgID, inflight(), 1, "HTTP_ERROR", 404, "BECAUSE")
	f.reject(t, "a terminal reason on a successful attempt", attemptWith, f.orgID, inflight(), 1, "SUCCESS", 204, "INTERNAL_ERROR")
	for _, r := range failureReasons {
		f.accept(t, "terminal reason "+r, attemptWith, f.orgID, inflight(), 1, "HTTP_ERROR", 404, r)
	}
	var withReason string
	q, _ := f.env.Pool.Querier()
	if err := q.QueryRow(context.Background(), "SELECT id::text FROM delivery_attempts WHERE terminal_reason = 'EGRESS_BLOCKED'").Scan(&withReason); err != nil {
		t.Fatal(err)
	}
	f.reject(t, "rewriting a terminal reason", "UPDATE delivery_attempts SET terminal_reason = 'INTERNAL_ERROR' WHERE id = $1", withReason)
	f.reject(t, "clearing a terminal reason", "UPDATE delivery_attempts SET terminal_reason = NULL WHERE id = $1", withReason)
	f.reject(t, "deleting an attempt", "DELETE FROM delivery_attempts WHERE id = $1", withReason)
}

// The transition into FAILED must carry the reason, retries must not, and a terminal delivery
// stays immutable, reason included.
func TestFailureReasonFollowsTheDeliveryLifecycle(t *testing.T) {
	f := newDeliveryFixture(t)
	const clear = "lease_expires_at = NULL, claim_token = NULL, claimed_by = NULL"

	noReason := f.inFlightAt(t, f.orgID, f.dest1, f.ep1, 1)
	if err := f.finish(f.orgID, noReason, f.claimToken(t, noReason), 1, "HTTP_ERROR", 404,
		"status = 'FAILED', "+clear+", last_http_status = 404, completed_at = now()"); err == nil {
		t.Fatal("IN_FLIGHT -> FAILED without a reason was accepted")
	}
	if f.status(t, noReason) != "IN_FLIGHT" {
		t.Fatal("the rejected transition must leave the delivery IN_FLIGHT")
	}

	retry := f.inFlightAt(t, f.orgID, f.dest1, f.ep1, 1)
	if err := f.finish(f.orgID, retry, f.claimToken(t, retry), 1, "HTTP_ERROR", 503,
		"status = 'PENDING', "+clear+", next_attempt_at = now() + interval '1 minute', failure_reason = 'RETRIES_EXHAUSTED'"); err == nil {
		t.Fatal("a retry (PENDING) with a failure reason was accepted")
	}

	id := f.inFlightAt(t, f.orgID, f.dest1, f.ep1, 1)
	if err := f.finish(f.orgID, id, f.claimToken(t, id), 1, "HTTP_ERROR", 404, toFailed); err != nil {
		t.Fatalf("a FAILED transition with a reason must be accepted: %v", err)
	}
	if f.env.Count(t, "SELECT count(*) FROM deliveries WHERE id = $1 AND status = 'FAILED' AND failure_reason = 'DESTINATION_REJECTED'", id) != 1 {
		t.Fatal("the reason was not stored")
	}
	for name, sql := range map[string]string{
		"rewrite the reason":        "UPDATE deliveries SET failure_reason = 'INTERNAL_ERROR' WHERE id = $1",
		"clear the reason":          "UPDATE deliveries SET failure_reason = NULL WHERE id = $1",
		"redrive (back to PENDING)": "UPDATE deliveries SET status = 'PENDING', failure_reason = NULL, completed_at = NULL WHERE id = $1",
		"touch another column":      "UPDATE deliveries SET last_http_status = 200 WHERE id = $1",
	} {
		f.reject(t, name, sql, id)
	}
}

// Migration 00019: legacy FAILED rows stay NULL and immutable, new FAILED rows need a reason,
// the migration is idempotent after an interrupted run, and it rolls back and re-applies.
func TestMigration19KeepsLegacyFailedRowsRollsBackAndReapplies(t *testing.T) {
	f := newDeliveryFixture(t)
	ctx := context.Background()
	db, err := f.env.Pool.SQLDB()
	if err != nil {
		t.Fatal(err)
	}
	provider := newGooseProvider(t, db)
	count := func(sql string, args ...any) int { return f.env.Count(t, sql, args...) }
	columns := "SELECT count(*) FROM information_schema.columns WHERE (table_name = 'deliveries' AND column_name = 'failure_reason') OR (table_name = 'delivery_attempts' AND column_name = 'terminal_reason')"
	constraints := "SELECT count(*) FROM pg_constraint WHERE conname IN ('deliveries_failure_reason_valid', 'deliveries_failure_reason_only_when_failed', 'deliveries_failed_has_reason', 'delivery_attempts_terminal_reason_valid', 'delivery_attempts_terminal_reason_not_success')"

	if _, err := provider.DownTo(ctx, 18); err != nil {
		t.Fatalf("rollback to 18: %v", err)
	}
	if count(columns) != 0 || count(constraints) != 0 {
		t.Fatal("the rollback must remove both columns and every constraint")
	}

	// A delivery that failed before the reasons existed, with its attempt history.
	q, _ := f.env.Pool.Querier()
	var legacy, legacyAttempt string
	if err := q.QueryRow(ctx, "INSERT INTO deliveries (organization_id, event_id, destination_id) VALUES ($1, $2, $3) RETURNING id::text",
		f.orgID, f.newEvent(t, f.orgID, f.ep1), f.dest1).Scan(&legacy); err != nil {
		t.Fatal(err)
	}
	f.accept(t, "claim", claimSQL, legacy)
	f.accept(t, "attempt", attemptInsert, f.orgID, legacy, 1, "HTTP_ERROR", 404, 5)
	f.accept(t, "fail", "UPDATE deliveries SET status = 'FAILED', lease_expires_at = NULL, claim_token = NULL, claimed_by = NULL, last_http_status = 404, completed_at = now() WHERE id = $1", legacy)
	if err := q.QueryRow(ctx, "SELECT id::text FROM delivery_attempts WHERE delivery_id = $1", legacy).Scan(&legacyAttempt); err != nil {
		t.Fatal(err)
	}
	var before string
	snapshot := `SELECT (SELECT string_agg(d::text, '|' ORDER BY id) FROM deliveries d) || '#' || coalesce((SELECT string_agg(a::text, '|' ORDER BY id) FROM delivery_attempts a), '')`
	if err := q.QueryRow(ctx, snapshot).Scan(&before); err != nil {
		t.Fatal(err)
	}

	// An interrupted earlier run: the column exists and a constraint of the same name has another meaning.
	f.env.Exec(t, "ALTER TABLE deliveries ADD COLUMN failure_reason text")
	f.env.Exec(t, "ALTER TABLE deliveries ADD CONSTRAINT deliveries_failure_reason_valid CHECK (failure_reason IS NULL OR failure_reason <> 'x')")

	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("up must repair an interrupted run: %v", err)
	}
	if count(columns) != 2 || count(constraints) != 5 {
		t.Fatalf("after up: %d columns, %d constraints", count(columns), count(constraints))
	}
	if count("SELECT count(*) FROM deliveries WHERE failure_reason IS NULL AND id = $1 AND status = 'FAILED'", legacy) != 1 ||
		count("SELECT count(*) FROM delivery_attempts WHERE terminal_reason IS NULL AND id = $1", legacyAttempt) != 1 {
		t.Fatal("legacy rows must keep NULL reasons: nothing is backfilled")
	}
	if count("SELECT count(*) FROM pg_constraint WHERE conname = 'deliveries_failure_reason_valid' AND pg_get_constraintdef(oid) LIKE '%RETRIES_EXHAUSTED%'") != 1 {
		t.Fatal("the interrupted run's constraint must be replaced by the real one")
	}

	// The legacy row is still FAILED, still unreadable for change, and the read model omits the reason.
	f.reject(t, "changing a legacy FAILED row", "UPDATE deliveries SET failure_reason = 'INTERNAL_ERROR' WHERE id = $1", legacy)
	f.reject(t, "redriving a legacy FAILED row", "UPDATE deliveries SET status = 'PENDING', completed_at = NULL WHERE id = $1", legacy)
	rec, err := delivery.DeliveryRecordByID(ctx, q, f.orgID, legacy)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != "FAILED" || rec.FailureReason != nil {
		t.Fatalf("legacy record: %+v", rec)
	}
	raw, _ := json.Marshal(rec)
	if strings.Contains(string(raw), "failureReason") {
		t.Fatalf("a legacy record must omit failureReason: %s", raw)
	}
	attempts, err := delivery.AttemptsOf(ctx, q, f.orgID, legacy)
	if err != nil || len(attempts) != 1 || attempts[0].TerminalReason != nil {
		t.Fatalf("legacy attempts: %+v %v", attempts, err)
	}
	if raw, _ := json.Marshal(attempts[0]); strings.Contains(string(raw), "terminalReason") {
		t.Fatalf("a legacy attempt must omit terminalReason: %s", raw)
	}

	// New failures need a reason, and a new failure keeps it.
	f.rejectDelivery(t, "a new FAILED delivery without a reason", dv{org: f.orgID, event: f.newEvent(t, f.orgID, f.ep1), dest: f.dest1, status: "FAILED", attempts: 1, httpStatus: 500, completed: "now()", legacy: true}, "deliveries_failed_has_reason")
	fresh := f.mustDelivery(t, dv{org: f.orgID, event: f.newEvent(t, f.orgID, f.ep1), dest: f.dest1, status: "FAILED", attempts: 1, httpStatus: 500, completed: "now()", reason: "'EGRESS_BLOCKED'"})

	// Rolling back drops only the new data, keeping every delivery and attempt.
	if _, err := provider.DownTo(ctx, 18); err != nil {
		t.Fatalf("second rollback: %v", err)
	}
	if count(columns) != 0 || count(constraints) != 0 || count("SELECT count(*) FROM deliveries") != 2 || count("SELECT count(*) FROM delivery_attempts") != 1 {
		t.Fatal("the rollback removes only the reason columns and constraints")
	}
	if count("SELECT count(*) FROM deliveries WHERE id = $1 AND status = 'FAILED'", fresh) != 1 {
		t.Fatal("a rolled-back FAILED delivery must stay FAILED")
	}
	var after string
	if err := q.QueryRow(ctx, snapshot).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(after, strings.SplitN(before, "#", 2)[1]) {
		t.Fatalf("the attempt history changed across the rollback:\n%s\n%s", before, after)
	}

	// Re-apply: the previously failed rows get NULL reasons (the data was dropped, nothing is invented)
	// and a second up is a no-op.
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("re-apply: %v", err)
	}
	if count(columns) != 2 || count(constraints) != 5 {
		t.Fatal("re-applying must recreate the columns and the constraints")
	}
	if count("SELECT count(*) FROM deliveries WHERE status = 'FAILED' AND failure_reason IS NULL") != 2 {
		t.Fatal("after a rollback and re-apply, failed deliveries are legacy rows with NULL reasons")
	}
	if res, err := provider.Up(ctx); err != nil || len(res) != 0 {
		t.Fatalf("a second up must be a no-op: %d %v", len(res), err)
	}
}
