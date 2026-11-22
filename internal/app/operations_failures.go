package app

import (
	"context"
	"strconv"
	"time"

	"github.com/Thiago-tods/relayforge/internal/delivery"
	"github.com/Thiago-tods/relayforge/internal/tenancy"
)

// defaultFailureFeedWindow is the span of the failure feed when `from` is not given. The longest span is
// maxOperationsWindow (31 days), like the delivery summary.
const defaultFailureFeedWindow = 7 * 24 * time.Hour

// FailureFeedQuery is the raw query of the failure feed. Every field is client text, validated only after the
// caller is authorized.
type FailureFeedQuery struct {
	Limit, Cursor, FailureReason, DestinationID, From, To string
}

// FailureFeed returns one page of the organization's current failures (ADR 32). Authorization runs first and
// needs the database, so a caller who is not a member gets 404 whatever the parameters say. Everything is
// validated before the deliveries table is read.
func (s *Service) FailureFeed(ctx context.Context, p Principal, orgID string, q FailureFeedQuery) (delivery.FailureFeed, error) {
	db := s.mustQuerier()
	if _, err := authorize(ctx, db, p, orgID, tenancy.PermDeliveriesRead); err != nil {
		return delivery.FailureFeed{}, err
	}
	limit := delivery.DefaultFailureFeedLimit
	if q.Limit != "" {
		n, err := strconv.Atoi(q.Limit)
		if err != nil || n < 1 || n > delivery.MaxFailureFeedLimit {
			return delivery.FailureFeed{}, Validation("limit must be an integer from 1 to 100")
		}
		limit = n
	}
	if q.DestinationID != "" && !IsUUID(q.DestinationID) {
		return delivery.FailureFeed{}, Validation("destinationId must be a UUID")
	}
	if q.FailureReason != "" && !delivery.ValidFailureFilter(q.FailureReason) {
		return delivery.FailureFeed{}, Validation("failureReason must be DESTINATION_REJECTED, REDIRECT_REFUSED, EGRESS_BLOCKED, RETRIES_EXHAUSTED, INTERNAL_ERROR or UNRECORDED")
	}
	var after *delivery.FailureCursor
	if q.Cursor != "" {
		c, err := delivery.ParseFailureCursor(q.Cursor)
		if err != nil {
			return delivery.FailureFeed{}, Validation("cursor is not valid")
		}
		after = &c
	}
	now := normalizeInstant(s.now())
	from, to, err := operationsWindowOf(q.From, q.To, now, defaultFailureFeedWindow)
	if err != nil {
		return delivery.FailureFeed{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, s.operationsDeadline())
	defer cancel()
	out, err := delivery.FailureFeedOf(ctx, db, orgID, delivery.FailureFilter{
		DestinationID: q.DestinationID, Reason: q.FailureReason, From: from, To: to, After: after, Limit: limit,
	}, now)
	return out, operationsError(ctx, err)
}

// defaultAttemptFeedWindow is the span of the attempt feed when `from` is not given. Attempts are far more
// numerous than failed deliveries, so the default is shorter than the failure feed's. The longest span is
// maxOperationsWindow (31 days).
const defaultAttemptFeedWindow = 24 * time.Hour

// AttemptFeedQuery is the raw query of the attempt feed. Every field is client text, validated only after the
// caller is authorized. There is deliberately no destination filter.
type AttemptFeedQuery struct {
	Limit, Cursor, Outcome, DeliveryID, From, To string
}

// AttemptFeed returns one page of the organization's recorded attempts (ADR 33). Authorization runs first and
// needs the database, so a caller who is not a member gets 404 whatever the parameters say. Everything is
// validated before the attempts table is read.
func (s *Service) AttemptFeed(ctx context.Context, p Principal, orgID string, q AttemptFeedQuery) (delivery.AttemptFeed, error) {
	db := s.mustQuerier()
	if _, err := authorize(ctx, db, p, orgID, tenancy.PermDeliveriesRead); err != nil {
		return delivery.AttemptFeed{}, err
	}
	limit := delivery.DefaultAttemptFeedLimit
	if q.Limit != "" {
		n, err := strconv.Atoi(q.Limit)
		if err != nil || n < 1 || n > delivery.MaxAttemptFeedLimit {
			return delivery.AttemptFeed{}, Validation("limit must be an integer from 1 to 100")
		}
		limit = n
	}
	if q.DeliveryID != "" && !IsUUID(q.DeliveryID) {
		return delivery.AttemptFeed{}, Validation("deliveryId must be a UUID")
	}
	if q.Outcome != "" && !delivery.ValidAttemptOutcome(q.Outcome) {
		return delivery.AttemptFeed{}, Validation("outcome must be SUCCESS, HTTP_ERROR, REDIRECT, NETWORK_ERROR, TIMEOUT, BLOCKED_BY_POLICY or ABANDONED")
	}
	var after *delivery.AttemptCursor
	if q.Cursor != "" {
		c, err := delivery.ParseAttemptCursor(q.Cursor)
		if err != nil {
			return delivery.AttemptFeed{}, Validation("cursor is not valid")
		}
		after = &c
	}
	now := normalizeInstant(s.now())
	from, to, err := operationsWindowOf(q.From, q.To, now, defaultAttemptFeedWindow)
	if err != nil {
		return delivery.AttemptFeed{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, s.operationsDeadline())
	defer cancel()
	out, err := delivery.AttemptFeedOf(ctx, db, orgID, delivery.AttemptFilter{
		DeliveryID: q.DeliveryID, Outcome: q.Outcome, From: from, To: to, After: after, Limit: limit,
	}, now)
	return out, operationsError(ctx, err)
}

// QueueAge returns the overdue-age distribution of the organization's due deliveries. It is read-only, takes no
// row lock and uses one service-clock instant for every bucket.
func (s *Service) QueueAge(ctx context.Context, p Principal, orgID string) (delivery.QueueAge, error) {
	q := s.mustQuerier()
	if _, err := authorize(ctx, q, p, orgID, tenancy.PermDeliveriesRead); err != nil {
		return delivery.QueueAge{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, s.operationsDeadline())
	defer cancel()
	out, err := delivery.QueueAgeOf(ctx, q, orgID, normalizeInstant(s.now()))
	return out, operationsError(ctx, err)
}
