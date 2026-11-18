package database_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Thiago-tods/relayforge/internal/app/apptest"
	"github.com/Thiago-tods/relayforge/internal/egress"
)

// The attempt-recording protocol these tests pin down (see migration 00014):
//
//  1. Claim:    UPDATE deliveries SET status = 'IN_FLIGHT', attempt_count = attempt_count + 1, ...  (commits)
//  2. Execute the HTTP request outside any transaction.
//  3. One transaction: INSERT the attempt, THEN UPDATE the delivery
//     WHERE id = $1 AND status = 'IN_FLIGHT' AND claim_token = $2.
//     If the UPDATE affects no row (stale claim), roll back the whole transaction.
//
// The attempt insert locks the delivery row (FOR NO KEY UPDATE) before anything
// else, so every writer takes the delivery row first and the attempt second.

const (
	claimSQL = `UPDATE deliveries SET status = 'IN_FLIGHT', attempt_count = attempt_count + 1,
		lease_expires_at = now() + interval '1 minute', claim_token = gen_random_uuid(), claimed_by = 'worker' WHERE id = $1`
	retrySQL = `UPDATE deliveries SET status = 'PENDING', lease_expires_at = NULL, claim_token = NULL, claimed_by = NULL,
		next_attempt_at = now() + interval '1 minute' WHERE id = $1`
)

var errStaleClaim = errors.New("stale claim token")

// inFlightAt returns an IN_FLIGHT delivery whose attempt_count is n, with a real
// history: attempts 1..n-1 were recorded as abandoned and each was followed by a retry and a new claim.
func (f *deliveryFixture) inFlightAt(t *testing.T, org, dest, endpoint string, n int) string {
	t.Helper()
	d := dv{org: org, event: f.newEvent(t, org, endpoint), dest: dest, max: 50}
	id := f.mustDelivery(t, d)
	f.accept(t, "first claim", claimSQL, id)
	for attempt := 1; attempt < n; attempt++ {
		f.accept(t, "abandoned attempt", attemptInsert, org, id, attempt, "ABANDONED", nil, 1)
		f.retryAndClaim(t, id)
	}
	return id
}

// retryAndClaim moves an IN_FLIGHT delivery back to PENDING and claims it again.
func (f *deliveryFixture) retryAndClaim(t *testing.T, id string) {
	t.Helper()
	f.accept(t, "retry", retrySQL, id)
	f.accept(t, "claim", claimSQL, id)
}

func (f *deliveryFixture) claimToken(t *testing.T, id string) string {
	t.Helper()
	q, _ := f.env.Pool.Querier()
	var token string
	if err := q.QueryRow(context.Background(), "SELECT claim_token::text FROM deliveries WHERE id = $1", id).Scan(&token); err != nil {
		t.Fatal(err)
	}
	return token
}

func (f *deliveryFixture) status(t *testing.T, id string) string {
	t.Helper()
	q, _ := f.env.Pool.Querier()
	var s string
	if err := q.QueryRow(context.Background(), "SELECT status FROM deliveries WHERE id = $1", id).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// finish performs step 3 of the protocol: record the attempt, then move the
// delivery with the claim token, in one transaction. A stale token rolls everything back.
func (f *deliveryFixture) finish(org, id, token string, number int, outcome string, httpStatus any, assign string) error {
	return f.env.Pool.InTx(context.Background(), func(tx pgx.Tx) error {
		ctx := context.Background()
		if _, err := tx.Exec(ctx, attemptInsert, org, id, number, outcome, httpStatus, 5); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, "UPDATE deliveries SET "+assign+" WHERE id = $1 AND status = 'IN_FLIGHT' AND claim_token = $2", id, token)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return errStaleClaim
		}
		return nil
	})
}

const (
	toSucceeded = "status = 'SUCCEEDED', lease_expires_at = NULL, claim_token = NULL, claimed_by = NULL, last_http_status = 204, completed_at = now()"
	toFailed    = "status = 'FAILED', lease_expires_at = NULL, claim_token = NULL, claimed_by = NULL, last_http_status = 404, completed_at = now(), failure_reason = 'DESTINATION_REJECTED'"
	toPending   = "status = 'PENDING', lease_expires_at = NULL, claim_token = NULL, claimed_by = NULL, next_attempt_at = now() + interval '1 minute'"
)

func pgCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

func (f *deliveryFixture) attemptRows(t *testing.T, id string) int {
	return f.env.Count(t, "SELECT count(*) FROM delivery_attempts WHERE delivery_id = $1", id)
}

// ---- 1. Attempt consistency ------------------------------------------------

func TestAttemptIsAcceptedWhileInFlightWithTheCurrentNumber(t *testing.T) {
	f := newDeliveryFixture(t)
	id := f.inFlightAt(t, f.orgID, f.dest1, f.ep1, 1)
	if err := f.finish(f.orgID, id, f.claimToken(t, id), 1, "SUCCESS", 204, toSucceeded); err != nil {
		t.Fatalf("the intended flow must work: %v", err)
	}
	if f.status(t, id) != "SUCCEEDED" || f.attemptRows(t, id) != 1 {
		t.Fatal("attempt and completion must both be committed")
	}
}

func TestRetryFlowRecordsEveryAttemptInOrder(t *testing.T) {
	f := newDeliveryFixture(t)
	id := f.inFlightAt(t, f.orgID, f.dest1, f.ep1, 1)
	for n := 1; n <= 3; n++ {
		if err := f.finish(f.orgID, id, f.claimToken(t, id), n, "HTTP_ERROR", 503, toPending); err != nil {
			t.Fatalf("attempt %d: %v", n, err)
		}
		f.accept(t, "claim", claimSQL, id)
	}
	if err := f.finish(f.orgID, id, f.claimToken(t, id), 4, "SUCCESS", 200, toSucceeded); err != nil {
		t.Fatal(err)
	}
	if f.attemptRows(t, id) != 4 || f.status(t, id) != "SUCCEEDED" {
		t.Fatal("expected four ordered attempts and a succeeded delivery")
	}
}

func TestAttemptsAreRejectedUnlessTheDeliveryIsInFlight(t *testing.T) {
	f := newDeliveryFixture(t)
	var none any

	reach := map[string]func() (string, int){
		"PENDING": func() (string, int) {
			id := f.inFlightAt(t, f.orgID, f.dest1, f.ep1, 1)
			f.accept(t, "attempt 1", attemptInsert, f.orgID, id, 1, "HTTP_ERROR", 503, 1)
			f.accept(t, "retry", retrySQL, id)
			return id, 1
		},
		"SUCCEEDED": func() (string, int) {
			id := f.inFlightAt(t, f.orgID, f.dest1, f.ep1, 1)
			if err := f.finish(f.orgID, id, f.claimToken(t, id), 1, "SUCCESS", 204, toSucceeded); err != nil {
				t.Fatal(err)
			}
			return id, 1
		},
		"FAILED": func() (string, int) {
			id := f.inFlightAt(t, f.orgID, f.dest1, f.ep1, 1)
			if err := f.finish(f.orgID, id, f.claimToken(t, id), 1, "HTTP_ERROR", 404, toFailed); err != nil {
				t.Fatal(err)
			}
			return id, 1
		},
		"CANCELLED": func() (string, int) {
			id := f.mustDelivery(t, f.pending(t))
			f.accept(t, "cancel", "UPDATE deliveries SET status = 'CANCELLED', completed_at = now() WHERE id = $1", id)
			return id, 0
		},
	}
	for status, setup := range reach {
		id, count := setup()
		if got := f.status(t, id); got != status {
			t.Fatalf("setup produced %s, want %s", got, status)
		}
		before := f.attemptRows(t, id)
		// Every number: the last attempt, the next one and a fresh one.
		for _, n := range []int{count, count + 1, count + 2, 1} {
			if n < 1 {
				continue
			}
			_, err := f.insertAttempt(id, f.orgID, n, "NETWORK_ERROR", none)
			if err == nil {
				t.Errorf("%s delivery accepted attempt %d", status, n)
			} else if !strings.Contains(err.Error(), "IN_FLIGHT") && n != 1 {
				t.Errorf("%s delivery, attempt %d: rejected for the wrong reason: %v", status, n, err)
			}
		}
		if f.attemptRows(t, id) != before {
			t.Errorf("%s delivery gained attempt rows", status)
		}
	}
}

func (f *deliveryFixture) insertAttempt(id, org string, n int, outcome string, httpStatus any) (int64, error) {
	q, _ := f.env.Pool.Querier()
	tag, err := q.Exec(context.Background(), attemptInsert, org, id, n, outcome, httpStatus, 1)
	return tag.RowsAffected(), err
}

func TestAttemptNumberMustEqualTheCurrentAttemptCount(t *testing.T) {
	f := newDeliveryFixture(t)
	var none any
	id := f.inFlightAt(t, f.orgID, f.dest1, f.ep1, 3) // attempts 1 and 2 are recorded, 3 is running

	for _, n := range []int{1, 2} {
		_, err := f.insertAttempt(id, f.orgID, n, "NETWORK_ERROR", none)
		if err == nil || !strings.Contains(err.Error(), "must equal") {
			t.Errorf("lower number %d (backfill) must be rejected by the equality rule: %v", n, err)
		}
	}
	for _, n := range []int{4, 5, 100} {
		_, err := f.insertAttempt(id, f.orgID, n, "NETWORK_ERROR", none)
		if err == nil || !strings.Contains(err.Error(), "must equal") {
			t.Errorf("higher number %d must be rejected by the equality rule: %v", n, err)
		}
	}
	if f.attemptRows(t, id) != 2 {
		t.Fatal("rejected attempts must not be stored")
	}
	if _, err := f.insertAttempt(id, f.orgID, 3, "NETWORK_ERROR", none); err != nil {
		t.Fatalf("the current number must be accepted: %v", err)
	}
}

func TestAttemptsCannotLeaveGapsOrBackfillAfterALaterClaim(t *testing.T) {
	f := newDeliveryFixture(t)
	var none any
	// Attempt 1 is never recorded (a bug or a lost write), yet the delivery is claimed again.
	id := f.mustDelivery(t, dv{org: f.orgID, event: f.newEvent(t, f.orgID, f.ep1), dest: f.dest1, max: 50})
	f.accept(t, "claim 1", claimSQL, id)
	f.retryAndClaim(t, id) // now attempt_count = 2 and attempt 1 has no row

	_, err := f.insertAttempt(id, f.orgID, 1, "ABANDONED", none)
	if err == nil || !strings.Contains(err.Error(), "must equal") {
		t.Errorf("backfilling attempt 1 after claim 2 must be rejected: %v", err)
	}
	_, err = f.insertAttempt(id, f.orgID, 2, "NETWORK_ERROR", none)
	if err == nil || !strings.Contains(err.Error(), "gap") {
		t.Errorf("attempt 2 without attempt 1 must be rejected as a gap: %v", err)
	}
	if f.attemptRows(t, id) != 0 {
		t.Fatal("no attempt may be stored for a delivery with a gap")
	}
}

func TestStaleClaimTokenRollsBackTheAttemptToo(t *testing.T) {
	f := newDeliveryFixture(t)
	id := f.inFlightAt(t, f.orgID, f.dest1, f.ep1, 1)
	token := f.claimToken(t, id)

	var stale string
	q, _ := f.env.Pool.Querier()
	if err := q.QueryRow(context.Background(), "SELECT gen_random_uuid()::text").Scan(&stale); err != nil {
		t.Fatal(err)
	}
	err := f.finish(f.orgID, id, stale, 1, "SUCCESS", 204, toSucceeded)
	if !errors.Is(err, errStaleClaim) {
		t.Fatalf("a stale token must fail fencing, got %v", err)
	}
	if f.attemptRows(t, id) != 0 {
		t.Fatal("the attempt must be rolled back together with the failed fencing")
	}
	if f.status(t, id) != "IN_FLIGHT" || f.claimToken(t, id) != token {
		t.Fatal("the delivery must be untouched by the stale worker")
	}
	// The legitimate holder can still finish.
	if err := f.finish(f.orgID, id, token, 1, "SUCCESS", 204, toSucceeded); err != nil {
		t.Fatalf("the current token must still work: %v", err)
	}
}

func TestRecordingAfterLeavingInFlightInTheSameTransactionIsRejected(t *testing.T) {
	f := newDeliveryFixture(t)
	id := f.inFlightAt(t, f.orgID, f.dest1, f.ep1, 1)
	token := f.claimToken(t, id)
	// Wrong order: completing first and recording second. The attempt must be recorded first.
	err := f.env.Pool.InTx(context.Background(), func(tx pgx.Tx) error {
		ctx := context.Background()
		if _, err := tx.Exec(ctx, "UPDATE deliveries SET "+toSucceeded+" WHERE id = $1 AND claim_token = $2", id, token); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, attemptInsert, f.orgID, id, 1, "SUCCESS", 204, 5)
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "IN_FLIGHT") {
		t.Fatalf("recording after completion must be rejected: %v", err)
	}
	if f.status(t, id) != "IN_FLIGHT" || f.attemptRows(t, id) != 0 {
		t.Fatal("the failed transaction must leave no trace")
	}
}

// TestStaleWorkerAndReaperNeverBothRecordTheSameAttempt races a worker that
// finished late (stale) against the reaper that gave up on its lease. Exactly
// one must win, whichever commits first, and PostgreSQL must never report a deadlock.
func TestStaleWorkerAndReaperNeverBothRecordTheSameAttempt(t *testing.T) {
	f := newDeliveryFixture(t)
	for i := 0; i < 40; i++ {
		id := f.inFlightAt(t, f.orgID, f.dest1, f.ep1, 1)
		token := f.claimToken(t, id)

		var wg sync.WaitGroup
		start := make(chan struct{})
		errs := make([]error, 2)
		wg.Add(2)
		go func() { // the worker reports a success
			defer wg.Done()
			<-start
			errs[0] = f.finish(f.orgID, id, token, 1, "SUCCESS", 204, toSucceeded)
		}()
		go func() { // the reaper records the attempt as abandoned and schedules a retry
			defer wg.Done()
			<-start
			errs[1] = f.finish(f.orgID, id, token, 1, "ABANDONED", nil, toPending)
		}()
		close(start)
		wg.Wait()

		wins := 0
		for _, err := range errs {
			if err == nil {
				wins++
				continue
			}
			if code := pgCode(err); code == "40P01" || code == "40001" {
				t.Fatalf("iteration %d: unexpected %s (deadlock or serialization failure): %v", i, code, err)
			}
		}
		if wins != 1 {
			t.Fatalf("iteration %d: %d transactions committed, want exactly 1: %v", i, wins, errs)
		}
		if f.attemptRows(t, id) != 1 {
			t.Fatalf("iteration %d: %d attempt rows, want exactly 1", i, f.attemptRows(t, id))
		}
		q, _ := f.env.Pool.Querier()
		var outcome string
		if err := q.QueryRow(context.Background(), "SELECT outcome FROM delivery_attempts WHERE delivery_id = $1", id).Scan(&outcome); err != nil {
			t.Fatal(err)
		}
		want := map[string]string{"SUCCESS": "SUCCEEDED", "ABANDONED": "PENDING"}[outcome]
		if got := f.status(t, id); got != want {
			t.Fatalf("iteration %d: attempt %s but the delivery is %s, want %s", i, outcome, got, want)
		}
	}
}

func TestParallelDeliveriesNeverDeadlock(t *testing.T) {
	f := newDeliveryFixture(t)
	const deliveries = 12
	ids := make([]string, deliveries)
	for i := range ids {
		ids[i] = f.inFlightAt(t, f.orgID, f.dest1, f.ep1, 1)
	}
	var wg sync.WaitGroup
	errs := make(chan error, deliveries*2)
	for _, id := range ids {
		token := f.claimToken(t, id)
		for range 2 { // two stale/legitimate workers per delivery, all at once
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := f.finish(f.orgID, id, token, 1, "SUCCESS", 204, toSucceeded); err != nil && pgCode(err) == "40P01" {
					errs <- err
				}
			}()
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("deadlock: %v", err)
	}
	for _, id := range ids {
		if f.attemptRows(t, id) != 1 || f.status(t, id) != "SUCCEEDED" {
			t.Errorf("delivery %s: %d attempts, status %s", id, f.attemptRows(t, id), f.status(t, id))
		}
	}
}

// ---- 2. Append-only, including TRUNCATE -----------------------------------

func TestTruncateIsRejectedAndHistoryRemains(t *testing.T) {
	f := newDeliveryFixture(t)
	id := f.inFlightAt(t, f.orgID, f.dest1, f.ep1, 1)
	if err := f.finish(f.orgID, id, f.claimToken(t, id), 1, "SUCCESS", 204, toSucceeded); err != nil {
		t.Fatal(err)
	}
	deliveries := f.env.Count(t, "SELECT count(*) FROM deliveries")
	destinations := f.env.Count(t, "SELECT count(*) FROM destinations")

	for _, stmt := range []string{
		"TRUNCATE delivery_attempts",
		"TRUNCATE delivery_attempts RESTART IDENTITY",
		"TRUNCATE deliveries CASCADE",
		"TRUNCATE destinations CASCADE",
		"TRUNCATE webhook_events CASCADE", // cascades into deliveries and attempts
		"TRUNCATE ingestion_endpoints CASCADE",
		"TRUNCATE organizations CASCADE",
	} {
		f.reject(t, stmt, stmt)
	}
	if f.attemptRows(t, id) != 1 || f.env.Count(t, "SELECT count(*) FROM deliveries") != deliveries ||
		f.env.Count(t, "SELECT count(*) FROM destinations") != destinations {
		t.Fatal("a rejected TRUNCATE must leave the history intact")
	}
}

// TestGuaranteesHoldForARuntimeRoleWithFullTablePrivileges shows that the
// protection does not depend on withholding privileges: a role that may
// INSERT, UPDATE, DELETE and TRUNCATE is still stopped by the triggers, and it
// cannot switch the triggers off because it is not the table owner or a superuser.
func TestGuaranteesHoldForARuntimeRoleWithFullTablePrivileges(t *testing.T) {
	f := newDeliveryFixture(t)
	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatal(err)
	}
	role := "rf_app_" + hex.EncodeToString(suffix)
	ctx := context.Background()
	q, _ := f.env.Pool.Querier()
	if _, err := q.Exec(ctx, "CREATE ROLE "+role+" NOLOGIN"); err != nil {
		t.Fatalf("this test needs a test server role that may create roles: %v", err)
	}
	t.Cleanup(func() {
		q, _ := f.env.Pool.Querier()
		_, _ = q.Exec(context.Background(), "DROP OWNED BY "+role)
		_, _ = q.Exec(context.Background(), "DROP ROLE IF EXISTS "+role)
	})
	if _, err := q.Exec(ctx, "GRANT ALL ON ALL TABLES IN SCHEMA public TO "+role); err != nil {
		t.Fatal(err)
	}

	id := f.inFlightAt(t, f.orgID, f.dest1, f.ep1, 1)
	if err := f.finish(f.orgID, id, f.claimToken(t, id), 1, "SUCCESS", 204, toSucceeded); err != nil {
		t.Fatal(err)
	}
	for _, priv := range []string{"UPDATE", "DELETE", "TRUNCATE"} {
		if f.env.Count(t, fmt.Sprintf("SELECT CASE WHEN has_table_privilege('%s', 'delivery_attempts', '%s') THEN 1 ELSE 0 END", role, priv)) != 1 {
			t.Fatalf("the role must hold %s, otherwise this test proves nothing", priv)
		}
	}

	as := func(stmt string) error {
		return f.env.Pool.InTx(ctx, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, "SET LOCAL ROLE "+role); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, stmt)
			return err
		})
	}
	// Stopped by the triggers (SQLSTATE class 23), not by missing privileges (42501).
	for _, stmt := range []string{
		"UPDATE delivery_attempts SET outcome = 'TIMEOUT', http_status = NULL",
		"DELETE FROM delivery_attempts",
		"TRUNCATE delivery_attempts",
		"DELETE FROM deliveries",
		"TRUNCATE deliveries CASCADE",
		"DELETE FROM destinations",
		"TRUNCATE destinations CASCADE",
		"UPDATE deliveries SET status = 'PENDING', completed_at = NULL",
	} {
		err := as(stmt)
		if err == nil {
			t.Errorf("%q succeeded for a role with full privileges", stmt)
		} else if code := pgCode(err); code != "23000" {
			t.Errorf("%q: SQLSTATE %s, want 23000 from a trigger: %v", stmt, code, err)
		}
	}
	// Switching the guarantees off needs ownership or superuser rights the role lacks.
	for _, stmt := range []string{
		"ALTER TABLE delivery_attempts DISABLE TRIGGER ALL",
		"ALTER TABLE deliveries DISABLE TRIGGER USER",
		"DROP TRIGGER delivery_attempts_append_only ON delivery_attempts",
		"SET session_replication_role = replica",
		"DROP TABLE delivery_attempts",
	} {
		if err := as(stmt); pgCode(err) != "42501" {
			t.Errorf("%q: SQLSTATE %q, want 42501 (insufficient privilege): %v", stmt, pgCode(err), err)
		}
	}
	if f.attemptRows(t, id) != 1 || f.status(t, id) != "SUCCEEDED" {
		t.Fatal("history changed")
	}
}

// ---- 3. Non-deletion invariants -------------------------------------------

func TestDestinationsAndDeliveriesAreNeverDeletedEvenWhenUnreferenced(t *testing.T) {
	f := newDeliveryFixture(t)
	unreferencedDest := f.insertDestination(t, f.orgID, f.userID, "Unused", validURL)
	unreferencedDelivery := f.mustDelivery(t, f.pending(t)) // has no attempts
	referenced := f.inFlightAt(t, f.orgID, f.dest1, f.ep1, 1)
	if err := f.finish(f.orgID, referenced, f.claimToken(t, referenced), 1, "SUCCESS", 204, toSucceeded); err != nil {
		t.Fatal(err)
	}

	for name, stmt := range map[string][]any{
		"unreferenced destination": {"DELETE FROM destinations WHERE id = $1", unreferencedDest},
		"referenced destination":   {"DELETE FROM destinations WHERE id = $1", f.dest1},
		"unreferenced delivery":    {"DELETE FROM deliveries WHERE id = $1", unreferencedDelivery},
		"delivery with attempts":   {"DELETE FROM deliveries WHERE id = $1", referenced},
		"all destinations":         {"DELETE FROM destinations"},
		"all deliveries":           {"DELETE FROM deliveries"},
	} {
		sql := stmt[0].(string)
		_, err := func() (any, error) {
			q, _ := f.env.Pool.Querier()
			_, err := q.Exec(context.Background(), sql, stmt[1:]...)
			return nil, err
		}()
		if err == nil {
			t.Errorf("%s: DELETE succeeded", name)
		} else if pgCode(err) != "23000" || !strings.Contains(err.Error(), "never deleted") {
			t.Errorf("%s: rejected by something other than the removal trigger: %v", name, err)
		}
	}
	if f.env.Count(t, "SELECT count(*) FROM destinations WHERE id = $1", unreferencedDest) != 1 ||
		f.env.Count(t, "SELECT count(*) FROM deliveries WHERE id = $1", unreferencedDelivery) != 1 {
		t.Fatal("a rejected DELETE removed a row")
	}
}

func TestRevocationAndNormalTransitionsStillWork(t *testing.T) {
	f := newDeliveryFixture(t)
	f.accept(t, "rename", "UPDATE destinations SET name = 'Renamed' WHERE id = $1", f.dest1)
	f.accept(t, "rotate the secret", "UPDATE destinations SET signing_secret_ciphertext = decode(repeat('cd', 40), 'hex'), secret_version = secret_version + 1 WHERE id = $1", f.dest1)
	f.accept(t, "pause", "UPDATE destinations SET status = 'PAUSED' WHERE id = $1", f.dest1)
	f.accept(t, "activate", "UPDATE destinations SET status = 'ACTIVE' WHERE id = $1", f.dest1)
	f.accept(t, "revoke", "UPDATE destinations SET status = 'REVOKED', revoked_at = now() WHERE id = $1", f.dest1)

	if f.env.Count(t, "SELECT count(*) FROM destinations WHERE id = $1 AND status = 'REVOKED' AND revoked_at IS NOT NULL", f.dest1) != 1 {
		t.Fatal("the destination must be revoked, not removed")
	}
	// A revoked destination keeps working as a historical reference.
	id := f.inFlightAt(t, f.orgID, f.dest1, f.ep1, 1)
	if err := f.finish(f.orgID, id, f.claimToken(t, id), 1, "SUCCESS", 204, toSucceeded); err != nil {
		t.Fatalf("history for a revoked destination must still be recordable: %v", err)
	}
}

// ---- 4. URL constraint ------------------------------------------------------

func TestDestinationURLConstraintRejectsMalformedHostsAndPorts(t *testing.T) {
	f := newDeliveryFixture(t)
	ins := f.destinationSQL(cipher)
	args := func(name, url string) []any { return []any{f.orgID, name, url, "ACTIVE", 1, 1, f.userID, nil} }
	label := func(n int) string { return strings.Repeat("a", n) }

	valid := []string{
		"https://a.io", "https://example.com", "https://example.com/hook?token=t", "https://a-b.example.com:65535/x",
		"https://example.com:1", "https://example.com:8443?x=1", "https://xn--bcher-kva.example/x",
		"https://a.b.c.d.example.org/", "https://" + label(63) + ".com", "https://1abc.example.com", "https://a--b.example.com",
		"https://" + label(63) + "." + label(63) + "." + label(63) + "." + label(61), // exactly 253 characters of host
	}
	for i, u := range valid {
		f.accept(t, "valid "+u[:min(len(u), 40)], ins, args(fmt.Sprintf("ok-%d", i), u)...)
	}

	invalid := map[string]string{
		"port 0":                  "https://example.com:0/x",
		"port 65536":              "https://example.com:65536/x",
		"port 99999":              "https://example.com:99999",
		"six digit port":          "https://example.com:100000/x",
		"empty port":              "https://example.com:/x",
		"empty port without path": "https://example.com:",
		"leading zero port":       "https://example.com:0443/x",
		"non-numeric port":        "https://example.com:abc/x",
		"leading hyphen label":    "https://-a.example.com/",
		"trailing hyphen label":   "https://a-.example.com/",
		"hyphen only label":       "https://-.example.com/",
		"empty label":             "https://a..example.com/",
		"leading dot":             "https://.example.com/",
		"trailing dot":            "https://example.com./x",
		"trailing dot with port":  "https://example.com.:443/x",
		"label of 64":             "https://" + label(64) + ".com/",
		"host of 254":             "https://" + label(63) + "." + label(63) + "." + label(63) + "." + label(62) + "/",
		"single label host":       "https://intranet/",
		"numeric last label":      "https://example.123/",
		"ipv4 literal":            "https://127.0.0.1/",
		"public ipv4 literal":     "https://8.8.8.8/",
		"hex last label":          "https://0x7f.0x1/",
		"underscore in host":      "https://exa_mple.com/",
		"uppercase host":          "https://Example.com/",
		"empty host":              "https:///x",
		"empty host with port":    "https://:443/x",
	}
	for name, u := range invalid {
		f.reject(t, name, ins, args("bad-"+name, u)...)
	}
}

// TestDatabaseNeverRejectsAURLTheGoValidatorNormalizes keeps the database
// constraint no stricter than the authoritative Go validator for the URLs it
// stores: whatever ValidateDestinationURL produces must be insertable.
func TestDatabaseNeverRejectsAURLTheGoValidatorNormalizes(t *testing.T) {
	f := newDeliveryFixture(t)
	policy := egress.DefaultURLPolicy()
	policy.AllowedPorts = []int{443, 8443, 65535, 1}
	inputs := []string{
		"https://example.com", "https://Example.COM:443/x?token=s3cret&b=2", "https://example.com./x", "https://sub.a.example.co.uk/a%20b",
		"https://xn--bcher-kva.example/x", "https://example.com:8443/x", "https://example.com:65535", "https://example.com:1/?q",
		"https://example.com?x=1", "https://a.io/", "https://a-b-c.d-e.example.net/p/a/t/h",
		"https://" + strings.Repeat("a", 63) + ".example.org/x", "https://1abc.example.com/x", "https://a--b.example.com",
	}
	ins := f.destinationSQL(cipher)
	for i, raw := range inputs {
		v, err := egress.ValidateDestinationURL(raw, policy)
		if err != nil {
			t.Fatalf("corpus entry %q is not valid for the Go validator: %v", raw, err)
		}
		f.accept(t, "Go-normalized form of "+raw[:min(len(raw), 30)], ins, f.orgID, fmt.Sprintf("go-%d", i), v.Normalized, "ACTIVE", 1, 1, f.userID, nil)
	}
}

// ---- 5. Migration 00014 rollback -------------------------------------------

func TestMigration14RollsBackToTheOriginalBehaviorAndReapplies(t *testing.T) {
	env := apptest.New(t)
	ctx := context.Background()
	db, err := env.Pool.SQLDB()
	if err != nil {
		t.Fatal(err)
	}
	provider := newGooseProvider(t, db)
	exec := func(sql string, args ...any) error {
		q, _ := env.Pool.Querier()
		_, err := q.Exec(ctx, sql, args...)
		return err
	}
	count := func(sql string, args ...any) int { return env.Count(t, sql, args...) }

	if _, err := provider.DownTo(ctx, 13); err != nil {
		t.Fatalf("rolling back 00014: %v", err)
	}
	for _, trg := range []string{"destinations_never_deleted", "deliveries_never_deleted", "destinations_never_truncated", "deliveries_never_truncated", "delivery_attempts_never_truncated"} {
		if count("SELECT count(*) FROM pg_trigger WHERE tgname = $1", trg) != 0 {
			t.Errorf("trigger %s must be removed by the rollback", trg)
		}
	}
	if count("SELECT count(*) FROM pg_proc WHERE proname = 'relayforge_reject_row_removal'") != 0 {
		t.Error("the removal-guard function must be dropped by the rollback")
	}
	// 00013 is intact: its append-only trigger and attempt check still exist.
	if count("SELECT count(*) FROM pg_trigger WHERE tgname IN ('delivery_attempts_append_only', 'delivery_attempts_check_claimed')") != 2 {
		t.Error("rolling back 00014 must keep the 00013 triggers")
	}
	// The original, looser URL shape is back: port 0 was accepted by 00011.
	var org, user string
	q, _ := env.Pool.Querier()
	if err := q.QueryRow(ctx, "INSERT INTO organizations (name, slug) VALUES ('Org', 'rollback-org') RETURNING id").Scan(&org); err != nil {
		t.Fatal(err)
	}
	if err := q.QueryRow(ctx, "INSERT INTO users (email, normalized_email, password_hash, display_name) VALUES ('r@x.io','r@x.io','h','R') RETURNING id").Scan(&user); err != nil {
		t.Fatal(err)
	}
	insert := "INSERT INTO destinations (id, organization_id, name, url, signing_secret_ciphertext, encryption_key_version, created_by_user_id) " +
		"VALUES (gen_random_uuid(), $1, $2, $3, " + cipher + ", 1, $4)"
	if err := exec(insert, org, "Zero port", "https://example.com:0/x", user); err != nil {
		t.Errorf("00011's original URL shape must be restored (port 0 was accepted then): %v", err)
	}
	if err := exec("DELETE FROM destinations WHERE name = 'Zero port'"); err != nil {
		t.Errorf("deleting must work again after the rollback: %v", err)
	}

	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("re-applying 00014: %v", err)
	}
	if err := exec(insert, org, "Zero port 2", "https://example.com:0/x", user); err == nil {
		t.Error("port 0 must be rejected again after re-applying")
	}
	if err := exec(insert, org, "Kept", "https://example.com/x", user); err != nil {
		t.Fatalf("a valid destination must be accepted after re-applying: %v", err)
	}
	// A row-level trigger only fires for rows that exist, so a row is needed to prove it.
	if err := exec("DELETE FROM destinations WHERE name = 'Kept'"); err == nil {
		t.Error("DELETE must be rejected again after re-applying")
	}
}

// TestAttemptInsertWaitsForAConcurrentStateChange proves the attempt trigger
// takes the delivery row lock: while another transaction holds an uncommitted
// state change, the insert must wait and then judge the committed state. Without the lock
// it would read the stale IN_FLIGHT row and store an attempt for a delivery that moved on.
func TestAttemptInsertWaitsForAConcurrentStateChange(t *testing.T) {
	f := newDeliveryFixture(t)
	id := f.inFlightAt(t, f.orgID, f.dest1, f.ep1, 1)
	ctx := context.Background()

	moved := make(chan struct{})   // the state change is applied but not committed
	release := make(chan struct{}) // commit it
	reaperDone := make(chan error, 1)
	go func() {
		reaperDone <- f.env.Pool.InTx(ctx, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, retrySQL, id); err != nil {
				return err
			}
			close(moved)
			<-release
			return nil
		})
	}()
	<-moved

	writerDone := make(chan error, 1)
	go func() {
		_, err := f.insertAttempt(id, f.orgID, 1, "NETWORK_ERROR", nil)
		writerDone <- err
	}()
	select {
	case err := <-writerDone:
		close(release)
		<-reaperDone
		t.Fatalf("the attempt insert did not wait for the concurrent state change (result: %v)", err)
	case <-time.After(400 * time.Millisecond):
		// Still blocked on the row lock, as required.
	}
	close(release)
	if err := <-reaperDone; err != nil {
		t.Fatal(err)
	}
	if err := <-writerDone; err == nil || !strings.Contains(err.Error(), "IN_FLIGHT") {
		t.Fatalf("after the state change commits, the insert must be rejected as not IN_FLIGHT: %v", err)
	}
	if f.attemptRows(t, id) != 0 || f.status(t, id) != "PENDING" {
		t.Fatal("no attempt may be stored for a delivery that left IN_FLIGHT")
	}
}
