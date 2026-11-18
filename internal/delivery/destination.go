package delivery

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Thiago-tods/relayforge/internal/database"
)

// Destination statuses.
const (
	DestinationActive  = "ACTIVE"
	DestinationPaused  = "PAUSED"
	DestinationRevoked = "REVOKED"
)

// Destination repository errors.
var (
	ErrDestinationNotFound  = errors.New("destination not found")
	ErrDestinationNameTaken = errors.New("destination name already in use")
)

// Destination is a delivery destination. The ciphertext and the key and secret
// versions are excluded from JSON so they can never appear in an API response.
// URL is the full normalized URL, which may contain a secret token in its query:
// never log it or show it to clients (use the egress display form instead).
type Destination struct {
	ID                      string     `json:"id"`
	OrganizationID          string     `json:"organizationId"`
	Name                    string     `json:"name"`
	URL                     string     `json:"-"`
	Status                  string     `json:"status"`
	CreatedByUserID         string     `json:"createdByUserId"`
	CreatedAt               time.Time  `json:"createdAt"`
	UpdatedAt               time.Time  `json:"updatedAt"`
	RevokedAt               *time.Time `json:"revokedAt,omitempty"`
	SigningSecretCiphertext []byte     `json:"-"`
	EncryptionKeyVersion    int        `json:"-"`
	SecretVersion           int64      `json:"-"`
}

const destinationColumns = `id::text, organization_id::text, name, url, status, created_by_user_id::text, created_at, updated_at,
	revoked_at, signing_secret_ciphertext, encryption_key_version, secret_version`

func scanDestination(row pgx.Row) (Destination, error) {
	var d Destination
	err := row.Scan(&d.ID, &d.OrganizationID, &d.Name, &d.URL, &d.Status, &d.CreatedByUserID, &d.CreatedAt,
		&d.UpdatedAt, &d.RevokedAt, &d.SigningSecretCiphertext, &d.EncryptionKeyVersion, &d.SecretVersion)
	return d, err
}

func destinationErr(err error, op string) error {
	var pg *pgconn.PgError
	switch {
	case err == nil:
		return nil
	case errors.Is(err, pgx.ErrNoRows):
		return ErrDestinationNotFound
	case errors.As(err, &pg) && pg.Code == "23505" && pg.ConstraintName == "destinations_active_name_key":
		return ErrDestinationNameTaken
	default:
		return fmt.Errorf("%s: %w", op, err)
	}
}

// InsertDestination stores a new ACTIVE destination with an already-encrypted secret.
func InsertDestination(ctx context.Context, db database.Querier, d Destination) (Destination, error) {
	out, err := scanDestination(db.QueryRow(ctx,
		`INSERT INTO destinations (id, organization_id, name, url, signing_secret_ciphertext, encryption_key_version, created_by_user_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING `+destinationColumns,
		d.ID, d.OrganizationID, d.Name, d.URL, d.SigningSecretCiphertext, d.EncryptionKeyVersion, d.CreatedByUserID))
	return out, destinationErr(err, "insert destination")
}

// DestinationByID loads a destination of one organization. forUpdate takes a row lock.
func DestinationByID(ctx context.Context, db database.Querier, orgID, id string, forUpdate bool) (Destination, error) {
	q := "SELECT " + destinationColumns + " FROM destinations WHERE id = $1 AND organization_id = $2"
	if forUpdate {
		q += " FOR UPDATE"
	}
	d, err := scanDestination(db.QueryRow(ctx, q, id, orgID))
	return d, destinationErr(err, "select destination")
}

// ListDestinations returns a page of an organization's destinations, newest first.
// status is an optional filter.
func ListDestinations(ctx context.Context, db database.Querier, orgID, status string, limit, offset int) ([]Destination, int, error) {
	var total int
	if err := db.QueryRow(ctx, "SELECT count(*) FROM destinations WHERE organization_id = $1 AND ($2 = '' OR status = $2)",
		orgID, status).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count destinations: %w", err)
	}
	rows, err := db.Query(ctx, "SELECT "+destinationColumns+` FROM destinations
		 WHERE organization_id = $1 AND ($2 = '' OR status = $2)
		 ORDER BY created_at DESC, id DESC LIMIT $3 OFFSET $4`, orgID, status, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("list destinations: %w", err)
	}
	defer rows.Close()
	out := []Destination{}
	for rows.Next() {
		d, err := scanDestination(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("scan destination: %w", err)
		}
		out = append(out, d)
	}
	return out, total, rows.Err()
}

// RenameDestination changes the name of a non-revoked destination.
func RenameDestination(ctx context.Context, db database.Querier, orgID, id, name string) (Destination, error) {
	d, err := scanDestination(db.QueryRow(ctx,
		"UPDATE destinations SET name = $3 WHERE id = $1 AND organization_id = $2 RETURNING "+destinationColumns, id, orgID, name))
	return d, destinationErr(err, "rename destination")
}

// SetDestinationURL changes the URL of a non-revoked destination. The caller
// must have validated and normalized it. Deliveries still pending use the new URL.
func SetDestinationURL(ctx context.Context, db database.Querier, orgID, id, normalizedURL string) (Destination, error) {
	d, err := scanDestination(db.QueryRow(ctx,
		"UPDATE destinations SET url = $3 WHERE id = $1 AND organization_id = $2 RETURNING "+destinationColumns, id, orgID, normalizedURL))
	return d, destinationErr(err, "update destination url")
}

// SetDestinationStatus moves a destination between ACTIVE, PAUSED and REVOKED.
// REVOKED sets revoked_at; the database refuses any later change.
func SetDestinationStatus(ctx context.Context, db database.Querier, orgID, id, status string, now time.Time) (Destination, error) {
	var revokedAt *time.Time
	if status == DestinationRevoked {
		revokedAt = &now
	}
	d, err := scanDestination(db.QueryRow(ctx,
		"UPDATE destinations SET status = $3, revoked_at = $4 WHERE id = $1 AND organization_id = $2 RETURNING "+destinationColumns,
		id, orgID, status, revokedAt))
	return d, destinationErr(err, "set destination status")
}

// DisplayURL returns the URL without its query string, which may hold a secret
// token, and whether a query was present. It is the only form to show or log.
func (d Destination) DisplayURL() (display string, hasQuery bool) {
	display, _, hasQuery = strings.Cut(d.URL, "?")
	return display, hasQuery
}

// CancelPendingForDestination cancels the PENDING deliveries of one destination of
// one organization and returns how many it cancelled. Cancelled deliveries stay as
// history. IN_FLIGHT deliveries are not touched: the worker re-checks the
// destination before it sends.
func CancelPendingForDestination(ctx context.Context, db database.Querier, orgID, destinationID string, now time.Time) (int64, error) {
	tag, err := db.Exec(ctx, `UPDATE deliveries SET status = 'CANCELLED', completed_at = $3
		 WHERE organization_id = $1 AND destination_id = $2 AND status = 'PENDING'`, orgID, destinationID, now)
	if err != nil {
		return 0, fmt.Errorf("cancel pending deliveries: %w", err)
	}
	return tag.RowsAffected(), nil
}

// ReplaceDestinationSecret stores a new encrypted secret and increments secret_version.
func ReplaceDestinationSecret(ctx context.Context, db database.Querier, orgID, id string, ciphertext []byte, keyVersion int) (Destination, error) {
	d, err := scanDestination(db.QueryRow(ctx,
		`UPDATE destinations SET signing_secret_ciphertext = $3, encryption_key_version = $4, secret_version = secret_version + 1
		  WHERE id = $1 AND organization_id = $2 RETURNING `+destinationColumns, id, orgID, ciphertext, keyVersion))
	return d, destinationErr(err, "replace destination secret")
}
