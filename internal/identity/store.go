package identity

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Thiago-tods/relayforge/internal/database"
)

// Account statuses.
const (
	StatusActive   = "ACTIVE"
	StatusDisabled = "DISABLED"
)

// Sentinel errors returned by the repositories.
var (
	ErrEmailTaken      = errors.New("email address is already registered")
	ErrUserNotFound    = errors.New("user not found")
	ErrSessionNotFound = errors.New("session not found")
)

// User is a global platform identity. PasswordHash is deliberately excluded
// from JSON so it cannot leak through an API response by accident.
type User struct {
	ID              string    `json:"id"`
	Email           string    `json:"email"`
	NormalizedEmail string    `json:"-"`
	PasswordHash    string    `json:"-"`
	DisplayName     string    `json:"displayName"`
	Status          string    `json:"status"`
	CreatedAt       time.Time `json:"createdAt"`
}

// Session is a server-side session. Token hashes are never serialized.
type Session struct {
	ID            string
	UserID        string
	CSRFTokenHash []byte
	ExpiresAt     time.Time
	CreatedAt     time.Time
	LastSeenAt    time.Time
}

const userColumns = "id, email, normalized_email, password_hash, display_name, status, created_at"

func scanUser(row pgx.Row) (User, error) {
	var u User
	err := row.Scan(&u.ID, &u.Email, &u.NormalizedEmail, &u.PasswordHash, &u.DisplayName, &u.Status, &u.CreatedAt)
	return u, err
}

// CreateUser inserts a user. It returns ErrEmailTaken when the normalized email exists.
func CreateUser(ctx context.Context, db database.Querier, email, normalizedEmail, passwordHash, displayName string) (User, error) {
	u, err := scanUser(db.QueryRow(ctx,
		`INSERT INTO users (email, normalized_email, password_hash, display_name)
		 VALUES ($1, $2, $3, $4) RETURNING `+userColumns,
		email, normalizedEmail, passwordHash, displayName))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "users_normalized_email_key" {
			return User{}, ErrEmailTaken
		}
		return User{}, fmt.Errorf("insert user: %w", err)
	}
	return u, nil
}

// UserByNormalizedEmail loads a user by canonical email.
func UserByNormalizedEmail(ctx context.Context, db database.Querier, normalizedEmail string) (User, error) {
	u, err := scanUser(db.QueryRow(ctx, "SELECT "+userColumns+" FROM users WHERE normalized_email = $1", normalizedEmail))
	return u, mapNoRows(err, ErrUserNotFound, "select user by email")
}

// UserByID loads a user by ID.
func UserByID(ctx context.Context, db database.Querier, id string) (User, error) {
	u, err := scanUser(db.QueryRow(ctx, "SELECT "+userColumns+" FROM users WHERE id = $1", id))
	return u, mapNoRows(err, ErrUserNotFound, "select user by id")
}

// CreateSession stores a new session. Only token hashes are persisted.
func CreateSession(ctx context.Context, db database.Querier, userID string, tokenHash, csrfHash []byte, expiresAt time.Time) (Session, error) {
	var s Session
	err := db.QueryRow(ctx,
		`INSERT INTO sessions (user_id, token_hash, csrf_token_hash, expires_at)
		 VALUES ($1, $2, $3, $4)
		 RETURNING id, user_id, csrf_token_hash, expires_at, created_at, last_seen_at`,
		userID, tokenHash, csrfHash, expiresAt).
		Scan(&s.ID, &s.UserID, &s.CSRFTokenHash, &s.ExpiresAt, &s.CreatedAt, &s.LastSeenAt)
	if err != nil {
		return Session{}, fmt.Errorf("insert session: %w", err)
	}
	return s, nil
}

// ActiveSession returns the session and its user for a token hash, provided the
// session is not revoked or expired at now and the user is ACTIVE. Any other
// state yields ErrSessionNotFound, so callers cannot tell the reasons apart.
func ActiveSession(ctx context.Context, db database.Querier, tokenHash []byte, now time.Time) (Session, User, error) {
	var s Session
	var u User
	err := db.QueryRow(ctx,
		`SELECT s.id, s.user_id, s.csrf_token_hash, s.expires_at, s.created_at, s.last_seen_at,
		        u.id, u.email, u.normalized_email, u.password_hash, u.display_name, u.status, u.created_at
		   FROM sessions s JOIN users u ON u.id = s.user_id
		  WHERE s.token_hash = $1 AND s.revoked_at IS NULL AND s.expires_at > $2 AND u.status = 'ACTIVE'`,
		tokenHash, now).
		Scan(&s.ID, &s.UserID, &s.CSRFTokenHash, &s.ExpiresAt, &s.CreatedAt, &s.LastSeenAt,
			&u.ID, &u.Email, &u.NormalizedEmail, &u.PasswordHash, &u.DisplayName, &u.Status, &u.CreatedAt)
	if err != nil {
		return Session{}, User{}, mapNoRows(err, ErrSessionNotFound, "select session")
	}
	return s, u, nil
}

// TouchSession updates last_seen_at when it is older than minAge.
func TouchSession(ctx context.Context, db database.Querier, sessionID string, now time.Time, minAge time.Duration) error {
	_, err := db.Exec(ctx,
		"UPDATE sessions SET last_seen_at = $2 WHERE id = $1 AND last_seen_at < $2 - make_interval(secs => $3)",
		sessionID, now, minAge.Seconds())
	if err != nil {
		return fmt.Errorf("touch session: %w", err)
	}
	return nil
}

// RotateCSRF replaces the stored CSRF token hash of an active session.
func RotateCSRF(ctx context.Context, db database.Querier, sessionID string, newHash []byte) error {
	tag, err := db.Exec(ctx,
		"UPDATE sessions SET csrf_token_hash = $2 WHERE id = $1 AND revoked_at IS NULL", sessionID, newHash)
	if err != nil {
		return fmt.Errorf("rotate csrf token: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrSessionNotFound
	}
	return nil
}

// RevokeSession marks a session revoked. It is idempotent: revoking an already
// revoked or unknown session is not an error.
func RevokeSession(ctx context.Context, db database.Querier, sessionID string, now time.Time) error {
	if _, err := db.Exec(ctx,
		"UPDATE sessions SET revoked_at = $2 WHERE id = $1 AND revoked_at IS NULL", sessionID, now); err != nil {
		return fmt.Errorf("revoke session: %w", err)
	}
	return nil
}

func mapNoRows(err error, sentinel error, op string) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, pgx.ErrNoRows):
		return sentinel
	default:
		return fmt.Errorf("%s: %w", op, err)
	}
}
