// Package dbtest provides helpers for integration tests that need a real PostgreSQL server.
//
// Tests never touch an existing database. Each test creates a uniquely named
// database (rf_test_ followed by 12 hex digits) on the server given by
// RELAYFORGE_TEST_DATABASE_URL and drops it when the test finishes. The helper
// refuses to drop any database it did not create itself.
package dbtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
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
	// EnvAllowRemote permits a non-loopback test server (for example a shared,
	// disposable CI database host). Loopback servers never need it.
	EnvAllowRemote = "RELAYFORGE_TEST_ALLOW_REMOTE"

	namePrefix = "rf_test_"
	opTimeout  = 30 * time.Second
)

// generatedName matches exactly the names produced by NewDatabase.
var generatedName = regexp.MustCompile(`^` + namePrefix + `[0-9a-f]{12}$`)

// created records the databases this process created, so only those can be dropped.
var created sync.Map

// allowedQueryKeys are the only URL parameters accepted for the admin URL.
// Parameters such as host, dbname or service could silently redirect the
// connection to a different server or database.
var allowedQueryKeys = map[string]bool{
	"sslmode":          true,
	"connect_timeout":  true,
	"application_name": true,
}

// ServerURL returns the admin URL of the test server. When it is not configured
// the test is skipped locally, but fails in CI (CI=true) or when
// RELAYFORGE_REQUIRE_INTEGRATION=1, so integration tests are never skipped silently there.
// A configured URL that is unsuitable for a disposable server fails the test.
func ServerURL(t testing.TB) string {
	t.Helper()
	v := os.Getenv(EnvURL)
	if v == "" {
		if os.Getenv("CI") == "true" || os.Getenv(EnvRequire) == "1" {
			t.Fatalf("%s must be set: integration tests are required in this environment", EnvURL)
		}
		t.Skipf("%s is not set; skipping PostgreSQL integration test", EnvURL)
		return ""
	}
	if err := ValidateServerURL(v); err != nil {
		t.Fatalf("%s is not suitable for a disposable test server: %v", EnvURL, err)
	}
	return v
}

// ValidateServerURL checks that raw looks like the admin URL of a disposable
// test server. It rejects malformed URLs, unexpected parameters, names that look
// like production, and non-loopback hosts unless RELAYFORGE_TEST_ALLOW_REMOTE=1.
// Error messages never contain the URL or any credential.
func ValidateServerURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("URL cannot be parsed")
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return errors.New("scheme must be postgres or postgresql")
	}
	if u.User == nil || u.User.Username() == "" {
		return errors.New("a user name is required")
	}
	host := u.Hostname()
	if host == "" || strings.Contains(u.Host, ",") {
		return errors.New("exactly one host is required")
	}
	if port := u.Port(); port != "" && !regexp.MustCompile(`^[0-9]{1,5}$`).MatchString(port) {
		return errors.New("port must be numeric")
	}
	if !isLoopback(host) && os.Getenv(EnvAllowRemote) != "1" {
		return fmt.Errorf("host must be loopback (localhost, 127.0.0.0/8 or ::1); set %s=1 only for a disposable remote server", EnvAllowRemote)
	}
	name := strings.TrimPrefix(u.Path, "/")
	if strings.Contains(name, "/") {
		return errors.New("database name is invalid")
	}
	lower := strings.ToLower(name)
	for _, bad := range []string{"prod", "live"} {
		if strings.Contains(lower, bad) {
			return errors.New("database name looks like a production database")
		}
	}
	for key := range u.Query() {
		if !allowedQueryKeys[strings.ToLower(key)] {
			return fmt.Errorf("URL parameter %q is not allowed", key)
		}
	}
	return nil
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// NewDatabase creates an isolated, empty database and returns a pool
// configuration for it. The database is dropped automatically at test cleanup.
func NewDatabase(t testing.TB) config.Database {
	t.Helper()
	adminURL := ServerURL(t)

	name := namePrefix + randomSuffix(t)
	if !generatedName.MatchString(name) {
		t.Fatalf("generated database name %q is invalid", name)
	}

	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	admin, err := connect(ctx, adminURL)
	if err != nil {
		t.Fatalf("connect to test server: %v", err)
	}
	defer func() { _ = admin.Close(context.Background()) }()

	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatalf("create test database: %v", redactErr(err, adminURL))
	}
	created.Store(name, struct{}{})

	u, err := url.Parse(adminURL)
	if err != nil {
		t.Fatal("test server URL cannot be parsed")
	}
	u.Path = "/" + name
	cfg := config.Database{
		URL:               u.String(),
		MaxConns:          5,
		MinConns:          0,
		MaxConnLifetime:   time.Hour,
		MaxConnIdleTime:   time.Minute,
		HealthCheckPeriod: time.Minute,
		ConnectTimeout:    10 * time.Second,
	}

	t.Cleanup(func() { Drop(t, cfg) })
	return cfg
}

// Drop removes a database created by NewDatabase. It is called automatically at
// cleanup and may also be called earlier to simulate the database disappearing.
// It is a no-op if the database is already gone and fails the test, without
// dropping anything, if the name was not generated by this package.
func Drop(t testing.TB, cfg config.Database) {
	t.Helper()
	name, err := droppableName(cfg.URL)
	if err != nil {
		t.Errorf("refusing to drop database: %v", err)
		return
	}
	adminURL := ServerURL(t)

	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	conn, err := connect(ctx, adminURL)
	if err != nil {
		t.Errorf("connect to test server for drop: %v", err)
		return
	}
	defer func() { _ = conn.Close(context.Background()) }()
	if _, err := conn.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
		t.Errorf("drop test database %s: %v", name, redactErr(err, adminURL))
	}
}

// droppableName extracts the database name from rawURL and returns it only if
// it was generated by this process.
func droppableName(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", errors.New("URL cannot be parsed")
	}
	name := strings.TrimPrefix(u.Path, "/")
	if !generatedName.MatchString(name) {
		return "", fmt.Errorf("database name %q was not generated by dbtest", name)
	}
	if _, ok := created.Load(name); !ok {
		return "", fmt.Errorf("database %q was not created by this test process", name)
	}
	return name, nil
}

// connect opens one connection without ever surfacing the URL in errors.
func connect(ctx context.Context, rawURL string) (*pgx.Conn, error) {
	cfg, err := pgx.ParseConfig(rawURL)
	if err != nil {
		return nil, errors.New("test server URL cannot be parsed")
	}
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, errors.New(redactErr(err, rawURL))
	}
	return conn, nil
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
	msg = strings.ReplaceAll(msg, rawURL, "<redacted-url>")
	if u, perr := url.Parse(rawURL); perr == nil && u.User != nil {
		if pw, ok := u.User.Password(); ok && pw != "" {
			msg = strings.ReplaceAll(msg, pw, "<redacted>")
		}
	}
	return msg
}
