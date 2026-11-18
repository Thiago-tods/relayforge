package app

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/Thiago-tods/relayforge/internal/audit"
	"github.com/Thiago-tods/relayforge/internal/delivery"
	"github.com/Thiago-tods/relayforge/internal/ingest"
	"github.com/Thiago-tods/relayforge/internal/tenancy"
)

// SubscriptionFilter narrows a subscription listing.
type SubscriptionFilter = delivery.SubscriptionFilter

// CreateSubscription starts a new subscription interval linking an ingestion
// endpoint to a destination of the same organization (ADR 26). It never
// reactivates a revoked subscription: re-subscribing creates a new record.
//
// Rules, taken from the existing lifecycles rather than invented here: a REVOKED
// endpoint or destination cannot be subscribed (both are permanent); a PAUSED
// endpoint or destination can, because pausing is reversible and a paused
// destination keeps its pending deliveries.
func (s *Service) CreateSubscription(ctx context.Context, p Principal, orgID, endpointID, destinationID string) (delivery.Subscription, error) {
	if _, err := authorize(ctx, s.mustQuerier(), p, orgID, tenancy.PermSubscriptionsWrite); err != nil {
		return delivery.Subscription{}, err
	}
	if !IsUUID(endpointID) || !IsUUID(destinationID) {
		return delivery.Subscription{}, Validation("endpointId and destinationId must be UUIDs")
	}
	var out delivery.Subscription
	err := s.pool.InTx(ctx, func(tx pgx.Tx) error {
		// The organization lock serializes this with every other mutation of the
		// organization, including revocation of the endpoint or the destination.
		if _, err := lockAndAuthorize(ctx, tx, p, orgID, tenancy.PermSubscriptionsWrite); err != nil {
			return err
		}
		ep, err := ingest.EndpointByID(ctx, tx, orgID, endpointID, false)
		if errors.Is(err, ingest.ErrEndpointNotFound) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if ep.Status == ingest.StatusRevoked {
			return ErrEndpointRevoked
		}
		d, err := delivery.DestinationByID(ctx, tx, orgID, destinationID, false)
		if err != nil {
			return destinationErr(err)
		}
		if d.Status == delivery.DestinationRevoked {
			return ErrDestinationRevoked
		}
		id, err := delivery.Subscribe(ctx, tx, orgID, endpointID, destinationID, p.User.ID)
		if errors.Is(err, delivery.ErrAlreadySubscribed) {
			return ErrAlreadySubscribed
		}
		if err != nil {
			return err
		}
		if out, err = delivery.SubscriptionByID(ctx, tx, orgID, id); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Event{
			OrganizationID: orgID, ActorUserID: p.User.ID, Action: audit.SubscriptionCreated,
			ResourceType: "subscription", ResourceID: id,
			Metadata: map[string]any{"endpointId": endpointID, "destinationId": destinationID},
		})
	})
	return out, err
}

// ListSubscriptions returns a page of the organization's subscriptions.
func (s *Service) ListSubscriptions(ctx context.Context, p Principal, orgID string, f SubscriptionFilter, limit, offset int) ([]delivery.Subscription, int, error) {
	q := s.mustQuerier()
	if _, err := authorize(ctx, q, p, orgID, tenancy.PermSubscriptionsRead); err != nil {
		return nil, 0, err
	}
	if (f.EndpointID != "" && !IsUUID(f.EndpointID)) || (f.DestinationID != "" && !IsUUID(f.DestinationID)) {
		return nil, 0, Validation("endpointId and destinationId must be UUIDs")
	}
	if f.Status != "" && f.Status != "ACTIVE" && f.Status != "REVOKED" {
		return nil, 0, Validation("status must be ACTIVE or REVOKED")
	}
	return delivery.ListSubscriptions(ctx, q, orgID, f, limit, offset)
}

// GetSubscription returns one subscription of the organization.
func (s *Service) GetSubscription(ctx context.Context, p Principal, orgID, subscriptionID string) (delivery.Subscription, error) {
	q := s.mustQuerier()
	if _, err := authorize(ctx, q, p, orgID, tenancy.PermSubscriptionsRead); err != nil {
		return delivery.Subscription{}, err
	}
	if !IsUUID(subscriptionID) {
		return delivery.Subscription{}, ErrNotFound
	}
	sub, err := delivery.SubscriptionByID(ctx, q, orgID, subscriptionID)
	if errors.Is(err, delivery.ErrNotFound) {
		return delivery.Subscription{}, ErrNotFound
	}
	return sub, err
}

// RevokeSubscription ends a subscription interval. The record is kept, and events
// accepted while it was active stay eligible for delivery (ADR 26).
func (s *Service) RevokeSubscription(ctx context.Context, p Principal, orgID, subscriptionID string) (delivery.Subscription, error) {
	var out delivery.Subscription
	err := s.pool.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := lockAndAuthorize(ctx, tx, p, orgID, tenancy.PermSubscriptionsWrite); err != nil {
			return err
		}
		if !IsUUID(subscriptionID) {
			return ErrNotFound
		}
		switch err := delivery.RevokeSubscription(ctx, tx, orgID, subscriptionID); {
		case errors.Is(err, delivery.ErrNotFound):
			return ErrNotFound
		case errors.Is(err, delivery.ErrAlreadyRevoked):
			return ErrSubscriptionRevoked
		case err != nil:
			return err
		}
		var err error
		if out, err = delivery.SubscriptionByID(ctx, tx, orgID, subscriptionID); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Event{
			OrganizationID: orgID, ActorUserID: p.User.ID, Action: audit.SubscriptionRevoked,
			ResourceType: "subscription", ResourceID: subscriptionID,
		})
	})
	return out, err
}
