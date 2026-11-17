// Package dbtest provides helpers for integration tests that need a real PostgreSQL server.
//
// Tests never touch an existing database. Each test creates a uniquely named
// database on the server given by RELAYFORGE_TEST_DATABASE_URL and drops it
// when the test finishes.
package dbtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Thiago-tods/relayforge/internal/platform/config"
)

const (
	// EnvURL names the variable holding the admin connection URL of a disposable
	// PostgreSQL server. The role needs the CREATEDB privilege.
	EnvURL = "RELAYFORGE_TEST_DATABASE_URL"
	// EnvRequire forces integration tests to fail instead of skip when no server is configured.
	EnvRequire = "RELAYFORGE_REQUIRE_INTEGRATION"

	opTimeout = 30 * time.Second
)

// ServerURL returns the admin URL of the test server. When it is not configured
// the test is skipped locally, but fails in CI (CI=true) or when
// RELAYFORGE_REQUIRE_INTEGRATION=1, so integration tests are never skipped silently there.
func ServerURL(t testing.TB) string {
	t.Helper()
	if v := os.Getenv(EnvURL); v != "" {
		return v
	}
	if os.Getenv("CI") == "true" || os.Getenv(EnvRequire) == "1" {
		t.Fatalf("%s must be set: integration tests are required in this environment", EnvURL)
	}
	t.Skipf("%s is not set; skipping PostgreSQL integration test", EnvURL)
	return ""
}

// NewDatabase creates an isolated, empty database and returns a pool
// configuration for it. The database is dropped automatically at test cleanup.
func NewDatabase(t testing.TB) config.Database {
	t.Helper()
	adminURL := ServerURL(t)

	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()

	admin, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatalf("connect to test server: %v", redactErr(err, adminURL))
	}
	defer func() { _ = admin.Close(context.Background()) }()

	name := "rf_test_" + randomSuffix(t)
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatalf("create test database: %v", err)
	}

	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), opTimeout)
		defer ccancel()
		conn, err := pgx.Connect(cctx, adminURL)
		if err != nil {
			t.Errorf("cleanup: connect to test server: %v", redactErr(err, adminURL))
			return
		}
		defer func() { _ = conn.Close(context.Background()) }()
		if _, err := conn.Exec(cctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Errorf("cleanup: drop test database %s: %v", name, err)
		}
	})

	u, err := url.Parse(adminURL)
	if err != nil {
		t.Fatal("test server URL is not a valid URL")
	}
	u.Path = "/" + name

	return config.Database{
		URL:               u.String(),
		MaxConns:          5,
		MinConns:          0,
		MaxConnLifetime:   time.Hour,
		MaxConnIdleTime:   time.Minute,
		HealthCheckPeriod: time.Minute,
		ConnectTimeout:    10 * time.Second,
	}
}

func randomSuffix(t testing.TB) string {
	t.Helper()
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("generate random suffix: %v", err)
	}
	return hex.EncodeToString(b)
}

// redactErr keeps the test server password out of failure output.
func redactErr(err error, rawURL string) string {
	msg := err.Error()
	if u, perr := url.Parse(rawURL); perr == nil && u.User != nil {
		if pw, ok := u.User.Password(); ok && pw != "" {
			return strings.ReplaceAll(msg, pw, "<redacted>")
		}
	}
	return msg
}
