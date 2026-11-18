package observability

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func logTo(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func serve(t *testing.T, buf *bytes.Buffer, h http.HandlerFunc, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /items/{id}", h)
	mux.HandleFunc("GET /healthz", h)
	rec := httptest.NewRecorder()
	RequestLog(logTo(buf), mux).ServeHTTP(rec, req)
	return rec
}

func TestRequestLogGeneratesAnIDAndLogsTheRouteNotThePath(t *testing.T) {
	var buf bytes.Buffer
	var seen string
	rec := serve(t, &buf, func(w http.ResponseWriter, r *http.Request) {
		seen = RequestIDFrom(r.Context())
		_, _ = w.Write([]byte("hello"))
	}, httptest.NewRequest(http.MethodGet, "/items/secret-token-123?token=QUERYSECRET", nil))

	id := rec.Header().Get(RequestIDHeader)
	if len(id) != 32 || id != seen {
		t.Fatalf("response id %q, context id %q", id, seen)
	}
	out := buf.String()
	for _, banned := range []string{"secret-token-123", "QUERYSECRET", "token="} {
		if strings.Contains(out, banned) {
			t.Errorf("access log leaked %q: %s", banned, out)
		}
	}
	for _, want := range []string{"request_id=" + id, `route="GET /items/{id}"`, "status=200", "bytes=5", "method=GET"} {
		if !strings.Contains(out, want) {
			t.Errorf("access log lacks %q: %s", want, out)
		}
	}
}

func TestRequestLogAcceptsOnlySafeCallerIDs(t *testing.T) {
	for id, keep := range map[string]bool{
		"client-id-12345":              true,
		"short":                        false, // too short
		strings.Repeat("a", 65):        false, // too long
		"has space in it 123":          false,
		"line\nbreak-injection-attack": false,
		"quote\"inject=1 level=ERROR":  false,
	} {
		var buf bytes.Buffer
		req := httptest.NewRequest(http.MethodGet, "/items/1", nil)
		req.Header.Set(RequestIDHeader, id)
		rec := serve(t, &buf, func(http.ResponseWriter, *http.Request) {}, req)
		got := rec.Header().Get(RequestIDHeader)
		if keep && got != id {
			t.Errorf("a valid id %q was replaced by %q", id, got)
		}
		if !keep && (got == id || len(got) != 32) {
			t.Errorf("an unsafe id %q was echoed as %q", id, got)
		}
		if strings.Count(buf.String(), "\n") != 1 {
			t.Errorf("an id %q changed the number of log lines: %q", id, buf.String())
		}
	}
}

func TestRequestLogLevelsFollowTheStatusAndQuietRoutes(t *testing.T) {
	cases := []struct {
		path   string
		status int
		level  string
	}{
		{"/items/1", 200, "level=INFO"},
		{"/items/1", 404, "level=WARN"},
		{"/items/1", 503, "level=ERROR"},
		{"/healthz", 200, "level=DEBUG"},
	}
	for _, c := range cases {
		var buf bytes.Buffer
		serve(t, &buf, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(c.status) },
			httptest.NewRequest(http.MethodGet, c.path, nil))
		if !strings.Contains(buf.String(), c.level) || !strings.Contains(buf.String(), fmt.Sprintf("status=%d", c.status)) {
			t.Errorf("%s %d: %s", c.path, c.status, buf.String())
		}
	}
}

func TestRequestLogUnmatchedRouteHasABoundedLabel(t *testing.T) {
	var buf bytes.Buffer
	serve(t, &buf, func(http.ResponseWriter, *http.Request) {}, httptest.NewRequest(http.MethodGet, "/no/such/route/with-an-id-9f8e7d", nil))
	if !strings.Contains(buf.String(), "route=unmatched") || strings.Contains(buf.String(), "9f8e7d") {
		t.Errorf("unmatched requests must not log the raw path: %s", buf.String())
	}
}

func TestSafeErrorDropsDatabaseValues(t *testing.T) {
	pg := &pgconn.PgError{
		Code: "23505", ConstraintName: "users_email_key", Message: "duplicate key value violates unique constraint",
		Detail: "Key (email)=(victim@example.com) already exists.",
	}
	got := SafeError(fmt.Errorf("insert user: %w", pg))
	if strings.Contains(got, "victim@example.com") || !strings.Contains(got, "23505") || !strings.Contains(got, "users_email_key") {
		t.Errorf("SafeError = %q", got)
	}
	if SafeError(nil) != "" || SafeError(errors.New("plain failure")) != "plain failure" {
		t.Error("non-database errors keep their text")
	}
}
