package database_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// failWithReason ends an IN_FLIGHT delivery the way delivery.Finish does: the terminal attempt
// (with its terminal_reason) first, then the fenced update that sets the delivery's failure_reason.
func (f *deliveryFixture) failWithReason(t *testing.T, id string, attempt int, reason string) {
	t.Helper()
	ctx := context.Background()
	token := f.claimToken(t, id)
	err := f.env.Pool.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "INSERT INTO delivery_attempts (organization_id, delivery_id, attempt_number, started_at, finished_at, outcome, http_status, duration_ms, terminal_reason) "+
			"VALUES ($1, $2, $3, now() - interval '1 second', now(), 'HTTP_ERROR', 404, 5, $4)", f.orgID, id, attempt, reason); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, "UPDATE deliveries SET status = 'FAILED', lease_expires_at = NULL, claim_token = NULL, claimed_by = NULL, "+
			"last_http_status = 404, completed_at = now(), failure_reason = $3 WHERE id = $1 AND claim_token = $2", id, token, reason)
		return err
	})
	if err != nil {
		t.Fatalf("fail the delivery with %s: %v", reason, err)
	}
}

const attemptHistorySQL = `SELECT coalesce(string_agg(a::text, '|' ORDER BY attempt_number), '') FROM delivery_attempts a WHERE delivery_id = $1`

var errRollBack = errors.New("roll back the test transaction")

// TestTheReasonConstraintsLeaveRoomForARedrive checks the claim ADR 0030 makes about a future
// redrive: it can clear deliveries.failure_reason (and completed_at) in one UPDATE while the
// attempt history, including the terminal reason of the attempt that failed, stays untouched.
//
// Redrive is NOT implemented, and the production transition trigger still forbids it. The test
// therefore disables that one trigger inside a transaction and always rolls the transaction back,
// so the trigger is restored (DDL is transactional in PostgreSQL) and nothing persists.
func TestTheReasonConstraintsLeaveRoomForARedrive(t *testing.T) {
	f := newDeliveryFixture(t)
	ctx := context.Background()
	q, _ := f.env.Pool.Querier()
	const trigger = "deliveries_enforce_transition"
	triggerState := func() string {
		var s string
		if err := q.QueryRow(ctx, "SELECT tgenabled::text FROM pg_trigger WHERE tgname = $1 AND tgrelid = 'deliveries'::regclass", trigger).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	if triggerState() != "O" {
		t.Fatalf("setup: the transition trigger must be enabled")
	}

	t.Run("a failed delivery with budget left can return to PENDING and keeps its history", func(t *testing.T) {
		id := f.inFlightAt(t, f.orgID, f.dest1, f.ep1, 1) // max_attempts is 50
		f.failWithReason(t, id, 1, "DESTINATION_REJECTED")
		var historyBefore string
		if err := q.QueryRow(ctx, attemptHistorySQL, id).Scan(&historyBefore); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(historyBefore, "DESTINATION_REJECTED") {
			t.Fatalf("setup: the terminal attempt must carry its reason: %s", historyBefore)
		}

		err := f.env.Pool.InTx(ctx, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, "ALTER TABLE deliveries DISABLE TRIGGER "+trigger); err != nil {
				return err
			}
			// The constraints refuse a half-done redrive: leaving the reason on a PENDING delivery.
			sp, err := tx.Begin(ctx)
			if err != nil {
				return err
			}
			_, halfErr := sp.Exec(ctx, "UPDATE deliveries SET status = 'PENDING', completed_at = NULL, next_attempt_at = now() + interval '1 minute' WHERE id = $1", id)
			if err := sp.Rollback(ctx); err != nil {
				return err
			}
			if halfErr == nil || !strings.Contains(halfErr.Error(), "deliveries_failure_reason_only_when_failed") {
				t.Errorf("a PENDING delivery that keeps its failure reason must be rejected by the constraint, got %v", halfErr)
			}

			// The complete redrive: status, reason and completion time change together.
			if _, err := tx.Exec(ctx, "UPDATE deliveries SET status = 'PENDING', failure_reason = NULL, completed_at = NULL, next_attempt_at = now() + interval '1 minute' WHERE id = $1", id); err != nil {
				t.Errorf("the constraints must accept a redriven delivery: %v", err)
				return errRollBack
			}
			var status string
			var reason, completed *string
			if err := tx.QueryRow(ctx, "SELECT status, failure_reason, completed_at::text FROM deliveries WHERE id = $1", id).Scan(&status, &reason, &completed); err != nil {
				return err
			}
			if status != "PENDING" || reason != nil || completed != nil {
				t.Errorf("redriven delivery: %s %v %v", status, reason, completed)
			}
			var historyDuring string
			if err := tx.QueryRow(ctx, attemptHistorySQL, id).Scan(&historyDuring); err != nil {
				return err
			}
			if historyDuring != historyBefore {
				t.Errorf("the attempt history changed:\n%s\n%s", historyBefore, historyDuring)
			}
			var kept int
			if err := tx.QueryRow(ctx, "SELECT count(*) FROM delivery_attempts WHERE delivery_id = $1 AND attempt_number = 1 AND terminal_reason = 'DESTINATION_REJECTED'", id).Scan(&kept); err != nil {
				return err
			}
			if kept != 1 {
				t.Errorf("the previous terminal attempt must keep its reason")
			}
			// The history stays append-only even with the transition trigger off.
			sp2, err := tx.Begin(ctx)
			if err != nil {
				return err
			}
			_, clearErr := sp2.Exec(ctx, "UPDATE delivery_attempts SET terminal_reason = NULL WHERE delivery_id = $1", id)
			if err := sp2.Rollback(ctx); err != nil {
				return err
			}
			if clearErr == nil {
				t.Errorf("a terminal reason was cleared: attempt history must stay append-only")
			}
			return errRollBack
		})
		if !errors.Is(err, errRollBack) {
			t.Fatalf("the transaction must end in the deliberate rollback, got %v", err)
		}

		// Rolled back: the trigger is enabled again and nothing was persisted.
		if triggerState() != "O" {
			t.Fatalf("the transition trigger was left in state %q", triggerState())
		}
		if f.env.Count(t, "SELECT count(*) FROM deliveries WHERE id = $1 AND status = 'FAILED' AND failure_reason = 'DESTINATION_REJECTED' AND completed_at IS NOT NULL", id) != 1 {
			t.Fatal("the rollback must leave the delivery FAILED with its reason")
		}
		var historyAfter string
		if err := q.QueryRow(ctx, attemptHistorySQL, id).Scan(&historyAfter); err != nil || historyAfter != historyBefore {
			t.Fatalf("history after the rollback: %v\n%s\n%s", err, historyBefore, historyAfter)
		}
		f.reject(t, "a redrive with the trigger enabled", "UPDATE deliveries SET status = 'PENDING', failure_reason = NULL, completed_at = NULL WHERE id = $1", id)
	})

	// A delivery that used its whole budget is not redrivable by a status change alone, even with
	// the trigger off: PENDING requires attempt_count < max_attempts and max_attempts is fixed.
	t.Run("an exhausted delivery needs a retry-budget decision as well", func(t *testing.T) {
		id := f.mustDelivery(t, dv{org: f.orgID, event: f.newEvent(t, f.orgID, f.ep1), dest: f.dest1, max: 1})
		f.accept(t, "claim", claimSQL, id)
		f.failWithReason(t, id, 1, "RETRIES_EXHAUSTED")
		err := f.env.Pool.InTx(ctx, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, "ALTER TABLE deliveries DISABLE TRIGGER "+trigger); err != nil {
				return err
			}
			_, uerr := tx.Exec(ctx, "UPDATE deliveries SET status = 'PENDING', failure_reason = NULL, completed_at = NULL, next_attempt_at = now() WHERE id = $1", id)
			if uerr == nil || !strings.Contains(uerr.Error(), "deliveries_attempts_match_status") {
				t.Errorf("an exhausted delivery must not return to PENDING with its budget unchanged, got %v", uerr)
			}
			return errRollBack
		})
		if !errors.Is(err, errRollBack) {
			t.Fatalf("got %v", err)
		}
		if triggerState() != "O" {
			t.Fatalf("the transition trigger was left in state %q", triggerState())
		}
	})
}
