package delivery

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Thiago-tods/relayforge/internal/database"
)

// Subscription is one subscription lifecycle interval, with the destination's
// current name and status for display. ADR 26: the interval is
// [CreatedAt, RevokedAt); RevokedAt is nil while the subscription is active.
type Subscription struct {
	ID                string     `json:"id"`
	OrganizationID    string     `json:"organizationId"`
	EndpointID        string     `json:"endpointId"`
	DestinationID     string     `json:"destinationId"`
	DestinationName   string     `json:"destinationName"`
	DestinationStatus string     `json:"destinationStatus"`
	Status            string     `json:"status"`
	CreatedByUserID   string     `json:"createdByUserId"`
	CreatedAt         time.Time  `json:"createdAt"`
	RevokedAt         *time.Time `json:"revokedAt,omitempty"`
}

// SubscriptionFilter narrows a listing. Zero values mean "no filter".
type SubscriptionFilter struct {
	EndpointID    string
	DestinationID string
	Status        string
}

const subscriptionSelect = `SELECT s.id::text, s.organization_id::text, s.ingestion_endpoint_id::text, s.destination_id::text,
	d.name, d.status, s.status, s.created_by_user_id::text, s.created_at, s.revoked_at
	FROM subscriptions s
	JOIN destinations d ON d.id = s.destination_id AND d.organization_id = s.organization_id`

func scanSubscription(row pgx.Row) (Subscription, error) {
	var s Subscription
	err := row.Scan(&s.ID, &s.OrganizationID, &s.EndpointID, &s.DestinationID, &s.DestinationName, &s.DestinationStatus,
		&s.Status, &s.CreatedByUserID, &s.CreatedAt, &s.RevokedAt)
	return s, err
}

// SubscriptionByID loads one subscription of an organization.
func SubscriptionByID(ctx context.Context, db database.Querier, orgID, id string) (Subscription, error) {
	s, err := scanSubscription(db.QueryRow(ctx, subscriptionSelect+" WHERE s.id = $1 AND s.organization_id = $2", id, orgID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Subscription{}, ErrNotFound
	}
	if err != nil {
		return Subscription{}, fmt.Errorf("select subscription: %w", err)
	}
	return s, nil
}

// ListSubscriptions returns a page of an organization's subscriptions, newest first.
func ListSubscriptions(ctx context.Context, db database.Querier, orgID string, f SubscriptionFilter, limit, offset int) ([]Subscription, int, error) {
	const where = ` WHERE s.organization_id = $1
		AND ($2::uuid IS NULL OR s.ingestion_endpoint_id = $2::uuid)
		AND ($3::uuid IS NULL OR s.destination_id = $3::uuid)
		AND ($4 = '' OR s.status = $4)`
	var total int
	if err := db.QueryRow(ctx, "SELECT count(*) FROM subscriptions s"+where, orgID, nullText(f.EndpointID), nullText(f.DestinationID), f.Status).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count subscriptions: %w", err)
	}
	rows, err := db.Query(ctx, subscriptionSelect+where+" ORDER BY s.created_at DESC, s.id DESC LIMIT $5 OFFSET $6",
		orgID, nullText(f.EndpointID), nullText(f.DestinationID), f.Status, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("list subscriptions: %w", err)
	}
	defer rows.Close()
	out := []Subscription{}
	for rows.Next() {
		s, err := scanSubscription(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("scan subscription: %w", err)
		}
		out = append(out, s)
	}
	return out, total, rows.Err()
}
