package ingest

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Thiago-tods/relayforge/internal/database"
)

// Endpoint statuses.
const (
	StatusActive  = "ACTIVE"
	StatusPaused  = "PAUSED"
	StatusRevoked = "REVOKED"
)

// Event statuses and outbox message types.
const (
	EventReceived        = "RECEIVED"
	MessageEventAccepted = "webhook_event.accepted"
)

// Repository errors.
var (
	ErrEndpointNotFound = errors.New("ingestion endpoint not found")
	ErrEventNotFound    = errors.New("webhook event not found")
)

// Endpoint is an ingestion endpoint. The ciphertext and key/secret versions are
// excluded from JSON so they can never appear in an API response.
type Endpoint struct {
	ID                      string     `json:"id"`
	OrganizationID          string     `json:"organizationId"`
	PublicID                string     `json:"publicId"`
	Name                    string     `json:"name"`
	Status                  string     `json:"status"`
	CreatedByUserID         string     `json:"createdByUserId"`
	CreatedAt               time.Time  `json:"createdAt"`
	UpdatedAt               time.Time  `json:"updatedAt"`
	RevokedAt               *time.Time `json:"revokedAt,omitempty"`
	SigningSecretCiphertext []byte     `json:"-"`
	EncryptionKeyVersion    int        `json:"-"`
	SecretVersion           int64      `json:"-"`
}

// SecretAAD is the additional authenticated data that binds a signing-secret
// ciphertext to its endpoint, so a ciphertext copied to another row will not open.
func SecretAAD(endpointID string) []byte {
	return []byte("relayforge:ingestion-endpoint:v1:" + endpointID)
}

const endpointColumns = `id, organization_id, public_id, name, status, created_by_user_id, created_at, updated_at,
	revoked_at, signing_secret_ciphertext, encryption_key_version, secret_version`

func scanEndpoint(row pgx.Row) (Endpoint, error) {
	var e Endpoint
	err := row.Scan(&e.ID, &e.OrganizationID, &e.PublicID, &e.Name, &e.Status, &e.CreatedByUserID, &e.CreatedAt,
		&e.UpdatedAt, &e.RevokedAt, &e.SigningSecretCiphertext, &e.EncryptionKeyVersion, &e.SecretVersion)
	return e, err
}

func endpointErr(err error, op string) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, pgx.ErrNoRows):
		return ErrEndpointNotFound
	default:
		return fmt.Errorf("%s: %w", op, err)
	}
}

// InsertEndpoint stores a new ACTIVE endpoint with an already-encrypted secret.
func InsertEndpoint(ctx context.Context, db database.Querier, e Endpoint) (Endpoint, error) {
	out, err := scanEndpoint(db.QueryRow(ctx,
		`INSERT INTO ingestion_endpoints
		   (id, organization_id, public_id, name, signing_secret_ciphertext, encryption_key_version, created_by_user_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING `+endpointColumns,
		e.ID, e.OrganizationID, e.PublicID, e.Name, e.SigningSecretCiphertext, e.EncryptionKeyVersion, e.CreatedByUserID))
	if err != nil {
		return Endpoint{}, fmt.Errorf("insert ingestion endpoint: %w", err)
	}
	return out, nil
}

// EndpointByPublicID loads an endpoint by its public identifier.
func EndpointByPublicID(ctx context.Context, db database.Querier, publicID string) (Endpoint, error) {
	e, err := scanEndpoint(db.QueryRow(ctx, "SELECT "+endpointColumns+" FROM ingestion_endpoints WHERE public_id = $1", publicID))
	return e, endpointErr(err, "select endpoint by public id")
}

// EndpointByID loads an endpoint of one organization. forUpdate takes a row lock.
func EndpointByID(ctx context.Context, db database.Querier, orgID, id string, forUpdate bool) (Endpoint, error) {
	q := "SELECT " + endpointColumns + " FROM ingestion_endpoints WHERE id = $1 AND organization_id = $2"
	if forUpdate {
		q += " FOR UPDATE"
	}
	e, err := scanEndpoint(db.QueryRow(ctx, q, id, orgID))
	return e, endpointErr(err, "select endpoint")
}

// ListEndpoints returns a page of an organization's endpoints, newest first.
func ListEndpoints(ctx context.Context, db database.Querier, orgID string, limit, offset int) ([]Endpoint, int, error) {
	var total int
	if err := db.QueryRow(ctx, "SELECT count(*) FROM ingestion_endpoints WHERE organization_id = $1", orgID).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count endpoints: %w", err)
	}
	rows, err := db.Query(ctx,
		"SELECT "+endpointColumns+` FROM ingestion_endpoints WHERE organization_id = $1
		  ORDER BY created_at DESC, id DESC LIMIT $2 OFFSET $3`, orgID, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("list endpoints: %w", err)
	}
	defer rows.Close()
	out := []Endpoint{}
	for rows.Next() {
		e, err := scanEndpoint(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("scan endpoint: %w", err)
		}
		out = append(out, e)
	}
	return out, total, rows.Err()
}

// RenameEndpoint changes the endpoint name.
func RenameEndpoint(ctx context.Context, db database.Querier, orgID, id, name string) (Endpoint, error) {
	e, err := scanEndpoint(db.QueryRow(ctx,
		"UPDATE ingestion_endpoints SET name = $3 WHERE id = $1 AND organization_id = $2 RETURNING "+endpointColumns, id, orgID, name))
	return e, endpointErr(err, "rename endpoint")
}

// SetEndpointStatus changes the status. Revocation also records revoked_at.
func SetEndpointStatus(ctx context.Context, db database.Querier, orgID, id, status string, now time.Time) (Endpoint, error) {
	var revokedAt *time.Time
	if status == StatusRevoked {
		revokedAt = &now
	}
	e, err := scanEndpoint(db.QueryRow(ctx,
		"UPDATE ingestion_endpoints SET status = $3, revoked_at = $4 WHERE id = $1 AND organization_id = $2 RETURNING "+endpointColumns,
		id, orgID, status, revokedAt))
	return e, endpointErr(err, "set endpoint status")
}

// ReplaceSecret atomically swaps the ciphertext and increments secret_version.
func ReplaceSecret(ctx context.Context, db database.Querier, orgID, id string, ciphertext []byte, keyVersion int) (Endpoint, error) {
	e, err := scanEndpoint(db.QueryRow(ctx,
		`UPDATE ingestion_endpoints
		    SET signing_secret_ciphertext = $3, encryption_key_version = $4, secret_version = secret_version + 1
		  WHERE id = $1 AND organization_id = $2 RETURNING `+endpointColumns, id, orgID, ciphertext, keyVersion))
	return e, endpointErr(err, "replace endpoint secret")
}

// LockEndpointShared takes a shared row lock on the endpoint and returns its
// current status and secret version. Ingestion calls it inside its transaction:
// rotation, pausing and revocation need an exclusive lock, so they wait for
// in-flight ingestion to finish, and ingestion started after them sees the new state.
func LockEndpointShared(ctx context.Context, db database.Querier, id string) (status string, secretVersion int64, err error) {
	err = db.QueryRow(ctx, "SELECT status, secret_version FROM ingestion_endpoints WHERE id = $1 FOR SHARE", id).Scan(&status, &secretVersion)
	return status, secretVersion, endpointErr(err, "lock endpoint")
}

// Event is a stored webhook event. Payload is nil in listings.
type Event struct {
	ID                  string    `json:"id"`
	OrganizationID      string    `json:"organizationId"`
	IngestionEndpointID string    `json:"ingestionEndpointId"`
	IdempotencyKey      string    `json:"idempotencyKey"`
	ContentType         string    `json:"contentType"`
	PayloadSHA256       []byte    `json:"-"`
	PayloadSize         int       `json:"payloadSize"`
	ReceivedAt          time.Time `json:"receivedAt"`
	Status              string    `json:"status"`
	Payload             []byte    `json:"-"`
}

// NewEvent describes an event to insert.
type NewEvent struct {
	ID, OrganizationID, EndpointID, IdempotencyKey, ContentType string
	Payload, PayloadSHA256                                      []byte
}

// InsertEvent inserts an event unless the (endpoint, idempotency key) pair
// exists. It reports whether a row was inserted. PostgreSQL arbitrates races:
// a concurrent insert of the same key makes this statement wait for the other
// transaction and then insert nothing.
func InsertEvent(ctx context.Context, db database.Querier, e NewEvent) (inserted bool, err error) {
	tag, err := db.Exec(ctx,
		`INSERT INTO webhook_events
		   (id, organization_id, ingestion_endpoint_id, idempotency_key, payload, payload_sha256, content_type)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 ON CONFLICT (ingestion_endpoint_id, idempotency_key) DO NOTHING`,
		e.ID, e.OrganizationID, e.EndpointID, e.IdempotencyKey, e.Payload, e.PayloadSHA256, e.ContentType)
	if err != nil {
		return false, fmt.Errorf("insert webhook event: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// EventByIdempotencyKey returns the id and payload hash of the stored event.
func EventByIdempotencyKey(ctx context.Context, db database.Querier, endpointID, key string) (id string, sha []byte, err error) {
	err = db.QueryRow(ctx,
		"SELECT id, payload_sha256 FROM webhook_events WHERE ingestion_endpoint_id = $1 AND idempotency_key = $2",
		endpointID, key).Scan(&id, &sha)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil, ErrEventNotFound
	}
	if err != nil {
		return "", nil, fmt.Errorf("select event by idempotency key: %w", err)
	}
	return id, sha, nil
}

// InsertOutbox adds the unpublished outbox record for an event.
func InsertOutbox(ctx context.Context, db database.Querier, orgID, eventID, messageType string, payload []byte) error {
	if _, err := db.Exec(ctx,
		"INSERT INTO outbox (organization_id, event_id, message_type, payload) VALUES ($1, $2, $3, $4)",
		orgID, eventID, messageType, payload); err != nil {
		return fmt.Errorf("insert outbox record: %w", err)
	}
	return nil
}

// EventFilter narrows an event listing. Zero values mean "no filter".
type EventFilter struct {
	EndpointID string
	Status     string
	From, To   time.Time // received_at >= From and < To
}

// ListEvents returns a page of an organization's event metadata, newest first.
func ListEvents(ctx context.Context, db database.Querier, orgID string, f EventFilter, limit, offset int) ([]Event, int, error) {
	where := `organization_id = $1
	  AND ($2::uuid IS NULL OR ingestion_endpoint_id = $2)
	  AND ($3::text IS NULL OR status = $3)
	  AND ($4::timestamptz IS NULL OR received_at >= $4)
	  AND ($5::timestamptz IS NULL OR received_at < $5)`
	args := []any{orgID, nullIfEmpty(f.EndpointID), nullIfEmpty(f.Status), nullTime(f.From), nullTime(f.To)}

	var total int
	if err := db.QueryRow(ctx, "SELECT count(*) FROM webhook_events WHERE "+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count events: %w", err)
	}
	rows, err := db.Query(ctx,
		`SELECT id, organization_id, ingestion_endpoint_id, idempotency_key, content_type, payload_sha256,
		        octet_length(payload), received_at, status
		   FROM webhook_events WHERE `+where+` ORDER BY received_at DESC, id DESC LIMIT $6 OFFSET $7`,
		append(args, limit, offset)...)
	if err != nil {
		return nil, 0, fmt.Errorf("list events: %w", err)
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.OrganizationID, &e.IngestionEndpointID, &e.IdempotencyKey, &e.ContentType,
			&e.PayloadSHA256, &e.PayloadSize, &e.ReceivedAt, &e.Status); err != nil {
			return nil, 0, fmt.Errorf("scan event: %w", err)
		}
		out = append(out, e)
	}
	return out, total, rows.Err()
}

// EventByID loads one event of an organization including its payload.
func EventByID(ctx context.Context, db database.Querier, orgID, id string) (Event, error) {
	var e Event
	err := db.QueryRow(ctx,
		`SELECT id, organization_id, ingestion_endpoint_id, idempotency_key, content_type, payload_sha256,
		        octet_length(payload), received_at, status, payload
		   FROM webhook_events WHERE id = $1 AND organization_id = $2`, id, orgID).
		Scan(&e.ID, &e.OrganizationID, &e.IngestionEndpointID, &e.IdempotencyKey, &e.ContentType,
			&e.PayloadSHA256, &e.PayloadSize, &e.ReceivedAt, &e.Status, &e.Payload)
	if errors.Is(err, pgx.ErrNoRows) {
		return Event{}, ErrEventNotFound
	}
	if err != nil {
		return Event{}, fmt.Errorf("select event: %w", err)
	}
	return e, nil
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}
