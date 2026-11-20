package app

import (
	"context"
	"errors"
	"time"

	"github.com/Thiago-tods/relayforge/internal/database"
	"github.com/Thiago-tods/relayforge/internal/delivery"
	"github.com/Thiago-tods/relayforge/internal/tenancy"
)

// DeliveryFilter narrows a delivery listing.
type DeliveryFilter struct {
	DestinationID string
	EventID       string
	Status        string
	// FailureReason is "", one of the five failure reasons, or UNRECORDED. See ListDeliveries.
	FailureReason string
	From, To      time.Time
}

// DeliveryDetail is a delivery with its recorded attempts.
type DeliveryDetail struct {
	Delivery delivery.DeliveryRecord
	Attempts []delivery.AttemptView
}

// ListDeliveries returns a page of the organization's deliveries. History is
// read-only: nothing here changes a delivery or an attempt.
//
// failureReason (ADR 32) is optional. It implies status FAILED: another status is a validation error, and
// UNRECORDED selects FAILED deliveries that have no recorded reason. Only a filtered request runs under the
// five-second operations deadline (503 read_model_timeout).
func (s *Service) ListDeliveries(ctx context.Context, p Principal, orgID string, f DeliveryFilter, limit, offset int) ([]delivery.DeliveryRecord, int, error) {
	q := s.mustQuerier()
	if _, err := authorize(ctx, q, p, orgID, tenancy.PermDeliveriesRead); err != nil {
		return nil, 0, err
	}
	return s.listDeliveries(ctx, q, orgID, f, limit, offset)
}

// ListDeliveriesParsed is ListDeliveries for a caller whose input is still raw client text: it authorizes first
// (one authorization query, a non-member or malformed organization is the usual 404) and only then calls parse, which
// reads and validates the pagination and timestamps. parse is never called for a caller who is not authorized, so
// malformed input can never turn a 404 into a 400 and reveal that membership. The HTTP handler uses it for a request
// that carries failureReason (ADR 32); every check that follows is the one ListDeliveries makes.
func (s *Service) ListDeliveriesParsed(ctx context.Context, p Principal, orgID string, parse func() (DeliveryFilter, int, int, error)) ([]delivery.DeliveryRecord, int, error) {
	q := s.mustQuerier()
	if _, err := authorize(ctx, q, p, orgID, tenancy.PermDeliveriesRead); err != nil {
		return nil, 0, err
	}
	f, limit, offset, err := parse()
	if err != nil {
		return nil, 0, err
	}
	return s.listDeliveries(ctx, q, orgID, f, limit, offset)
}

// listDeliveries validates the filter and reads the page. The caller has already authorized orgID.
func (s *Service) listDeliveries(ctx context.Context, q database.Querier, orgID string, f DeliveryFilter, limit, offset int) ([]delivery.DeliveryRecord, int, error) {
	if (f.DestinationID != "" && !IsUUID(f.DestinationID)) || (f.EventID != "" && !IsUUID(f.EventID)) {
		return nil, 0, Validation("destinationId and eventId must be UUIDs")
	}
	if f.Status != "" && !delivery.Status(f.Status).Valid() {
		return nil, 0, Validation("status must be PENDING, IN_FLIGHT, SUCCEEDED, FAILED or CANCELLED")
	}
	if !f.From.IsZero() && !f.To.IsZero() && !f.From.Before(f.To) {
		return nil, 0, Validation("from must be before to")
	}
	hf := delivery.HistoryFilter{
		DestinationID: f.DestinationID, EventID: f.EventID, Status: f.Status, From: f.From, To: f.To,
	}
	if f.FailureReason == "" {
		// Unfiltered requests are unchanged: same statements, no operations deadline.
		return delivery.ListDeliveries(ctx, q, orgID, hf, limit, offset)
	}
	if !delivery.ValidFailureFilter(f.FailureReason) {
		return nil, 0, Validation("failureReason must be DESTINATION_REJECTED, REDIRECT_REFUSED, EGRESS_BLOCKED, RETRIES_EXHAUSTED, INTERNAL_ERROR or UNRECORDED")
	}
	if f.Status != "" && f.Status != string(delivery.StatusFailed) {
		return nil, 0, Validation("failureReason only applies to FAILED deliveries; remove status or use status=FAILED")
	}
	// A failure reason can be rare in a large history, so a filtered list gets the operational-read deadline (ADR 32).
	// The exact total is kept, and so is the response shape.
	hf.FailureReason = f.FailureReason
	ctx, cancel := context.WithTimeout(ctx, s.operationsDeadline())
	defer cancel()
	items, total, err := delivery.ListDeliveries(ctx, q, orgID, hf, limit, offset)
	return items, total, operationsError(ctx, err)
}

// GetDelivery returns one delivery and its attempts. Attempts are read first and
// the delivery second: attempt_count only grows, so the result never lists more
// attempts than attemptCount reports, even while a worker is appending.
func (s *Service) GetDelivery(ctx context.Context, p Principal, orgID, deliveryID string) (DeliveryDetail, error) {
	q := s.mustQuerier()
	if _, err := authorize(ctx, q, p, orgID, tenancy.PermDeliveriesRead); err != nil {
		return DeliveryDetail{}, err
	}
	if !IsUUID(deliveryID) {
		return DeliveryDetail{}, ErrNotFound
	}
	attempts, err := delivery.AttemptsOf(ctx, q, orgID, deliveryID)
	if err != nil {
		return DeliveryDetail{}, err
	}
	d, err := delivery.DeliveryRecordByID(ctx, q, orgID, deliveryID)
	if errors.Is(err, delivery.ErrNotFound) {
		return DeliveryDetail{}, ErrNotFound
	}
	if err != nil {
		return DeliveryDetail{}, err
	}
	return DeliveryDetail{Delivery: d, Attempts: attempts}, nil
}
