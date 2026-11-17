package database_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Thiago-tods/relayforge/internal/database"
	"github.com/Thiago-tods/relayforge/internal/database/dbtest"
	"github.com/Thiago-tods/relayforge/internal/platform/config"
	"github.com/Thiago-tods/relayforge/internal/platform/httpserver"
)

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestOpenRejectsInvalidConfig(t *testing.T) {
	_, err := database.Open(testContext(t), config.Database{}, quietLogger())
	if err == nil {
		t.Fatal("expected error for empty configuration")
	}
}

func TestOpenInvalidURLDoesNotLeakCredentials(t *testing.T) {
	cfg := config.Database{
		URL: "postgres://user:topsecret@127.0.0.1:1/db?sslmode=bogus", MaxConns: 2,
		MaxConnLifetime: time.Hour, MaxConnIdleTime: time.Minute,
		HealthCheckPeriod: time.Minute, ConnectTimeout: time.Second,
	}
	_, err := database.Open(testContext(t), cfg, quietLogger())
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "topsecret") {
		t.Fatalf("error leaks password: %v", err)
	}
}

func TestOpenConnectionFailureDoesNotLeakCredentials(t *testing.T) {
	// Port 1 is not served by PostgreSQL, so the connection is refused.
	cfg := config.Database{
		URL: "postgres://user:topsecret@127.0.0.1:1/db?sslmode=disable", MaxConns: 2,
		MaxConnLifetime: time.Hour, MaxConnIdleTime: time.Minute,
		HealthCheckPeriod: time.Minute, ConnectTimeout: 2 * time.Second,
	}
	pool, err := database.Open(testContext(t), cfg, quietLogger())
	if err == nil {
		pool.Close()
		t.Fatal("expected connection failure")
	}
	if strings.Contains(err.Error(), "topsecret") {
		t.Fatalf("error leaks password: %v", err)
	}
}

func TestOpenHonorsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cfg := config.Database{
		URL: "postgres://user:pw@127.0.0.1:1/db?sslmode=disable", MaxConns: 2,
		MaxConnLifetime: time.Hour, MaxConnIdleTime: time.Minute,
		HealthCheckPeriod: time.Minute, ConnectTimeout: 5 * time.Second,
	}
	if _, err := database.Open(ctx, cfg, quietLogger()); err == nil {
		t.Fatal("expected error with cancelled context")
	}
}

func TestNilPoolCheckFails(t *testing.T) {
	var p *database.Pool
	if err := p.Check(context.Background()); !errors.Is(err, database.ErrNotInitialized) {
		t.Fatalf("got %v, want ErrNotInitialized", err)
	}
	p.Close() // must not panic
}

func TestPoolLifecycle(t *testing.T) {
	cfg := dbtest.NewDatabase(t)
	cfg.MaxConns = 4
	ctx := testContext(t)

	pool, err := database.Open(ctx, cfg, quietLogger())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := pool.Check(ctx); err != nil {
		t.Fatalf("check: %v", err)
	}
	if got := pool.Stats().MaxConns; got != 4 {
		t.Fatalf("MaxConns = %d, want 4", got)
	}

	var one int
	if err := pool.SQLDB().QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil || one != 1 {
		t.Fatalf("query via database/sql: %v (got %d)", err, one)
	}

	pool.Close()
	pool.Close() // idempotent
	if err := pool.Check(ctx); err == nil {
		t.Fatal("check must fail after Close")
	}
}

func TestReadinessReflectsDatabaseState(t *testing.T) {
	cfg := dbtest.NewDatabase(t)
	ctx := testContext(t)
	pool, err := database.Open(ctx, cfg, quietLogger())
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	handler := httpserver.NewHandler(quietLogger(), map[string]httpserver.Check{"postgres": pool.Check})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("ready status = %d, want 200", rec.Code)
	}

	pool.Close()

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unready status = %d, want 503", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, cfg.URL) || strings.Contains(body, "pool") {
		t.Fatalf("readiness response leaks internal details: %s", body)
	}
}
