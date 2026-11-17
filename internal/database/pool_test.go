package database_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
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
	if err := mustSQLDB(t, pool).QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil || one != 1 {
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

func TestSQLDBReturnsSameHandleAndFailsAfterClose(t *testing.T) {
	pool := openPool(t)

	first, err := pool.SQLDB()
	if err != nil {
		t.Fatalf("SQLDB: %v", err)
	}
	second, err := pool.SQLDB()
	if err != nil || first != second {
		t.Fatalf("SQLDB must return the same handle (err %v)", err)
	}

	pool.Close()

	if db, err := pool.SQLDB(); !errors.Is(err, database.ErrClosed) || db != nil {
		t.Fatalf("SQLDB after Close = (%v, %v), want (nil, ErrClosed)", db, err)
	}
	if err := first.PingContext(testContext(t)); err == nil {
		t.Fatal("handle obtained before Close must be unusable after Close")
	}
}

func TestSQLDBAfterCloseWithoutPriorHandle(t *testing.T) {
	pool := openPool(t)
	pool.Close()
	if db, err := pool.SQLDB(); !errors.Is(err, database.ErrClosed) || db != nil {
		t.Fatalf("SQLDB after Close = (%v, %v), want (nil, ErrClosed)", db, err)
	}
}

func TestSQLDBNilPool(t *testing.T) {
	var p *database.Pool
	if _, err := p.SQLDB(); !errors.Is(err, database.ErrNotInitialized) {
		t.Fatalf("got %v, want ErrNotInitialized", err)
	}
}

// TestConcurrentSQLDBAndClose is meaningful under the race detector: it
// hammers SQLDB while Close runs, and asserts that no usable handle is
// produced once Close has returned.
func TestConcurrentSQLDBAndClose(t *testing.T) {
	pool := openPool(t)

	const callers = 16
	start := make(chan struct{})
	var wg sync.WaitGroup
	var closed atomic.Bool

	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for range 200 {
				wasClosed := closed.Load()
				db, err := pool.SQLDB()
				switch {
				case err == nil && db == nil:
					t.Error("SQLDB returned nil handle without error")
				case err != nil && !errors.Is(err, database.ErrClosed):
					t.Errorf("unexpected error: %v", err)
				case wasClosed && err == nil:
					t.Error("SQLDB created a handle after Close returned")
				}
			}
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		pool.Close()
		closed.Store(true)
		pool.Close() // concurrent and repeated Close must be safe
	}()

	close(start)
	wg.Wait()

	if _, err := pool.SQLDB(); !errors.Is(err, database.ErrClosed) {
		t.Fatalf("SQLDB after Close = %v, want ErrClosed", err)
	}
}
