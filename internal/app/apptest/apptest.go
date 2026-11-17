// Package apptest builds a fully migrated service on a disposable PostgreSQL
// database for integration tests of internal/app and internal/httpapi.
package apptest

import (
	"context"
	"io"
	"log/slog"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/Thiago-tods/relayforge/internal/app"
	"github.com/Thiago-tods/relayforge/internal/database"
	"github.com/Thiago-tods/relayforge/internal/database/dbtest"
	"github.com/Thiago-tods/relayforge/internal/identity"
	"github.com/Thiago-tods/relayforge/internal/ingest"
	"github.com/Thiago-tods/relayforge/internal/observability"
	"github.com/Thiago-tods/relayforge/internal/secretbox"
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

// Env is a migrated database with services on top of it.
type Env struct {
	Pool   *database.Pool
	Svc    *app.Service
	Ingest *ingest.Service
	Box    *secretbox.Box
	Clock  *Clock
	// Metrics collects the ingestion metrics of this environment.
	Metrics *observability.Registry
	// IngestConfig is the configuration Ingest was built with.
	IngestConfig ingest.Config
}

// TestKey is a fixed 32-byte key for tests only. It protects nothing real.
var TestKey = []byte("0123456789abcdef0123456789abcdef")

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

	box, err := secretbox.New(TestKey, 1)
	if err != nil {
		t.Fatal(err)
	}
	clock := &Clock{now: time.Now().UTC()}
	appCfg := app.DefaultConfig()
	appCfg.Box = box
	svc, err := app.New(pool, identity.NewHasher(FastArgon2Params), appCfg, clock.Now, log)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	reg := observability.NewRegistry()
	ingestCfg := ingest.DefaultConfig()
	ingestCfg.PerEndpointPerMinute = 100000
	ingestCfg.PerIPPerMinute = 100000
	ing := ingest.NewService(pool, box, ingestCfg, clock.Now, log, ingest.NewMetrics(reg))
	return &Env{Pool: pool, Svc: svc, Ingest: ing, Box: box, Clock: clock, Metrics: reg, IngestConfig: ingestCfg}
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

// CreateEndpoint creates an ingestion endpoint in the user's personal organization.
func (e *Env) CreateEndpoint(t *testing.T, u User) app.EndpointWithSecret {
	t.Helper()
	return e.CreateEndpointIn(t, u, u.Org.ID)
}

// CreateEndpointIn creates an ingestion endpoint in orgID as the given user.
func (e *Env) CreateEndpointIn(t *testing.T, u User, orgID string) app.EndpointWithSecret {
	t.Helper()
	res, err := e.Svc.CreateEndpoint(context.Background(), u.Principal, orgID, "Test endpoint")
	if err != nil {
		t.Fatalf("create endpoint: %v", err)
	}
	return res
}

// Signed builds a correctly signed JSON request timestamped with the fake clock.
func (e *Env) Signed(secret string, body []byte, idempotencyKey string) ingest.Request {
	ts := strconv.FormatInt(e.Clock.Now().Unix(), 10)
	return ingest.Request{
		ContentType: "application/json", Timestamp: ts, Signature: ingest.Sign(secret, ts, body),
		IdempotencyKey: idempotencyKey, Body: body,
	}
}

// Send runs the two ingestion phases, as the HTTP handler does.
func (e *Env) Send(publicID string, req ingest.Request) (ingest.Result, error) {
	ctx := context.Background()
	ep, err := e.Ingest.Lookup(ctx, publicID, "")
	if err != nil {
		return ingest.Result{}, err
	}
	return e.Ingest.Accept(ctx, ep, req)
}
