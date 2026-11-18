package delivery

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Thiago-tods/relayforge/internal/database"
)

// Repository errors.
var (
	// ErrNotFound means the delivery, its event or its destination does not exist in the organization.
	ErrNotFound = errors.New("delivery not found")
	// ErrStaleClaim means the claim token no longer matches: the delivery was recovered,
	// reclaimed, or finalized by someone else. The caller must roll back.
	ErrStaleClaim = errors.New("delivery claim is stale")
)

// Claimed is a delivery a worker has just claimed.
type Claimed struct {
	DeliveryID     string
	OrganizationID string
	EventID        string
	DestinationID  string
	// Attempt is the attempt number of this claim (the delivery's attempt_count).
	Attempt     int
	MaxAttempts int
	// ClaimToken is the fencing token. Only its holder may finalize the claim.
	ClaimToken     string
	LeaseExpiresAt time.Time
	// ClaimedAt is when the database recorded the claim.
	ClaimedAt time.Time
}

// ClaimDue claims up to limit due deliveries inside the caller's transaction and
// returns them. A delivery is due when it is PENDING, its next_attempt_at has
// passed, it has attempts left, and its destination is ACTIVE.
//
// FOR UPDATE SKIP LOCKED lets concurrent workers claim different deliveries
// without waiting for each other; a row locked by another transaction is skipped.
// Each claim increments attempt_count by one, sets the lease, and gets a fresh
// random UUID (gen_random_uuid, backed by PostgreSQL's strong random source) as its fencing
// token. The caller must commit before doing any network I/O and must not hold
// the transaction open while sending.
func ClaimDue(ctx context.Context, tx database.Querier, now time.Time, limit int, lease time.Duration, workerID string) ([]Claimed, error) {
	rows, err := tx.Query(ctx, `
		WITH due AS (
			SELECT d.id
			  FROM deliveries d
			  JOIN destinations x ON x.id = d.destination_id AND x.organization_id = d.organization_id
			 WHERE d.status = 'PENDING' AND d.next_attempt_at <= $1 AND d.attempt_count < d.max_attempts AND x.status = 'ACTIVE'
			 ORDER BY d.next_attempt_at, d.id
			 LIMIT $2
			   FOR UPDATE OF d SKIP LOCKED
		)
		UPDATE deliveries d
		   SET status = 'IN_FLIGHT', attempt_count = d.attempt_count + 1,
		       lease_expires_at = $1::timestamptz + make_interval(secs => $3),
		       claim_token = gen_random_uuid(), claimed_by = $4
		  FROM due
		 WHERE d.id = due.id
		RETURNING d.id, d.organization_id, d.event_id, d.destination_id, d.attempt_count, d.max_attempts,
		          d.claim_token::text, d.lease_expires_at, d.updated_at`,
		now, limit, lease.Seconds(), workerID)
	if err != nil {
		return nil, fmt.Errorf("claim deliveries: %w", err)
	}
	defer rows.Close()
	var out []Claimed
	for rows.Next() {
		var c Claimed
		if err := rows.Scan(&c.DeliveryID, &c.OrganizationID, &c.EventID, &c.DestinationID, &c.Attempt, &c.MaxAttempts,
			&c.ClaimToken, &c.LeaseExpiresAt, &c.ClaimedAt); err != nil {
			return nil, fmt.Errorf("scan claimed delivery: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SendData is everything needed to build one outgoing request.
type SendData struct {
	DeliveryID        string
	OrganizationID    string
	EventID           string
	DestinationID     string
	CreatedAt         time.Time
	Payload           []byte
	ContentType       string
	EventReceivedAt   time.Time
	URL               string
	DestinationStatus string
	Ciphertext        []byte
	KeyVersion        int
	SecretVersion     int64
}

// LoadSendData loads a delivery with its event and destination in one query.
// Every join repeats organization_id, so a delivery can only ever see its own
// organization's event and destination, whatever ids a message carried.
func LoadSendData(ctx context.Context, q database.Querier, orgID, deliveryID string) (SendData, error) {
	var s SendData
	var keyVersion int16
	err := q.QueryRow(ctx, `
		SELECT d.id, d.organization_id, d.event_id, d.destination_id, d.created_at,
		       e.payload, e.content_type, e.received_at,
		       x.url, x.status, x.signing_secret_ciphertext, x.encryption_key_version, x.secret_version
		  FROM deliveries d
		  JOIN webhook_events e ON e.id = d.event_id AND e.organization_id = d.organization_id
		  JOIN destinations x   ON x.id = d.destination_id AND x.organization_id = d.organization_id
		 WHERE d.id = $1 AND d.organization_id = $2`, deliveryID, orgID).
		Scan(&s.DeliveryID, &s.OrganizationID, &s.EventID, &s.DestinationID, &s.CreatedAt,
			&s.Payload, &s.ContentType, &s.EventReceivedAt,
			&s.URL, &s.DestinationStatus, &s.Ciphertext, &keyVersion, &s.SecretVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return SendData{}, ErrNotFound
	}
	if err != nil {
		return SendData{}, fmt.Errorf("load delivery: %w", err)
	}
	s.KeyVersion = int(keyVersion)
	return s, nil
}

// AttemptRecord is the result of one attempt, written to delivery_attempts.
type AttemptRecord struct {
	Number     int
	StartedAt  time.Time
	FinishedAt time.Time
	Outcome    Outcome
	// HTTPStatus is zero when there was no response.
	HTTPStatus int
	// ErrorClass is empty for HTTP responses.
	ErrorClass string
}

func nullInt(v int) any {
	if v == 0 {
		return nil
	}
	return v
}

func nullText(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Finish records an attempt and moves the delivery to its next state. The
// caller must run it inside one transaction and roll the transaction back when
// it returns an error.
//
// The order is mandatory (migration 00014): the attempt is inserted first,
// while the delivery is still IN_FLIGHT, and the delivery is updated second
// with the claim token as fence. If the fenced update affects no row, the claim is
// stale and ErrStaleClaim is returned; rolling back removes the attempt too.
func Finish(ctx context.Context, tx database.Querier, orgID, deliveryID, claimToken string, rec AttemptRecord, d Decision, now time.Time) error {
	finished := rec.FinishedAt
	if finished.Before(rec.StartedAt) {
		finished = rec.StartedAt
	}
	durationMS := int(finished.Sub(rec.StartedAt) / time.Millisecond)
	if _, err := tx.Exec(ctx, `
		INSERT INTO delivery_attempts (organization_id, delivery_id, attempt_number, started_at, finished_at, outcome, http_status, error_class, duration_ms)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		orgID, deliveryID, rec.Number, rec.StartedAt, finished, string(rec.Outcome), nullInt(rec.HTTPStatus), nullText(rec.ErrorClass), durationMS); err != nil {
		return fmt.Errorf("record delivery attempt: %w", err)
	}

	var tag interface{ RowsAffected() int64 }
	var err error
	switch d.Status {
	case StatusPending:
		tag, err = tx.Exec(ctx, `
			UPDATE deliveries
			   SET status = 'PENDING', lease_expires_at = NULL, claim_token = NULL, claimed_by = NULL,
			       next_attempt_at = $4, last_http_status = $5, last_error_class = $6
			 WHERE id = $1 AND organization_id = $2 AND status = 'IN_FLIGHT' AND claim_token = $3::uuid`,
			deliveryID, orgID, claimToken, d.NextAttemptAt, nullInt(rec.HTTPStatus), nullText(rec.ErrorClass))
	case StatusSucceeded, StatusFailed:
		tag, err = tx.Exec(ctx, `
			UPDATE deliveries
			   SET status = $4, lease_expires_at = NULL, claim_token = NULL, claimed_by = NULL,
			       last_http_status = $5, last_error_class = $6, completed_at = $7
			 WHERE id = $1 AND organization_id = $2 AND status = 'IN_FLIGHT' AND claim_token = $3::uuid`,
			deliveryID, orgID, claimToken, string(d.Status), nullInt(rec.HTTPStatus), nullText(rec.ErrorClass), now)
	default:
		return fmt.Errorf("finish delivery: unsupported next status %q", d.Status)
	}
	if err != nil {
		return fmt.Errorf("update delivery: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrStaleClaim
	}
	return nil
}

// Expired is a delivery whose lease ran out while it was IN_FLIGHT.
type Expired struct {
	DeliveryID     string
	OrganizationID string
	Attempt        int
	MaxAttempts    int
	ClaimToken     string
	ClaimedAt      time.Time
}

// ExpiredLeases locks and returns IN_FLIGHT deliveries whose lease expired at
// or before now, oldest first. FOR UPDATE SKIP LOCKED makes concurrent
// recovery processes take different rows, and skips a delivery a live worker is
// finalizing right now.
func ExpiredLeases(ctx context.Context, tx database.Querier, now time.Time, limit int) ([]Expired, error) {
	rows, err := tx.Query(ctx, `
		SELECT id, organization_id, attempt_count, max_attempts, claim_token::text, updated_at
		  FROM deliveries
		 WHERE status = 'IN_FLIGHT' AND lease_expires_at <= $1
		 ORDER BY lease_expires_at, id
		 LIMIT $2
		   FOR UPDATE SKIP LOCKED`, now, limit)
	if err != nil {
		return nil, fmt.Errorf("select expired leases: %w", err)
	}
	defer rows.Close()
	var out []Expired
	for rows.Next() {
		var e Expired
		if err := rows.Scan(&e.DeliveryID, &e.OrganizationID, &e.Attempt, &e.MaxAttempts, &e.ClaimToken, &e.ClaimedAt); err != nil {
			return nil, fmt.Errorf("scan expired lease: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// CancelForRevokedDestinations cancels PENDING deliveries whose destination was
// revoked. IN_FLIGHT deliveries are not touched: they finish or are recovered,
// and the worker checks the destination before sending.
func CancelForRevokedDestinations(ctx context.Context, q database.Querier, now time.Time) (int64, error) {
	tag, err := q.Exec(ctx, `
		UPDATE deliveries d SET status = 'CANCELLED', completed_at = $1
		  FROM destinations x
		 WHERE x.id = d.destination_id AND x.organization_id = d.organization_id
		   AND x.status = 'REVOKED' AND d.status = 'PENDING'`, now)
	if err != nil {
		return 0, fmt.Errorf("cancel deliveries of revoked destinations: %w", err)
	}
	return tag.RowsAffected(), nil
}

// Counts is a snapshot of the work queue, for gauges.
type Counts struct {
	Pending           int64
	DuePending        int64
	InFlight          int64
	ExpiredLeases     int64
	OutboxUnpublished int64
	// OldestDueAge is how long the longest-waiting due delivery has been due, zero
	// when nothing is due. It is the signal that work is stuck or the fleet is too small.
	OldestDueAge time.Duration
	// OldestOutboxAge is the age of the oldest unpublished outbox record, zero when none.
	OldestOutboxAge time.Duration
}

// QueueCounts returns the counts and ages used by the operational gauges.
//
// Every subquery names the status it counts, so PostgreSQL can use the partial
// indexes deliveries_due_idx (PENDING), deliveries_lease_idx (IN_FLIGHT) and
// outbox_unpublished_idx. Cost therefore follows the size of the live queue, not
// of the delivery history, which is never deleted and only grows.
func QueueCounts(ctx context.Context, q database.Querier, now time.Time) (Counts, error) {
	var c Counts
	var oldestDue, oldestOutbox *time.Time
	err := q.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM deliveries WHERE status = 'PENDING'),
		       (SELECT count(*) FROM deliveries WHERE status = 'PENDING' AND next_attempt_at <= $1),
		       (SELECT count(*) FROM deliveries WHERE status = 'IN_FLIGHT'),
		       (SELECT count(*) FROM deliveries WHERE status = 'IN_FLIGHT' AND lease_expires_at <= $1),
		       (SELECT count(*) FROM outbox WHERE published_at IS NULL),
		       (SELECT min(next_attempt_at) FROM deliveries WHERE status = 'PENDING' AND next_attempt_at <= $1),
		       (SELECT min(created_at) FROM outbox WHERE published_at IS NULL)`, now).
		Scan(&c.Pending, &c.DuePending, &c.InFlight, &c.ExpiredLeases, &c.OutboxUnpublished, &oldestDue, &oldestOutbox)
	if err != nil {
		return Counts{}, fmt.Errorf("count delivery queue: %w", err)
	}
	if oldestDue != nil {
		c.OldestDueAge = max(now.Sub(*oldestDue), 0)
	}
	if oldestOutbox != nil {
		c.OldestOutboxAge = max(now.Sub(*oldestOutbox), 0)
	}
	return c, nil
}
