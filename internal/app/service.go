package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/Thiago-tods/relayforge/internal/audit"
	"github.com/Thiago-tods/relayforge/internal/database"
	"github.com/Thiago-tods/relayforge/internal/identity"
	"github.com/Thiago-tods/relayforge/internal/tenancy"
)

// Config holds the service settings.
type Config struct {
	// SessionLifetime is the absolute session lifetime. There is no sliding expiration.
	SessionLifetime time.Duration
	// InvitationLifetime is how long an invitation stays valid.
	InvitationLifetime time.Duration
}

// DefaultConfig returns the documented defaults: 12 hour sessions, 7 day invitations.
func DefaultConfig() Config {
	return Config{SessionLifetime: 12 * time.Hour, InvitationLifetime: 7 * 24 * time.Hour}
}

const (
	maxDisplayNameLength = 100
	// maxLoginPasswordBytes bounds the work an unauthenticated caller can request.
	maxLoginPasswordBytes = 1024
	touchInterval         = 5 * time.Minute
	slugRetries           = 5
)

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// IsUUID reports whether s is a canonical lowercase UUID. Handlers use it to
// reject malformed identifiers before they reach the database.
func IsUUID(s string) bool { return uuidPattern.MatchString(strings.ToLower(s)) }

// Service implements the use cases. It is safe for concurrent use.
type Service struct {
	pool      *database.Pool
	hasher    *identity.Hasher
	cfg       Config
	now       func() time.Time
	log       *slog.Logger
	dummyHash string // verified against when the account does not exist, to equalize timing
}

// New creates a Service. now may be nil to use the wall clock.
func New(pool *database.Pool, hasher *identity.Hasher, cfg Config, now func() time.Time, log *slog.Logger) (*Service, error) {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	filler := make([]byte, 16)
	if _, err := rand.Read(filler); err != nil {
		return nil, fmt.Errorf("generate timing filler: %w", err)
	}
	dummy, err := hasher.Hash(context.Background(), hex.EncodeToString(filler))
	if err != nil {
		return nil, fmt.Errorf("prepare timing filler: %w", err)
	}
	return &Service{pool: pool, hasher: hasher, cfg: cfg, now: now, log: log, dummyHash: dummy}, nil
}

// Principal is the authenticated caller of a request.
type Principal struct {
	User    identity.User
	Session identity.Session
}

// AuthResult is returned by Register and Login. RawSessionToken and CSRFToken
// are delivered to the client once and never stored.
type AuthResult struct {
	User            identity.User
	ExpiresAt       time.Time
	RawSessionToken string
	CSRFToken       string
}

// RegisterInput is the validated-on-entry registration request.
type RegisterInput struct {
	Email            string
	Password         string
	DisplayName      string
	OrganizationName string // optional
	OrganizationSlug string // optional; derived from the name when empty
}

// Register creates a user, their first organization with an OWNER membership, a
// session and an audit event in a single transaction.
func (s *Service) Register(ctx context.Context, in RegisterInput) (AuthResult, tenancy.Organization, error) {
	normalized, err := identity.NormalizeEmail(in.Email)
	if err != nil {
		return AuthResult{}, tenancy.Organization{}, Validation("email address is invalid")
	}
	if err := identity.ValidatePassword(in.Password); err != nil {
		return AuthResult{}, tenancy.Organization{}, Validation(passwordMessage(err))
	}
	displayName := strings.TrimSpace(in.DisplayName)
	if displayName == "" || utf8.RuneCountInString(displayName) > maxDisplayNameLength || strings.ContainsFunc(displayName, isControl) {
		return AuthResult{}, tenancy.Organization{}, Validation("display name must be 1 to 100 printable characters")
	}
	orgName := strings.TrimSpace(in.OrganizationName)
	if orgName == "" {
		orgName = displayName + "'s organization"
	}
	orgName, err = tenancy.NormalizeName(orgName)
	if err != nil {
		return AuthResult{}, tenancy.Organization{}, Validation("organization name must be 1 to 100 printable characters")
	}
	explicitSlug := strings.TrimSpace(in.OrganizationSlug) != ""
	slug := tenancy.Slugify(orgName)
	if explicitSlug {
		if slug, err = tenancy.NormalizeSlug(in.OrganizationSlug); err != nil {
			return AuthResult{}, tenancy.Organization{}, Validation("organization slug must be 3 to 48 lowercase letters, digits or single hyphens")
		}
	}

	hash, err := s.hasher.Hash(ctx, in.Password)
	if err != nil {
		return AuthResult{}, tenancy.Organization{}, fmt.Errorf("hash password: %w", err)
	}

	var result AuthResult
	var org tenancy.Organization
	err = s.pool.InTx(ctx, func(tx pgx.Tx) error {
		user, err := identity.CreateUser(ctx, tx, strings.TrimSpace(in.Email), normalized, hash, displayName)
		if errors.Is(err, identity.ErrEmailTaken) {
			return ErrEmailTaken
		}
		if err != nil {
			return err
		}
		if org, err = createOrganization(ctx, tx, orgName, slug, !explicitSlug); err != nil {
			return err
		}
		if _, err = tenancy.AddMembership(ctx, tx, org.ID, user.ID, tenancy.RoleOwner); err != nil {
			return err
		}
		if result, err = s.createSession(ctx, tx, user); err != nil {
			return err
		}
		if err := audit.Record(ctx, tx, audit.Event{
			ActorUserID: user.ID, Action: audit.UserRegistered, ResourceType: "user", ResourceID: user.ID,
		}); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Event{
			OrganizationID: org.ID, ActorUserID: user.ID, Action: audit.OrganizationCreated,
			ResourceType: "organization", ResourceID: org.ID, Metadata: map[string]any{"slug": org.Slug, "via": "registration"},
		})
	})
	if err != nil {
		return AuthResult{}, tenancy.Organization{}, err
	}
	return result, org, nil
}

// Login verifies credentials and starts a session. Every failure returns
// ErrInvalidCredentials after comparable work, so neither the response nor its
// timing reveals whether the account exists.
func (s *Service) Login(ctx context.Context, email, password string) (AuthResult, error) {
	var user identity.User
	found := false
	if normalized, err := identity.NormalizeEmail(email); err == nil {
		switch u, err := identity.UserByNormalizedEmail(ctx, s.mustQuerier(), normalized); {
		case err == nil:
			user, found = u, true
		case !errors.Is(err, identity.ErrUserNotFound):
			return AuthResult{}, err
		}
	}

	hash := s.dummyHash
	if found {
		hash = user.PasswordHash
	}
	ok := false
	if len(password) <= maxLoginPasswordBytes {
		var err error
		if ok, err = s.hasher.Verify(ctx, password, hash); err != nil && found {
			s.log.Error("stored password hash is unreadable", "user_id", user.ID)
			ok = false
		} else if err != nil && !errors.Is(err, identity.ErrInvalidHash) {
			return AuthResult{}, err
		}
	}
	if !found || !ok || user.Status != identity.StatusActive {
		return AuthResult{}, ErrInvalidCredentials
	}

	var result AuthResult
	err := s.pool.InTx(ctx, func(tx pgx.Tx) error {
		var err error
		if result, err = s.createSession(ctx, tx, user); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Event{
			ActorUserID: user.ID, Action: audit.UserLoginSucceeded, ResourceType: "user", ResourceID: user.ID,
		})
	})
	if err != nil {
		return AuthResult{}, err
	}
	return result, nil
}

// Authenticate resolves a raw session token to a Principal. Missing, malformed,
// unknown, revoked and expired tokens and disabled users all yield ErrUnauthenticated.
func (s *Service) Authenticate(ctx context.Context, rawToken string) (Principal, error) {
	if !identity.WellFormedToken(rawToken) {
		return Principal{}, ErrUnauthenticated
	}
	now := s.now()
	q := s.mustQuerier()
	session, user, err := identity.ActiveSession(ctx, q, identity.HashToken(rawToken), now)
	if errors.Is(err, identity.ErrSessionNotFound) {
		return Principal{}, ErrUnauthenticated
	}
	if err != nil {
		return Principal{}, err
	}
	if err := identity.TouchSession(ctx, q, session.ID, now, touchInterval); err != nil {
		s.log.Warn("could not update session last_seen_at", "session_id", session.ID, "error", err)
	}
	return Principal{User: user, Session: session}, nil
}

// VerifyCSRF reports whether raw matches the CSRF token hash stored for the session.
func (p Principal) VerifyCSRF(raw string) bool {
	return identity.WellFormedToken(raw) && identity.TokenMatchesHash(raw, p.Session.CSRFTokenHash)
}

// Logout revokes the principal's current session and audits it. It is safe to repeat.
func (s *Service) Logout(ctx context.Context, p Principal) error {
	return s.pool.InTx(ctx, func(tx pgx.Tx) error {
		if err := identity.RevokeSession(ctx, tx, p.Session.ID, s.now()); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Event{
			ActorUserID: p.User.ID, Action: audit.UserLogout, ResourceType: "session", ResourceID: p.Session.ID,
		})
	})
}

// RotateCSRF issues a new CSRF token for the session, invalidating the previous one.
func (s *Service) RotateCSRF(ctx context.Context, p Principal) (string, error) {
	raw, hash, err := identity.NewToken()
	if err != nil {
		return "", err
	}
	if err := identity.RotateCSRF(ctx, s.mustQuerier(), p.Session.ID, hash); err != nil {
		if errors.Is(err, identity.ErrSessionNotFound) {
			return "", ErrUnauthenticated
		}
		return "", err
	}
	return raw, nil
}

// Memberships lists the organizations the principal belongs to.
func (s *Service) Memberships(ctx context.Context, p Principal) ([]tenancy.OrganizationWithRole, error) {
	return tenancy.OrganizationsForUser(ctx, s.mustQuerier(), p.User.ID)
}

func (s *Service) createSession(ctx context.Context, db database.Querier, user identity.User) (AuthResult, error) {
	rawToken, tokenHash, err := identity.NewToken()
	if err != nil {
		return AuthResult{}, err
	}
	rawCSRF, csrfHash, err := identity.NewToken()
	if err != nil {
		return AuthResult{}, err
	}
	session, err := identity.CreateSession(ctx, db, user.ID, tokenHash, csrfHash, s.now().Add(s.cfg.SessionLifetime))
	if err != nil {
		return AuthResult{}, err
	}
	return AuthResult{User: user, ExpiresAt: session.ExpiresAt, RawSessionToken: rawToken, CSRFToken: rawCSRF}, nil
}

// mustQuerier returns the pool as a Querier. A closed pool yields a Querier that
// fails every call, so callers surface a normal error instead of panicking.
func (s *Service) mustQuerier() database.Querier {
	q, err := s.pool.Querier()
	if err != nil {
		return failingQuerier{err: err}
	}
	return q
}

// createOrganization inserts an organization, retrying derived slugs on collision.
// Each attempt runs in a savepoint because a failed statement aborts a PostgreSQL transaction.
func createOrganization(ctx context.Context, tx pgx.Tx, name, slug string, derived bool) (tenancy.Organization, error) {
	candidate := slug
	for attempt := 0; attempt < slugRetries; attempt++ {
		sp, err := tx.Begin(ctx)
		if err != nil {
			return tenancy.Organization{}, fmt.Errorf("begin savepoint: %w", err)
		}
		org, err := tenancy.CreateOrganization(ctx, sp, name, candidate)
		if err == nil {
			if err := sp.Commit(ctx); err != nil {
				return tenancy.Organization{}, fmt.Errorf("release savepoint: %w", err)
			}
			return org, nil
		}
		if rbErr := sp.Rollback(ctx); rbErr != nil {
			return tenancy.Organization{}, fmt.Errorf("rollback savepoint: %w", rbErr)
		}
		if !errors.Is(err, tenancy.ErrSlugTaken) {
			return tenancy.Organization{}, err
		}
		if !derived {
			return tenancy.Organization{}, ErrSlugTaken
		}
		suffix := make([]byte, 3)
		if _, err := rand.Read(suffix); err != nil {
			return tenancy.Organization{}, fmt.Errorf("generate slug suffix: %w", err)
		}
		candidate = slug + "-" + hex.EncodeToString(suffix)
	}
	return tenancy.Organization{}, ErrSlugTaken
}

func passwordMessage(err error) string {
	msg := strings.TrimPrefix(err.Error(), identity.ErrPasswordPolicy.Error()+": ")
	return "password " + msg
}

func isControl(r rune) bool { return r < 0x20 || r == 0x7f }
