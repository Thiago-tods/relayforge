package app

import (
	"context"
	"errors"
	"time"

	"github.com/Thiago-tods/relayforge/internal/delivery"
	"github.com/Thiago-tods/relayforge/internal/tenancy"
)

// DeliveryFilter narrows a delivery listing.
type DeliveryFilter struct {
	DestinationID string
	EventID       string
	Status        string
	From, To      time.Time
}

// DeliveryDetail is a delivery with its recorded attempts.
type DeliveryDetail struct {
	Delivery delivery.DeliveryRecord
	Attempts []delivery.AttemptView
}

// ListDeliveries returns a page of the organization's deliveries. History is
// read-only: nothing here changes a delivery or an attempt.
func (s *Service) ListDeliveries(ctx context.Context, p Principal, orgID string, f DeliveryFilter, limit, offset int) ([]delivery.DeliveryRecord, int, error) {
	q := s.mustQuerier()
	if _, err := authorize(ctx, q, p, orgID, tenancy.PermDeliveriesRead); err != nil {
		return nil, 0, err
	}
	if (f.DestinationID != "" && !IsUUID(f.DestinationID)) || (f.EventID != "" && !IsUUID(f.EventID)) {
		return nil, 0, Validation("destinationId and eventId must be UUIDs")
	}
	if f.Status != "" && !delivery.Status(f.Status).Valid() {
		return nil, 0, Validation("status must be PENDING, IN_FLIGHT, SUCCEEDED, FAILED or CANCELLED")
	}
	if !f.From.IsZero() && !f.To.IsZero() && !f.From.Before(f.To) {
		return nil, 0, Validation("from must be before to")
	}
	return delivery.ListDeliveries(ctx, q, orgID, delivery.HistoryFilter{
		DestinationID: f.DestinationID, EventID: f.EventID, Status: f.Status, From: f.From, To: f.To,
	}, limit, offset)
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
