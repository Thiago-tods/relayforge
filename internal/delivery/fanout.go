package delivery

import (
	"context"
	"fmt"
	"time"

	"github.com/Thiago-tods/relayforge/internal/database"
)

// activeAtAcceptance is the event-time eligibility rule shared by FanOut and
// Reconcile. A subscription is a lifecycle interval [created_at, revoked_at); it
// applies to an event when the event was accepted inside that interval. The
// current status is deliberately not consulted: revoking a subscription later
// must not remove an event that was accepted while it was active, and a
// subscription created later must not receive history. Timestamps are
// transaction-start times (now()) in UTC, so comparing them is consistent:
// created_at <= received_at (inclusive) and revoked_at > received_at (exclusive).
const activeAtAcceptance = `s.created_at <= e.received_at AND (s.revoked_at IS NULL OR s.revoked_at > e.received_at)`

// FanOut creates one PENDING delivery for every subscription that was active when
// the event was accepted, and returns how many it created. A destination that was
// revoked after acceptance still gets its delivery, which the worker then cancels,
// so the decision stays auditable; a destination revoked before acceptance gets none. It is idempotent: UNIQUE (event_id, destination_id) with ON
// CONFLICT DO NOTHING means a duplicate or concurrent call creates nothing new.
//
// The organization is matched in the query, so a message that names the wrong
// organization finds no event and creates nothing.
// Deliveries for paused destinations are created and simply wait: the claim
// query skips them until the destination is activated.
func FanOut(ctx context.Context, tx database.Querier, orgID, eventID string, now time.Time) (int64, error) {
	tag, err := tx.Exec(ctx, `
		INSERT INTO deliveries (organization_id, event_id, destination_id, next_attempt_at)
		SELECT e.organization_id, e.id, s.destination_id, $3
		  FROM webhook_events e
		  JOIN subscriptions s ON s.ingestion_endpoint_id = e.ingestion_endpoint_id AND s.organization_id = e.organization_id
		       AND `+activeAtAcceptance+`
		  JOIN destinations x ON x.id = s.destination_id AND x.organization_id = s.organization_id
		       AND (x.revoked_at IS NULL OR x.revoked_at > e.received_at)
		 WHERE e.id = $1 AND e.organization_id = $2
		ON CONFLICT (event_id, destination_id) DO NOTHING`, eventID, orgID, now)
	if err != nil {
		return 0, fmt.Errorf("fan out event: %w", err)
	}
	return tag.RowsAffected(), nil
}

// Reconcile is the safety net behind the message broker. PostgreSQL is the
// source of truth, so an accepted event whose fan-out message was lost, expired
// or exhausted its redeliveries must still be delivered. Reconcile finds events
// received between lookback ago and grace ago that lack a delivery for one of
// their active subscriptions and creates it, up to limit per call. The grace
// period leaves recent events to the normal path; the lookback bounds the scan.
// It is idempotent and safe to run from several processes at once.
func Reconcile(ctx context.Context, tx database.Querier, now time.Time, grace, lookback time.Duration, limit int) (int64, error) {
	tag, err := tx.Exec(ctx, `
		WITH missing AS (
			SELECT e.organization_id, e.id AS event_id, s.destination_id
			  FROM webhook_events e
			  JOIN subscriptions s ON s.ingestion_endpoint_id = e.ingestion_endpoint_id AND s.organization_id = e.organization_id
			       AND `+activeAtAcceptance+`
			  JOIN destinations x ON x.id = s.destination_id AND x.organization_id = s.organization_id
			       AND (x.revoked_at IS NULL OR x.revoked_at > e.received_at)
			 WHERE e.received_at < $1::timestamptz - make_interval(secs => $2)
			   AND e.received_at >= $1::timestamptz - make_interval(secs => $3)
			   AND NOT EXISTS (SELECT 1 FROM deliveries d WHERE d.event_id = e.id AND d.destination_id = s.destination_id)
			 ORDER BY e.received_at, e.id
			 LIMIT $4
		)
		INSERT INTO deliveries (organization_id, event_id, destination_id, next_attempt_at)
		SELECT organization_id, event_id, destination_id, $1 FROM missing
		ON CONFLICT (event_id, destination_id) DO NOTHING`, now, grace.Seconds(), lookback.Seconds(), limit)
	if err != nil {
		return 0, fmt.Errorf("reconcile deliveries: %w", err)
	}
	return tag.RowsAffected(), nil
}
