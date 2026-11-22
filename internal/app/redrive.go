package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Thiago-tods/relayforge/internal/audit"
	"github.com/Thiago-tods/relayforge/internal/delivery"
	"github.com/Thiago-tods/relayforge/internal/platform/apperror"
	"github.com/Thiago-tods/relayforge/internal/tenancy"
)

// Errors of the redrive flow (ADR 34). Each has a stable code.
var (
	// ErrDeliveryNotFailed: the delivery is not FAILED now (already redriven and waiting or running,
	// succeeded, cancelled, or never failed). A repeated request lands here; there is no idempotency replay.
	ErrDeliveryNotFailed = apperror.New(http.StatusConflict, "delivery_not_failed", "the delivery is not in the FAILED status")
	// ErrDeliveryChanged: the delivery is FAILED but its attempt count differs from expectedAttemptCount, so
	// the request refers to an earlier failure of the same delivery. Read the delivery and decide again.
	ErrDeliveryChanged = apperror.New(http.StatusConflict, "delivery_changed", "the delivery changed since it was read: its attempt count is not the expected one")
	// ErrRetryBudgetExhausted: max_attempts is already at the ceiling, so no further budget can be granted.
	ErrRetryBudgetExhausted = apperror.New(http.StatusConflict, "retry_budget_exhausted", "the delivery already has the maximum retry budget")
	// ErrEventPayloadUnavailable: the stored payload is missing, empty or does not match its recorded hash.
	// A payload is never reconstructed or substituted.
	ErrEventPayloadUnavailable = apperror.New(http.StatusConflict, "event_payload_unavailable", "the event payload is not available or failed its integrity check")
	// ErrLockTimeout: a database lock was not obtained within the redrive's lock timeout. Nothing was changed.
	ErrLockTimeout = &apperror.Error{Status: http.StatusServiceUnavailable, Code: "lock_timeout",
		Message: "the request could not get its lock in time; try again", RetryAfter: time.Second}
)

const (
	// defaultRedriveLockTimeout bounds every lock wait of one redrive transaction (SET LOCAL, never global).
	defaultRedriveLockTimeout = 5 * time.Second
	maxRedriveReasonRunes     = 500
	// sqlstateLockNotAvailable is raised when lock_timeout expires.
	sqlstateLockNotAvailable = "55P03"
)

// RedriveInput is the validated-by-the-service body of a redrive request.
type RedriveInput struct {
	// Reason is the operator's reason, recorded in the audit event. 1 to 500 characters.
	Reason string
	// ExpectedAttemptCount is the attempt count of the failure the operator looked at.
	ExpectedAttemptCount int
}

// RedriveResult is what an accepted redrive returns.
type RedriveResult struct {
	Delivery              delivery.DeliveryRecord
	PreviousMaxAttempts   int
	PreviousFailureReason *string
}

func (s *Service) redriveLockTimeout() time.Duration {
	if s.cfg.RedriveLockTimeout > 0 {
		return s.cfg.RedriveLockTimeout
	}
	return defaultRedriveLockTimeout
}

// RedriveDelivery returns one FAILED delivery to the delivery pipeline (ADR 34).
//
// Authorization comes first: a non-member or a malformed organization or delivery id gets 404 and a member
// without deliveries:redrive gets 403, before parse is called, so malformed input never turns a 404 into a
// 400 for a non-member. parse then reads and validates the body.
//
// One transaction does everything, in the lock order organization (shared), destination (shared),
// delivery: the same order revoke uses, so the two cannot deadlock. The organization lock is shared
// (tenancy.LockOrganizationShared) so redrives of unrelated deliveries do not queue; it still excludes every
// membership change and every destination mutation. Then, with the delivery row locked: the delivery must be
// FAILED (409 delivery_not_failed), must still be the failure the operator saw (409 delivery_changed), its
// destination must not be revoked (409 destination_revoked; PAUSED is allowed and the worker keeps respecting
// the pause), the budget must be below the ceiling (409 retry_budget_exhausted) and the event payload must be
// present and match its recorded SHA-256 (409 event_payload_unavailable). Only then does one UPDATE set the
// delivery PENDING, raise max_attempts by five (capped at 50) and clear the failure; the audit event is written
// in the same transaction, so either all of it commits or none of it does.
//
// attempt_count is never reset and attempt history is never touched: the next claim records attempt
// attempt_count + 1. Retries of a redriven delivery use the ordinary attempt-number backoff, which is long
// for high attempt numbers; there is no separate redrive policy.
func (s *Service) RedriveDelivery(ctx context.Context, p Principal, orgID, deliveryID string, parse func() (RedriveInput, error)) (RedriveResult, error) {
	if _, err := authorize(ctx, s.mustQuerier(), p, orgID, tenancy.PermDeliveriesRedrive); err != nil {
		return RedriveResult{}, err
	}
	if !IsUUID(deliveryID) {
		return RedriveResult{}, ErrNotFound
	}
	in, err := parse()
	if err != nil {
		return RedriveResult{}, err
	}
	reason, err := normalizeRedriveReason(in.Reason)
	if err != nil {
		return RedriveResult{}, err
	}
	if in.ExpectedAttemptCount < 1 || in.ExpectedAttemptCount > delivery.MaxAttemptsCeiling {
		return RedriveResult{}, Validation("expectedAttemptCount must be between 1 and 50")
	}

	var out RedriveResult
	err = s.pool.InTx(ctx, func(tx pgx.Tx) error {
		// Transaction-local and constant-shaped (an integer of milliseconds): never a global setting.
		if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", s.redriveLockTimeout().Milliseconds())); err != nil {
			return fmt.Errorf("set lock timeout: %w", err)
		}
		if _, err := lockSharedAndAuthorize(ctx, tx, p, orgID, tenancy.PermDeliveriesRedrive); err != nil {
			return err
		}
		destID, err := delivery.DestinationIDOf(ctx, tx, orgID, deliveryID)
		if errors.Is(err, delivery.ErrNotFound) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		dest, err := delivery.LockDestinationShared(ctx, tx, orgID, destID)
		if err != nil {
			return destinationErr(err)
		}
		cur, err := delivery.LockForRedrive(ctx, tx, orgID, deliveryID)
		if errors.Is(err, delivery.ErrNotFound) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		switch {
		case cur.Status != string(delivery.StatusFailed):
			return ErrDeliveryNotFailed
		case cur.AttemptCount != in.ExpectedAttemptCount:
			return ErrDeliveryChanged
		case dest.Status == delivery.DestinationRevoked:
			return ErrDestinationRevoked
		case cur.MaxAttempts >= delivery.MaxAttemptsCeiling:
			return ErrRetryBudgetExhausted
		}
		intact, err := delivery.EventPayloadIntact(ctx, tx, orgID, cur.EventID)
		if err != nil {
			return err
		}
		if !intact {
			return ErrEventPayloadUnavailable
		}
		newMax, err := delivery.ApplyRedrive(ctx, tx, orgID, cur, s.now())
		if err != nil {
			return err
		}
		if err := audit.Record(ctx, tx, audit.Event{
			OrganizationID: orgID, ActorUserID: p.User.ID, Action: audit.DeliveryRedriven,
			ResourceType: "delivery", ResourceID: deliveryID,
			Metadata: map[string]any{
				"reason": reason, "previousStatus": cur.Status, "previousFailureReason": cur.FailureReason,
				"previousMaxAttempts": cur.MaxAttempts, "maxAttempts": newMax, "attemptCount": cur.AttemptCount,
				"destinationId": destID, "destinationStatus": dest.Status,
			},
		}); err != nil {
			return err
		}
		rec, err := delivery.DeliveryRecordByID(ctx, tx, orgID, deliveryID)
		if err != nil {
			return err
		}
		out = RedriveResult{Delivery: rec, PreviousMaxAttempts: cur.MaxAttempts, PreviousFailureReason: cur.FailureReason}
		return nil
	})
	if err != nil {
		var pg *pgconn.PgError
		if errors.As(err, &pg) && pg.Code == sqlstateLockNotAvailable {
			return RedriveResult{}, ErrLockTimeout
		}
		return RedriveResult{}, err
	}
	s.cfg.Redriven.Inc() // after the commit; a nil counter ignores it
	return out, nil
}

// normalizeRedriveReason trims the operator reason and checks it: 1 to 500 characters of valid UTF-8 with no
// control characters. It is stored in the audit record and never written to application logs.
func normalizeRedriveReason(raw string) (string, error) {
	r := strings.TrimSpace(raw)
	if r == "" || !utf8.ValidString(r) || utf8.RuneCountInString(r) > maxRedriveReasonRunes || strings.ContainsFunc(r, isControl) {
		return "", Validation("reason must be 1 to 500 printable characters")
	}
	return r, nil
}
