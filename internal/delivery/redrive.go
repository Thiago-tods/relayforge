package delivery

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Thiago-tods/relayforge/internal/database"
)

// Redrive budget (ADR 34): an accepted redrive raises the delivery's max_attempts by RedriveBudgetStep,
// never above MaxAttemptsCeiling. The database trigger enforces the same arithmetic.
const (
	RedriveBudgetStep  = 5
	MaxAttemptsCeiling = 50
)

// RedriveFlag is the transaction-local setting the delivery trigger requires for FAILED -> PENDING
// (migration 00022). It is an application guard, not a security boundary.
const RedriveFlag = "relayforge.redrive"

// NextMaxAttempts is the budget after one accepted redrive: min(old + 5, 50).
func NextMaxAttempts(old int) int { return min(old+RedriveBudgetStep, MaxAttemptsCeiling) }

// ErrRedriveRace is returned by ApplyRedrive when the guarded UPDATE matched no row although the caller
// holds the row lock and has just checked the state. It means a programming error, not a client mistake.
var ErrRedriveRace = errors.New("delivery changed while it was being redriven")

// RedriveState is the part of a delivery a redrive decides on.
type RedriveState struct {
	ID            string
	EventID       string
	DestinationID string
	Status        string
	AttemptCount  int
	MaxAttempts   int
	// FailureReason is nil for a delivery that failed before reasons were recorded.
	FailureReason *string
}

// DestinationIDOf returns the destination of a delivery of orgID without locking it. The column never
// changes (the delivery trigger forbids it), so reading it before the locks is safe; it only tells the
// caller which destination row to lock first.
func DestinationIDOf(ctx context.Context, db database.Querier, orgID, deliveryID string) (string, error) {
	var id string
	err := db.QueryRow(ctx, RedriveDestinationIDStatement, deliveryID, orgID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("select delivery destination: %w", err)
	}
	return id, nil
}

// LockDestinationShared loads a destination of orgID and takes a shared row lock on it. Revoke, pause,
// activate, rename, re-target and rotate take FOR UPDATE on the same row, so they conflict with it.
func LockDestinationShared(ctx context.Context, db database.Querier, orgID, id string) (Destination, error) {
	d, err := scanDestination(db.QueryRow(ctx,
		"SELECT "+destinationColumns+" FROM destinations WHERE id = $1 AND organization_id = $2 FOR SHARE", id, orgID))
	return d, destinationErr(err, "lock destination")
}

// LockForRedrive loads a delivery of orgID and locks its row (FOR NO KEY UPDATE, the lock an UPDATE of the
// row takes, so no upgrade is needed later). A concurrent redrive waits here and then reads the
// committed state.
func LockForRedrive(ctx context.Context, db database.Querier, orgID, deliveryID string) (RedriveState, error) {
	var s RedriveState
	err := db.QueryRow(ctx, RedriveLockStatement, deliveryID, orgID).
		Scan(&s.ID, &s.EventID, &s.DestinationID, &s.Status, &s.AttemptCount, &s.MaxAttempts, &s.FailureReason)
	if errors.Is(err, pgx.ErrNoRows) {
		return RedriveState{}, ErrNotFound
	}
	if err != nil {
		return RedriveState{}, fmt.Errorf("lock delivery for redrive: %w", err)
	}
	return s, nil
}

// EventPayloadIntact reports whether the delivery's event exists in orgID, has a non-empty payload, and
// the payload still hashes to the SHA-256 recorded at ingestion. The hash is computed in PostgreSQL: the
// payload never reaches the application. A payload is never reconstructed or replaced (ADR 34).
func EventPayloadIntact(ctx context.Context, db database.Querier, orgID, eventID string) (bool, error) {
	var ok bool
	err := db.QueryRow(ctx, RedrivePayloadStatement, eventID, orgID).Scan(&ok)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check event payload: %w", err)
	}
	return ok, nil
}

// ApplyRedrive moves a FAILED delivery back to PENDING and raises its budget, in the caller's
// transaction. The caller must hold the row lock (LockForRedrive) and must have checked the state.
//
// The transaction-local flag is on only for the UPDATE and is switched off right after it. The UPDATE
// repeats the eligibility predicates, so a state the caller got wrong changes nothing and returns
// ErrRedriveRace. attempt_count is not touched, and last_http_status and last_error_class keep
// describing the last attempt, as they do for any delivery waiting for a retry. It returns the new
// budget. next_attempt_at is now, so the delivery is immediately claimable.
func ApplyRedrive(ctx context.Context, tx database.Querier, orgID string, prev RedriveState, now time.Time) (int, error) {
	if _, err := tx.Exec(ctx, "SELECT set_config('"+RedriveFlag+"', 'on', true)"); err != nil {
		return 0, fmt.Errorf("set redrive flag: %w", err)
	}
	var newMax int
	err := tx.QueryRow(ctx, RedriveUpdateStatement,
		prev.ID, orgID, now, prev.AttemptCount, RedriveBudgetStep, MaxAttemptsCeiling).Scan(&newMax)
	if _, ferr := tx.Exec(ctx, "SELECT set_config('"+RedriveFlag+"', 'off', true)"); ferr != nil && err == nil {
		err = fmt.Errorf("clear redrive flag: %w", ferr)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrRedriveRace
	}
	if err != nil {
		return 0, fmt.Errorf("redrive delivery: %w", err)
	}
	return newMax, nil
}

// The exact SQL of the redrive path, exported only so that a test can EXPLAIN the statements the service really
// runs instead of copies of them (like ClaimDueStatement). Nothing else should use them.
const (
	// RedriveDestinationIDStatement (deliveryID, orgID) reads the immutable destination of a delivery without a lock.
	RedriveDestinationIDStatement = "SELECT destination_id::text FROM deliveries WHERE id = $1 AND organization_id = $2"
	// RedriveLockStatement (deliveryID, orgID) locks the delivery row and reads what the redrive decides on.
	RedriveLockStatement = `
		SELECT id::text, event_id::text, destination_id::text, status, attempt_count, max_attempts, failure_reason
		  FROM deliveries WHERE id = $1 AND organization_id = $2
		   FOR NO KEY UPDATE`
	// RedrivePayloadStatement (eventID, orgID) checks the stored payload against its recorded SHA-256 inside PostgreSQL.
	RedrivePayloadStatement = `
		SELECT octet_length(payload) > 0 AND sha256(payload) = payload_sha256
		  FROM webhook_events WHERE id = $1 AND organization_id = $2`
	// RedriveUpdateStatement (deliveryID, orgID, now, attemptCount, step, ceiling) is the one UPDATE of a redrive.
	RedriveUpdateStatement = `
		UPDATE deliveries
		   SET status = 'PENDING', max_attempts = LEAST(max_attempts + $5, $6), next_attempt_at = $3,
		       failure_reason = NULL, completed_at = NULL
		 WHERE id = $1 AND organization_id = $2 AND status = 'FAILED' AND attempt_count = $4 AND max_attempts < $6
		RETURNING max_attempts`
)
