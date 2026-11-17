package delivery

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Thiago-tods/relayforge/internal/database"
)

// Subscription errors.
var (
	// ErrAlreadySubscribed means the endpoint/destination pair already has an ACTIVE subscription.
	ErrAlreadySubscribed = errors.New("delivery: an active subscription already exists")
	// ErrAlreadyRevoked means the subscription was revoked before.
	ErrAlreadyRevoked = errors.New("delivery: subscription already revoked")
)

// Subscribe starts a new subscription interval for an ingestion endpoint and a
// destination of the same organization, and returns its id. Each call creates a
// new record; a revoked record is never reactivated, so history stays intact.
//
// At most one ACTIVE subscription per pair is enforced by a partial unique index,
// so concurrent calls cannot create duplicates: exactly one succeeds and the
// others get ErrAlreadySubscribed. Tenant isolation is enforced by composite
// foreign keys: an endpoint or destination of another organization is rejected.
// The caller must have authorized the operation.
func Subscribe(ctx context.Context, tx database.Querier, orgID, endpointID, destinationID, userID string) (string, error) {
	var id string
	err := tx.QueryRow(ctx, `
		INSERT INTO subscriptions (organization_id, ingestion_endpoint_id, destination_id, created_by_user_id)
		VALUES ($1, $2, $3, $4)
		RETURNING id::text`, orgID, endpointID, destinationID, userID).Scan(&id)
	if err != nil {
		var pg *pgconn.PgError
		if errors.As(err, &pg) && pg.Code == "23505" && pg.ConstraintName == "subscriptions_one_active_per_pair_idx" {
			return "", ErrAlreadySubscribed
		}
		return "", fmt.Errorf("create subscription: %w", err)
	}
	return id, nil
}

// RevokeSubscription ends a subscription's interval at the database time. The
// record is kept: events accepted while it was active stay eligible for fan-out
// and reconciliation. It returns ErrNotFound if the subscription does not exist in
// the organization and ErrAlreadyRevoked if it was revoked before.
func RevokeSubscription(ctx context.Context, tx database.Querier, orgID, subscriptionID string) error {
	var status string
	err := tx.QueryRow(ctx,
		"SELECT status FROM subscriptions WHERE id = $1 AND organization_id = $2 FOR UPDATE", subscriptionID, orgID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("lock subscription: %w", err)
	}
	if status != "ACTIVE" {
		return ErrAlreadyRevoked
	}
	if _, err := tx.Exec(ctx,
		"UPDATE subscriptions SET status = 'REVOKED', revoked_at = now() WHERE id = $1 AND organization_id = $2", subscriptionID, orgID); err != nil {
		return fmt.Errorf("revoke subscription: %w", err)
	}
	return nil
}
