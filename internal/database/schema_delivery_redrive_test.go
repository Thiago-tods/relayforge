package database_test

import (
	"context"
	"fmt"
	"io/fs"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Thiago-tods/relayforge/internal/database"
	"github.com/Thiago-tods/relayforge/internal/delivery"
)

// Migration 00022 (ADR 34) lets a FAILED delivery return to PENDING in exactly one guarded way. These tests drive the
// trigger with direct SQL, which is the part of the guarantee the service does not provide.

// failedRow inserts a FAILED delivery with the given budget and one attempt. A legacy row has no failure reason.
func (f *deliveryFixture) failedRow(t *testing.T, max int, legacy bool) string {
	t.Helper()
	d := f.pending(t)
	d.status, d.attempts, d.max, d.httpStatus, d.completed, d.legacy = "FAILED", 1, max, 500, "now()", legacy
	if legacy {
		f.env.Exec(t, "ALTER TABLE deliveries DROP CONSTRAINT deliveries_failed_has_reason")
		defer f.env.Exec(t, "ALTER TABLE deliveries ADD CONSTRAINT deliveries_failed_has_reason CHECK (status <> 'FAILED' OR failure_reason IS NOT NULL) NOT VALID")
	}
	return f.mustDelivery(t, d)
}

// redriveSet is the complete assignment of a correct redrive to the given budget.
func redriveSet(newMax int) string {
	return fmt.Sprintf("status = 'PENDING', max_attempts = %d, failure_reason = NULL, completed_at = NULL, next_attempt_at = now()", newMax)
}

// updateWith runs one UPDATE of a delivery in its own transaction, with the redrive flag set or not.
func (f *deliveryFixture) updateWith(flag bool, id, set string) error {
	ctx := context.Background()
	return f.env.Pool.InTx(ctx, func(tx pgx.Tx) error {
		if flag {
			if _, err := tx.Exec(ctx, "SELECT set_config('relayforge.redrive', 'on', true)"); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, "UPDATE deliveries SET "+set+" WHERE id = $1", id)
		return err
	})
}

func (f *deliveryFixture) mustReject(t *testing.T, label string, err error, want string) {
	t.Helper()
	if err == nil {
		t.Errorf("%s: accepted, want a rejection", label)
		return
	}
	if !strings.Contains(err.Error(), want) {
		t.Errorf("%s: rejected by the wrong rule: %v (want %q)", label, err, want)
	}
}

func TestRedriveTriggerAllowsOnlyTheGuardedFailedToPendingTransition(t *testing.T) {
	f := newDeliveryFixture(t)
	const terminal = "immutable" // the identity rule ("identity columns are immutable") or the terminal rule ("terminal status FAILED is immutable")

	t.Run("accepts exactly +5 capped at 50, with the flag", func(t *testing.T) {
		for _, c := range []struct{ from, to int }{{5, 10}, {8, 13}, {10, 15}, {45, 50}, {48, 50}} {
			id := f.failedRow(t, c.from, false)
			if err := f.updateWith(true, id, redriveSet(c.to)); err != nil {
				t.Errorf("%d -> %d: %v", c.from, c.to, err)
				continue
			}
			var status string
			var max, attempts int
			var reason, completed *string
			q, _ := f.env.Pool.Querier()
			if err := q.QueryRow(context.Background(), "SELECT status, max_attempts, attempt_count, failure_reason, completed_at::text FROM deliveries WHERE id = $1", id).
				Scan(&status, &max, &attempts, &reason, &completed); err != nil {
				t.Fatal(err)
			}
			if status != "PENDING" || max != c.to || attempts != 1 || reason != nil || completed != nil {
				t.Errorf("%d -> %d: %s max %d attempts %d reason %v completed %v", c.from, c.to, status, max, attempts, reason, completed)
			}
		}
	})

	t.Run("accepts a legacy FAILED row without a recorded reason", func(t *testing.T) {
		id := f.failedRow(t, 8, true)
		if err := f.updateWith(true, id, redriveSet(13)); err != nil {
			t.Fatalf("legacy row: %v", err)
		}
	})

	t.Run("rejects everything else", func(t *testing.T) {
		f.mustReject(t, "no flag", f.updateWith(false, f.failedRow(t, 5, false), redriveSet(10)), terminal)
		f.mustReject(t, "+4", f.updateWith(true, f.failedRow(t, 5, false), redriveSet(9)), terminal)
		f.mustReject(t, "+6", f.updateWith(true, f.failedRow(t, 5, false), redriveSet(11)), terminal)
		f.mustReject(t, "+0 (budget unchanged)", f.updateWith(true, f.failedRow(t, 5, false), redriveSet(5)), terminal)
		f.mustReject(t, "48 -> 49 (must cap at 50)", f.updateWith(true, f.failedRow(t, 48, false), redriveSet(49)), terminal)
		f.mustReject(t, "budget already 50", f.updateWith(true, f.failedRow(t, 50, false), redriveSet(50)), terminal)
		f.mustReject(t, "decrease", f.updateWith(true, f.failedRow(t, 10, false), redriveSet(5)), terminal)
		f.mustReject(t, "attempt_count changed", f.updateWith(true, f.failedRow(t, 10, false), redriveSet(15)+", attempt_count = 2"), terminal)
		f.mustReject(t, "failure reason kept", f.updateWith(true, f.failedRow(t, 5, false),
			"status = 'PENDING', max_attempts = 10, completed_at = NULL, next_attempt_at = now()"), terminal)
		f.mustReject(t, "completed_at kept", f.updateWith(true, f.failedRow(t, 5, false),
			"status = 'PENDING', max_attempts = 10, failure_reason = NULL, next_attempt_at = now()"), terminal)
		f.mustReject(t, "FAILED -> IN_FLIGHT", f.updateWith(true, f.failedRow(t, 5, false),
			"status = 'IN_FLIGHT', attempt_count = 2, lease_expires_at = now() + interval '1 minute', claim_token = gen_random_uuid(), claimed_by = 'w'"), terminal)
		f.mustReject(t, "FAILED -> CANCELLED", f.updateWith(true, f.failedRow(t, 5, false), "status = 'CANCELLED'"), terminal)
		f.mustReject(t, "FAILED budget change without a status change", f.updateWith(true, f.failedRow(t, 5, false), "max_attempts = 10"), "identity columns are immutable")
	})

	t.Run("SUCCEEDED and CANCELLED stay immutable, flag or not", func(t *testing.T) {
		for _, st := range []string{"SUCCEEDED", "CANCELLED"} {
			for _, flag := range []bool{false, true} {
				d := f.pending(t)
				d.status, d.attempts, d.completed = st, 1, "now()"
				if st == "SUCCEEDED" {
					d.httpStatus = 204
				}
				id := f.mustDelivery(t, d)
				f.mustReject(t, fmt.Sprintf("%s -> PENDING (flag %v)", st, flag), f.updateWith(flag, id, redriveSet(13)), terminal)
			}
		}
	})

	t.Run("the budget of a live delivery cannot change even with the flag", func(t *testing.T) {
		for _, st := range []string{"PENDING", "IN_FLIGHT"} {
			d := f.pending(t)
			d.attempts = 1
			if st == "IN_FLIGHT" {
				d.status, d.lease, d.token, d.by = "IN_FLIGHT", inFlightLease, tokenExpr, "'w1'"
			}
			id := f.mustDelivery(t, d)
			f.mustReject(t, st+" budget change", f.updateWith(true, id, "max_attempts = 13"), "identity columns are immutable")
		}
	})
}

func TestRedriveKeepsTheAttemptHistoryAndContinuesTheNumbering(t *testing.T) {
	f := newDeliveryFixture(t)
	ctx := context.Background()
	q, _ := f.env.Pool.Querier()

	id := f.mustDelivery(t, dv{org: f.orgID, event: f.newEvent(t, f.orgID, f.ep1), dest: f.dest1, max: 8})
	f.accept(t, "claim", claimSQL, id)
	f.failWithReason(t, id, 1, "DESTINATION_REJECTED")
	var before string
	if err := q.QueryRow(ctx, attemptHistorySQL, id).Scan(&before); err != nil {
		t.Fatal(err)
	}

	if err := f.updateWith(true, id, redriveSet(13)); err != nil {
		t.Fatalf("redrive: %v", err)
	}
	var after string
	if err := q.QueryRow(ctx, attemptHistorySQL, id).Scan(&after); err != nil || after != before {
		t.Fatalf("the redrive changed the history: %v\n%s\n%s", err, before, after)
	}
	// The delivery goes through the normal claim: attempt number 2 follows attempt number 1, with no gap and no reuse.
	f.accept(t, "claim after redrive", claimSQL, id)
	if n, err := f.insertAttempt(id, f.orgID, 1, "HTTP_ERROR", 500); err == nil || n != 0 {
		t.Errorf("attempt number 1 must not be recorded again: %d %v", n, err)
	}
	if n, err := f.insertAttempt(id, f.orgID, 3, "HTTP_ERROR", 500); err == nil || n != 0 {
		t.Errorf("attempt number 3 would leave a gap: %d %v", n, err)
	}
	if n, err := f.insertAttempt(id, f.orgID, 2, "HTTP_ERROR", 500); err != nil || n != 1 {
		t.Errorf("attempt number 2 must be accepted: %d %v", n, err)
	}
	// The history stays append-only.
	f.reject(t, "updating an attempt after a redrive", "UPDATE delivery_attempts SET outcome = 'TIMEOUT' WHERE delivery_id = $1", id)
	f.reject(t, "deleting an attempt after a redrive", "DELETE FROM delivery_attempts WHERE delivery_id = $1", id)
}

func TestTheRedriveFlagIsTransactionLocal(t *testing.T) {
	f := newDeliveryFixture(t)
	ctx := context.Background()
	q, _ := f.env.Pool.Querier()
	if err := f.updateWith(true, f.failedRow(t, 5, false), redriveSet(10)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		var v *string
		if err := q.QueryRow(ctx, "SELECT current_setting('relayforge.redrive', true)").Scan(&v); err != nil {
			t.Fatal(err)
		}
		if v != nil && *v == "on" {
			t.Fatal("the flag leaked out of its transaction")
		}
	}
	// A later transaction without the flag is still refused, and a rollback leaves nothing behind either.
	f.mustReject(t, "a later flagless redrive", f.updateWith(false, f.failedRow(t, 5, false), redriveSet(10)), "immutable")
	id := f.failedRow(t, 5, false)
	err := f.env.Pool.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT set_config('relayforge.redrive', 'on', true)"); err != nil {
			return err
		}
		return errRollBack
	})
	if err != errRollBack {
		t.Fatal(err)
	}
	f.mustReject(t, "after a rolled-back flag", f.updateWith(false, id, redriveSet(10)), "immutable")
}

// functionBody extracts the text between the dollar quotes of the named function in a migration file.
func functionBody(t *testing.T, file, fn string) string {
	t.Helper()
	raw, err := fs.ReadFile(database.EmbeddedMigrations(), file)
	if err != nil {
		t.Fatal(err)
	}
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	start := strings.Index(text, "FUNCTION "+fn+"() RETURNS trigger")
	if start < 0 {
		t.Fatalf("%s: function %s not found", file, fn)
	}
	text = text[start:]
	open := strings.Index(text, "$$")
	closeAt := strings.Index(text[open+2:], "$$")
	if open < 0 || closeAt < 0 {
		t.Fatalf("%s: no dollar-quoted body", file)
	}
	return strings.TrimSpace(text[open+2 : open+2+closeAt])
}

func TestMigration22RollsBackToTheFunctionOf00012AndReapplies(t *testing.T) {
	f := newDeliveryFixture(t)
	ctx := context.Background()
	db, err := f.env.Pool.SQLDB()
	if err != nil {
		t.Fatal(err)
	}
	provider := newGooseProvider(t, db)
	q, _ := f.env.Pool.Querier()
	const fn = "relayforge_enforce_delivery_transition"
	source := func() string {
		var s string
		if err := q.QueryRow(ctx, "SELECT prosrc FROM pg_proc WHERE proname = $1", fn).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(strings.ReplaceAll(s, "\r\n", "\n"))
	}
	if got := source(); got != functionBody(t, "00022_delivery_redrive.sql", fn) || !strings.Contains(got, "relayforge.redrive") {
		t.Fatal("after a clean migration the function must be the one of 00022")
	}

	redriven := f.failedRow(t, 8, false)
	if err := f.updateWith(true, redriven, redriveSet(13)); err != nil {
		t.Fatalf("redrive before the rollback: %v", err)
	}
	stillFailed := f.failedRow(t, 8, false)
	rows := f.env.Count(t, "SELECT count(*) FROM deliveries")

	if _, err := provider.DownTo(ctx, 21); err != nil {
		t.Fatalf("rollback to 21: %v", err)
	}
	if got, want := source(), functionBody(t, "00012_create_deliveries.sql", fn); got != want {
		t.Fatalf("the rollback must restore the function of 00012 exactly:\n%s\n---\n%s", got, want)
	}
	if f.env.Count(t, "SELECT count(*) FROM deliveries") != rows || f.status(t, redriven) != "PENDING" || f.env.Count(t, "SELECT max_attempts FROM deliveries WHERE id = $1", redriven) != 13 {
		t.Fatal("the rollback must keep every delivery, the redriven one included")
	}
	f.mustReject(t, "redrive after the rollback", f.updateWith(true, stillFailed, redriveSet(13)), "immutable")
	// The old function still lets the redriven row live: claims and retries work on it.
	f.accept(t, "claim of a redriven row under the old function", claimSQL, redriven)

	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if got := source(); got != functionBody(t, "00022_delivery_redrive.sql", fn) {
		t.Fatal("up again must restore the function of 00022")
	}
	if err := f.updateWith(true, stillFailed, redriveSet(13)); err != nil {
		t.Fatalf("redrive after re-applying: %v", err)
	}
	// A finished migration run again changes nothing.
	if _, err := provider.Up(ctx); err != nil {
		t.Fatal(err)
	}
}

// TestRedriveStatementsAreIndexLookupsOnTheRowId EXPLAINs the exact statements the redrive runs (they are exported
// constants of internal/delivery). The table here is tiny, so the planner is told not to use sequential scans: the
// assertion is that every statement is an index lookup on the row id (the primary key or a unique key that contains it), so none of them can depend on
// the size of the delivery history. It is not a timing claim.
func TestRedriveStatementsAreIndexLookupsOnTheRowId(t *testing.T) {
	f := newDeliveryFixture(t)
	ctx := context.Background()
	id := f.failedRow(t, 8, false)
	q, _ := f.env.Pool.Querier()
	var eventID string
	if err := q.QueryRow(ctx, "SELECT event_id::text FROM deliveries WHERE id = $1", id).Scan(&eventID); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, sql string
		args      []any
	}{
		{"destination of a delivery", delivery.RedriveDestinationIDStatement, []any{id, f.orgID}},
		{"delivery lock", delivery.RedriveLockStatement, []any{id, f.orgID}},
		{"payload check", delivery.RedrivePayloadStatement, []any{eventID, f.orgID}},
		{"redrive update", delivery.RedriveUpdateStatement, []any{id, f.orgID, time.Now().UTC(), 1, delivery.RedriveBudgetStep, delivery.MaxAttemptsCeiling}},
	} {
		var plan []string
		err := f.env.Pool.InTx(ctx, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, "SET LOCAL enable_seqscan = off"); err != nil {
				return err
			}
			rows, err := tx.Query(ctx, "EXPLAIN "+c.sql, c.args...)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var line string
				if err := rows.Scan(&line); err != nil {
					return err
				}
				plan = append(plan, line)
			}
			return rows.Err()
		})
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		text := strings.Join(plan, "\n")
		if !strings.Contains(text, "Index Scan using") || !strings.Contains(text, "(id = '") || strings.Contains(text, "Seq Scan") {
			t.Errorf("%s: expected an index scan on the row id without a sequential scan:\n%s", c.name, text)
		}
	}
}
