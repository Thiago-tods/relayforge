package app

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/Thiago-tods/relayforge/internal/delivery"
	"github.com/Thiago-tods/relayforge/internal/platform/apperror"
	"github.com/Thiago-tods/relayforge/internal/tenancy"
)

const (
	// defaultOperationsDeadline bounds the database work of one operational read, so a heavy read
	// cannot hold one of the API's pooled connections for long. It is an operational bound on the
	// request: the statement is cancelled through the context, which depends on PostgreSQL honoring the
	// cancellation request (it normally does within milliseconds, but a cancel is not instantaneous).
	// Config.OperationsDeadline overrides it.
	defaultOperationsDeadline = 5 * time.Second
	// defaultOperationsWindow is the span of the delivery summary when `from` is not given.
	defaultOperationsWindow = 24 * time.Hour
	// maxOperationsWindow is the longest span of the delivery summary.
	maxOperationsWindow = 31 * 24 * time.Hour
	// minOperationsYear and maxOperationsYear are the years (in UTC) a window bound may have. RFC 3339
	// allows years 0000 to 9999, but year 0000 does not exist in the AD/BC calendar PostgreSQL uses
	// (it has no year zero and rejects such input). It is refused, together with any instant that,
	// once converted to UTC, falls outside 0001 to 9999 (for example 0001-01-01T00:00:00+14:00).
	minOperationsYear = 1
	maxOperationsYear = 9999
)

// ErrOperationsTimeout is returned when an operational read exceeds its deadline. The message
// carries no database detail.
var ErrOperationsTimeout = apperror.New(http.StatusServiceUnavailable, "read_model_timeout",
	"the query took too long; try a shorter time window")

func (s *Service) operationsDeadline() time.Duration {
	if s.cfg.OperationsDeadline > 0 {
		return s.cfg.OperationsDeadline
	}
	return defaultOperationsDeadline
}

// QueueHealth returns the organization's queue health. It is read-only: it takes no row lock
// and changes nothing, so it cannot interfere with the delivery workers.
//
// GeneratedAt is the service clock when the request started (UTC, microsecond precision like every
// bound sent to the database), not a database timestamp, and every time comparison of the snapshot (due, delayed, expired leases, the oldest due age) uses that one
// value, so the numbers agree with each other and with GeneratedAt.
func (s *Service) QueueHealth(ctx context.Context, p Principal, orgID string) (delivery.QueueSnapshot, error) {
	q := s.mustQuerier()
	if _, err := authorize(ctx, q, p, orgID, tenancy.PermDeliveriesRead); err != nil {
		return delivery.QueueSnapshot{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, s.operationsDeadline())
	defer cancel()
	out, err := delivery.QueueOf(ctx, q, orgID, normalizeInstant(s.now()))
	return out, operationsError(ctx, err)
}

// DeliverySummary returns the organization's delivery activity for the half-open window
// [from, to). Both bounds are optional RFC 3339 timestamps: `to` defaults to the service clock and
// `from` to `to` minus 24 hours. The window must be positive and at most 31 days.
//
// The authorization check runs first and needs the database, so a caller who is not a member gets
// 404 whatever the window says. The window is validated before the deliveries table is read: an
// invalid one never reaches the read models.
//
// Clock semantics: a default `to` is the service clock at the start of the request (it is the
// requested window endpoint, not a database value), and GeneratedAt is that same instant. created_at
// and completed_at are database timestamps. Deliveries created or completed just before `to` by
// transactions that commit after the read are not in the response, and a later request for the same
// window can count them: the most recent edge of a window ending "now" is not final.
func (s *Service) DeliverySummary(ctx context.Context, p Principal, orgID, fromRaw, toRaw string) (delivery.DeliverySummary, error) {
	q := s.mustQuerier()
	if _, err := authorize(ctx, q, p, orgID, tenancy.PermDeliveriesRead); err != nil {
		return delivery.DeliverySummary{}, err
	}
	now := normalizeInstant(s.now())
	from, to, err := operationsWindow(fromRaw, toRaw, now)
	if err != nil {
		return delivery.DeliverySummary{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, s.operationsDeadline())
	defer cancel()
	out, err := delivery.SummaryOf(ctx, q, orgID, from, to, now)
	return out, operationsError(ctx, err)
}

// operationsWindow validates and defaults the window. Offsets in RFC 3339 values are honored and
// both bounds are normalized the same way: converted to UTC and truncated to microseconds, the
// precision PostgreSQL stores, so the window the response echoes is exactly the window that was
// read. The maximum is inclusive: exactly 31 days is allowed.
func operationsWindow(fromRaw, toRaw string, now time.Time) (from, to time.Time, err error) {
	to = now
	if toRaw != "" {
		if to, err = time.Parse(time.RFC3339, toRaw); err != nil {
			return time.Time{}, time.Time{}, Validation("to must be an RFC 3339 timestamp")
		}
	}
	if fromRaw != "" {
		if from, err = time.Parse(time.RFC3339, fromRaw); err != nil {
			return time.Time{}, time.Time{}, Validation("from must be an RFC 3339 timestamp")
		}
	} else {
		from = to.Add(-defaultOperationsWindow)
	}
	from, to = normalizeInstant(from), normalizeInstant(to)
	if !supportedInstant(from) {
		return time.Time{}, time.Time{}, Validation("from must be a time between the years 0001 and 9999 (UTC)")
	}
	if !supportedInstant(to) {
		return time.Time{}, time.Time{}, Validation("to must be a time between the years 0001 and 9999 (UTC)")
	}
	if !from.Before(to) {
		return time.Time{}, time.Time{}, Validation("from must be before to")
	}
	if to.Sub(from) > maxOperationsWindow {
		return time.Time{}, time.Time{}, Validation("the time window must not exceed 31 days")
	}
	return from, to, nil
}

// normalizeInstant converts t to UTC and truncates it to microseconds.
func normalizeInstant(t time.Time) time.Time { return t.UTC().Truncate(time.Microsecond) }

// supportedInstant reports whether t (in UTC) is a date the database and the API both represent
// unambiguously.
func supportedInstant(t time.Time) bool {
	y := t.Year()
	return y >= minOperationsYear && y <= maxOperationsYear
}

// operationsError maps a deadline overrun to ErrOperationsTimeout and leaves other errors alone.
// A client that goes away (context.Canceled) is not a timeout: that error passes through, and the API
// renders it like any other unexpected error (a generic 500 nobody is left to read).
func operationsError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return ErrOperationsTimeout
	}
	return err
}
