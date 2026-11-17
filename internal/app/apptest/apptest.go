// Package apptest builds a fully migrated service on a disposable PostgreSQL
// database for integration tests of internal/app and internal/httpapi.
package apptest

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/Thiago-tods/relayforge/internal/app"
	"github.com/Thiago-tods/relayforge/internal/database"
	"github.com/Thiago-tods/relayforge/internal/database/dbtest"
	"github.com/Thiago-tods/relayforge/internal/identity"
	"github.com/Thiago-tods/relayforge/internal/tenancy"
)

// Password is a policy-compliant password shared by test users.
const Password = "correct horse battery staple"

// FastArgon2Params keep tests quick; production uses identity.DefaultArgon2Params.
var FastArgon2Params = identity.Argon2Params{MemoryKiB: 64, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32}

// Clock is a controllable time source.
type Clock struct {
	mu  sync.Mutex
	now time.Time
}

// Now returns the current fake time.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance moves the fake time forward.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// Env is a migrated database with a service on top of it.
type Env struct {
	Pool  *database.Pool
	Svc   *app.Service
	Clock *Clock
}

// New creates an isolated database, applies all migrations and builds a service.
func New(t *testing.T) *Env {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	pool, err := database.Open(ctx, dbtest.NewDatabase(t), log)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)

	db, err := pool.SQLDB()
	if err != nil {
		t.Fatal(err)
	}
	migrator, err := database.NewMigrator(db, database.EmbeddedMigrations())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := migrator.Up(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	clock := &Clock{now: time.Now().UTC()}
	svc, err := app.New(pool, identity.NewHasher(FastArgon2Params), app.DefaultConfig(), clock.Now, log)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return &Env{Pool: pool, Svc: svc, Clock: clock}
}

// Principal authenticates the session token of an AuthResult.
func (e *Env) Principal(t *testing.T, res app.AuthResult) app.Principal {
	t.Helper()
	p, err := e.Svc.Authenticate(context.Background(), res.RawSessionToken)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	return p
}

// User is a registered test user with a live session.
type User struct {
	Auth      app.AuthResult
	Principal app.Principal
	Org       tenancy.Organization // the personal organization created at registration
	Email     string
}

// Register creates a user and returns it with an authenticated principal.
func (e *Env) Register(t *testing.T, email string) User {
	t.Helper()
	res, org, err := e.Svc.Register(context.Background(), app.RegisterInput{
		Email: email, Password: Password, DisplayName: "Test " + email,
	})
	if err != nil {
		t.Fatalf("register %s: %v", email, err)
	}
	return User{Auth: res, Principal: e.Principal(t, res), Org: org, Email: email}
}

// AddMember makes an existing user a member of orgID with the given role by
// running the real invite and accept flow. The inviter must be allowed to invite role.
func (e *Env) AddMember(t *testing.T, inviter app.Principal, orgID string, member User, role tenancy.Role) {
	t.Helper()
	ctx := context.Background()
	_, token, err := e.Svc.CreateInvitation(ctx, inviter, orgID, member.Email, role)
	if err != nil {
		t.Fatalf("invite %s: %v", member.Email, err)
	}
	if _, err := e.Svc.AcceptInvitation(ctx, member.Principal, token); err != nil {
		t.Fatalf("accept invitation for %s: %v", member.Email, err)
	}
}

// Exec runs a statement directly against the database, bypassing the service.
func (e *Env) Exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	q, err := e.Pool.Querier()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

// Count runs a SELECT count(*) style query.
func (e *Env) Count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	q, err := e.Pool.Querier()
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := q.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", sql, err)
	}
	return n
}
