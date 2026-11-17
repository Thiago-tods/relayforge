package runtime

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Thiago-tods/relayforge/internal/database/dbtest"
)

const waitTimeout = 20 * time.Second

// harness runs the service in a goroutine and exposes its address and exit code.
type harness struct {
	addr   string
	cancel context.CancelFunc
	done   chan int
	stdout *bytes.Buffer
	stderr *bytes.Buffer
}

func envLookup(env map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := env[k]; return v, ok }
}

// start launches run on an OS-assigned port and waits until it listens or exits.
// h.addr is empty when the process exited before listening.
func start(t *testing.T, opts Options, env map[string]string) *harness {
	t.Helper()
	merged := map[string]string{"RELAYFORGE_HTTP_ADDR": "127.0.0.1:0", "RELAYFORGE_LOG_LEVEL": "error"}
	for k, v := range env {
		merged[k] = v
	}

	ctx, cancel := context.WithCancel(context.Background())
	h := &harness{cancel: cancel, done: make(chan int, 1), stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}}
	ready := make(chan string, 1)
	go func() {
		h.done <- run(ctx, "test-service", ":0", opts, envLookup(merged), h.stdout, h.stderr,
			func(addr string) { ready <- addr })
	}()

	select {
	case h.addr = <-ready:
	case code := <-h.done:
		h.done <- code // keep the exit code available to the test
	case <-time.After(waitTimeout):
		cancel()
		t.Fatal("timed out waiting for the service to start")
	}
	t.Cleanup(func() { h.stop(t) })
	return h
}

// stop cancels the service and returns its exit code. It is safe to call repeatedly.
func (h *harness) stop(t *testing.T) int {
	t.Helper()
	h.cancel()
	select {
	case code := <-h.done:
		h.done <- code
		return code
	case <-time.After(waitTimeout):
		t.Error("service did not shut down in time")
		return -1
	}
}

func (h *harness) exited() (int, bool) {
	select {
	case code := <-h.done:
		h.done <- code
		return code, true
	default:
		return 0, false
	}
}

func get(t *testing.T, addr, path string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// closedLocalAddr returns a loopback address that nothing listens on, so
// connections to it are refused without depending on a fixed port.
func closedLocalAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

func TestNoDatabaseRequiredServesHealthAndReady(t *testing.T) {
	h := start(t, Options{}, nil)
	if h.addr == "" {
		t.Fatalf("service did not start: %s", h.stderr)
	}
	if code, _ := get(t, h.addr, "/healthz"); code != http.StatusOK {
		t.Fatalf("healthz = %d", code)
	}
	if code, _ := get(t, h.addr, "/readyz"); code != http.StatusOK {
		t.Fatalf("readyz = %d", code)
	}
	if code := h.stop(t); code != 0 {
		t.Fatalf("exit code = %d, want 0 after graceful shutdown", code)
	}
}

func TestInvalidServerConfigurationExitsWithCode2(t *testing.T) {
	h := start(t, Options{}, map[string]string{"RELAYFORGE_LOG_LEVEL": "loud"})
	if code, ok := h.exited(); !ok || code != 2 {
		t.Fatalf("exit = (%d, %v), want (2, true)", code, ok)
	}
}

func TestMissingDatabaseURLExitsWithCode2(t *testing.T) {
	h := start(t, Options{RequireDatabase: true}, nil)
	code, ok := h.exited()
	if !ok || code != 2 {
		t.Fatalf("exit = (%d, %v), want (2, true)", code, ok)
	}
	if !strings.Contains(h.stderr.String(), "RELAYFORGE_DATABASE_URL") {
		t.Fatalf("stderr should name the missing variable: %q", h.stderr)
	}
}

func TestInvalidDatabaseConfigurationExitsWithCode2AndHidesSecrets(t *testing.T) {
	h := start(t, Options{RequireDatabase: true}, map[string]string{
		"RELAYFORGE_DATABASE_URL": "postgres://user:topsecret@127.0.0.1:5432/db",
		"RELAYFORGE_DB_MAX_CONNS": "0",
	})
	code, ok := h.exited()
	if !ok || code != 2 {
		t.Fatalf("exit = (%d, %v), want (2, true)", code, ok)
	}
	if strings.Contains(h.stderr.String(), "topsecret") || strings.Contains(h.stdout.String(), "topsecret") {
		t.Fatal("output leaks credentials")
	}
}

func TestDatabaseConnectionFailureExitsWithCode1AndHidesSecrets(t *testing.T) {
	h := start(t, Options{RequireDatabase: true}, map[string]string{
		"RELAYFORGE_DATABASE_URL":       "postgres://user:topsecret@" + closedLocalAddr(t) + "/db?sslmode=disable",
		"RELAYFORGE_DB_CONNECT_TIMEOUT": "3s",
	})
	code, ok := h.exited()
	if !ok || code != 1 {
		t.Fatalf("exit = (%d, %v), want (1, true)", code, ok)
	}
	if strings.Contains(h.stderr.String(), "topsecret") || strings.Contains(h.stdout.String(), "topsecret") {
		t.Fatal("output leaks credentials")
	}
}

func TestReadinessReflectsDatabaseAndLivenessDoesNot(t *testing.T) {
	dbCfg := dbtest.NewDatabase(t)
	h := start(t, Options{RequireDatabase: true}, map[string]string{"RELAYFORGE_DATABASE_URL": dbCfg.URL})
	if h.addr == "" {
		t.Fatalf("service did not start: %s", h.stderr)
	}

	code, body := get(t, h.addr, "/readyz")
	if code != http.StatusOK || !strings.Contains(body, `"postgres":"ok"`) {
		t.Fatalf("readyz with database = %d %s", code, body)
	}

	// Remove the database underneath the running process.
	dbtest.Drop(t, dbCfg)

	code, body = get(t, h.addr, "/readyz")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("readyz without database = %d, want 503", code)
	}
	if strings.Contains(body, dbCfg.URL) || strings.Contains(strings.ToLower(body), "password") {
		t.Fatalf("readyz leaks internals: %s", body)
	}

	if code, _ := get(t, h.addr, "/healthz"); code != http.StatusOK {
		t.Fatalf("healthz must stay 200 without database, got %d", code)
	}

	if code := h.stop(t); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
}

func TestServeAPIFailsClosedWithoutAValidEncryptionKey(t *testing.T) {
	dbCfg := dbtest.NewDatabase(t)
	for name, key := range map[string]string{"missing": "", "malformed": "%%%not-base64%%%", "wrong length": base64.StdEncoding.EncodeToString(make([]byte, 16))} {
		env := map[string]string{"RELAYFORGE_DATABASE_URL": dbCfg.URL}
		if key != "" {
			env["RELAYFORGE_ENCRYPTION_KEY"] = key
		}
		h := start(t, Options{RequireDatabase: true, ServeAPI: true}, env)
		code, ok := h.exited()
		if !ok || code != 2 {
			t.Errorf("%s key: exit = (%d, %v), want (2, true)", name, code, ok)
		}
		if key != "" && strings.Contains(h.stderr.String()+h.stdout.String(), key) {
			t.Errorf("%s key: output echoes the key", name)
		}
	}
	// The same applies in production.
	h := start(t, Options{RequireDatabase: true, ServeAPI: true}, map[string]string{
		"RELAYFORGE_DATABASE_URL": dbCfg.URL, "RELAYFORGE_ENV": "production",
	})
	if code, ok := h.exited(); !ok || code != 2 {
		t.Fatalf("production without key: exit = (%d, %v)", code, ok)
	}
}

func TestServeAPIMountsTheAPIAndOptionalMetrics(t *testing.T) {
	key := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	dbCfg := dbtest.NewDatabase(t)
	env := map[string]string{"RELAYFORGE_DATABASE_URL": dbCfg.URL, "RELAYFORGE_ENCRYPTION_KEY": key}

	h := start(t, Options{RequireDatabase: true, ServeAPI: true}, env)
	if h.addr == "" {
		t.Fatalf("service did not start: %s", h.stderr)
	}
	if code, _ := get(t, h.addr, "/api/v1/auth/me"); code != http.StatusUnauthorized {
		t.Fatalf("API must be mounted and require authentication, got %d", code)
	}
	if code, _ := get(t, h.addr, "/metrics"); code != http.StatusNotFound {
		t.Fatalf("metrics must be off by default, got %d", code)
	}
	if code, _ := get(t, h.addr, "/healthz"); code != http.StatusOK {
		t.Fatalf("healthz = %d", code)
	}
	if code := h.stop(t); code != 0 {
		t.Fatalf("exit code = %d, want 0 (pruner and pool must shut down cleanly)", code)
	}

	env["RELAYFORGE_METRICS_ENABLED"] = "true"
	h2 := start(t, Options{RequireDatabase: true, ServeAPI: true}, env)
	code, body := get(t, h2.addr, "/metrics")
	if code != http.StatusOK || !strings.Contains(body, "relayforge_ingest_requests_total") {
		t.Fatalf("metrics: %d %s", code, body)
	}
	if strings.Contains(body, key) {
		t.Fatal("metrics expose the key")
	}
}
