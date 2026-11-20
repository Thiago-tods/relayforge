package delivery

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Thiago-tods/relayforge/internal/database"
)

// DeliveryRecord is the read model of a delivery for the history API. It carries
// operational metadata only: no payload, no destination URL, no secrets and no
// claim fields (token, worker id, lease), which are internal to the worker.
type DeliveryRecord struct {
	ID              string     `json:"id"`
	OrganizationID  string     `json:"organizationId"`
	EventID         string     `json:"eventId"`
	DestinationID   string     `json:"destinationId"`
	DestinationName string     `json:"destinationName"`
	Status          string     `json:"status"`
	AttemptCount    int        `json:"attemptCount"`
	MaxAttempts     int        `json:"maxAttempts"`
	NextAttemptAt   *time.Time `json:"nextAttemptAt,omitempty"`
	LastHTTPStatus  *int       `json:"lastHttpStatus,omitempty"`
	LastErrorClass  *string    `json:"lastErrorClass,omitempty"`
	// FailureReason says why the delivery ended as FAILED (one of FailureReason). It is
	// absent for deliveries that are not FAILED and for deliveries that failed before it was recorded.
	FailureReason *string    `json:"failureReason,omitempty"`
	CreatedAt     time.Time  `json:"createdAt"`
	UpdatedAt     time.Time  `json:"updatedAt"`
	CompletedAt   *time.Time `json:"completedAt,omitempty"`
}

// AttemptView is one recorded attempt. Only the status code and a short error
// class are stored, never a response body or a raw network error.
type AttemptView struct {
	AttemptNumber int       `json:"attemptNumber"`
	StartedAt     time.Time `json:"startedAt"`
	FinishedAt    time.Time `json:"finishedAt"`
	DurationMS    int       `json:"durationMs"`
	Outcome       string    `json:"outcome"`
	HTTPStatus    *int      `json:"httpStatus,omitempty"`
	ErrorClass    *string   `json:"errorClass,omitempty"`
	// TerminalReason is set only on the attempt that ended the delivery as FAILED.
	TerminalReason *string `json:"terminalReason,omitempty"`
}

// HistoryFilter narrows a delivery listing. Zero values mean "no filter". From is
// inclusive and To exclusive, both compared with the delivery's creation time.
type HistoryFilter struct {
	DestinationID string
	EventID       string
	Status        string
	// FailureReason is "", one of the five FailureReason values, or UnrecordedFailureReason (a FAILED
	// delivery with no recorded reason). A reason implies status FAILED. The caller rejects a conflicting Status.
	FailureReason string
	From, To      time.Time
}

const deliverySelect = `SELECT d.id::text, d.organization_id::text, d.event_id::text, d.destination_id::text, x.name, d.status,
	d.attempt_count, d.max_attempts,
	CASE WHEN d.status = 'PENDING' THEN d.next_attempt_at END,
	d.last_http_status::int, d.last_error_class, d.failure_reason, d.created_at, d.updated_at, d.completed_at
	FROM deliveries d
	JOIN destinations x ON x.id = d.destination_id AND x.organization_id = d.organization_id`

func scanDeliveryRecord(row pgx.Row) (DeliveryRecord, error) {
	var r DeliveryRecord
	err := row.Scan(&r.ID, &r.OrganizationID, &r.EventID, &r.DestinationID, &r.DestinationName, &r.Status, &r.AttemptCount,
		&r.MaxAttempts, &r.NextAttemptAt, &r.LastHTTPStatus, &r.LastErrorClass, &r.FailureReason, &r.CreatedAt, &r.UpdatedAt, &r.CompletedAt)
	return r, err
}

func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC()
}

// ListDeliveries returns a page of an organization's deliveries, newest first,
// with the destination name joined in one query (no N+1). The organization is
// part of every predicate, so another tenant's rows can never be returned.
func ListDeliveries(ctx context.Context, db database.Querier, orgID string, f HistoryFilter, limit, offset int) ([]DeliveryRecord, int, error) {
	where := ` WHERE d.organization_id = $1
		AND ($2::uuid IS NULL OR d.destination_id = $2::uuid)
		AND ($3::uuid IS NULL OR d.event_id = $3::uuid)
		AND ($4 = '' OR d.status = $4)
		AND ($5::timestamptz IS NULL OR d.created_at >= $5)
		AND ($6::timestamptz IS NULL OR d.created_at < $6)`
	// The reason clause is one of six constants (see failureReasonClause), appended only when the filter is set, so
	// a request without it runs exactly the statements it always ran.
	reason, err := failureReasonClause(f.FailureReason)
	if err != nil {
		return nil, 0, err
	}
	where += reason
	from, to := nullTime(f.From), nullTime(f.To)
	var total int
	if err := db.QueryRow(ctx, "SELECT count(*) FROM deliveries d"+where, orgID, nullText(f.DestinationID), nullText(f.EventID), f.Status, from, to).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count deliveries: %w", err)
	}
	rows, err := db.Query(ctx, deliverySelect+where+" ORDER BY d.created_at DESC, d.id DESC LIMIT $7 OFFSET $8",
		orgID, nullText(f.DestinationID), nullText(f.EventID), f.Status, from, to, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("list deliveries: %w", err)
	}
	defer rows.Close()
	out := []DeliveryRecord{}
	for rows.Next() {
		r, err := scanDeliveryRecord(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("scan delivery: %w", err)
		}
		out = append(out, r)
	}
	return out, total, rows.Err()
}

// DeliveryRecordByID loads one delivery of an organization.
func DeliveryRecordByID(ctx context.Context, db database.Querier, orgID, id string) (DeliveryRecord, error) {
	r, err := scanDeliveryRecord(db.QueryRow(ctx, deliverySelect+" WHERE d.id = $1 AND d.organization_id = $2", id, orgID))
	if errors.Is(err, pgx.ErrNoRows) {
		return DeliveryRecord{}, ErrNotFound
	}
	if err != nil {
		return DeliveryRecord{}, fmt.Errorf("select delivery: %w", err)
	}
	return r, nil
}

// AttemptsOf returns the attempts of a delivery in order. A delivery has at most
// max_attempts (50) of them, so the result is bounded.
func AttemptsOf(ctx context.Context, db database.Querier, orgID, deliveryID string) ([]AttemptView, error) {
	rows, err := db.Query(ctx, `SELECT attempt_number, started_at, finished_at, duration_ms, outcome, http_status::int, error_class, terminal_reason
		FROM delivery_attempts WHERE delivery_id = $1 AND organization_id = $2 ORDER BY attempt_number`, deliveryID, orgID)
	if err != nil {
		return nil, fmt.Errorf("list attempts: %w", err)
	}
	defer rows.Close()
	out := []AttemptView{}
	for rows.Next() {
		var a AttemptView
		if err := rows.Scan(&a.AttemptNumber, &a.StartedAt, &a.FinishedAt, &a.DurationMS, &a.Outcome, &a.HTTPStatus, &a.ErrorClass, &a.TerminalReason); err != nil {
			return nil, fmt.Errorf("scan attempt: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
