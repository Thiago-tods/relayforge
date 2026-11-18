package httpserver

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestHealthz(t *testing.T) {
	rec := httptest.NewRecorder()
	NewHandler(quietLogger(), nil).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestEveryResponseCarriesARequestIDAndMountedRoutesAreLoggedByPattern(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	inner := http.NewServeMux()
	inner.HandleFunc("GET /api/v1/things/{id}", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	h := NewHandlerWith(log, nil, Mount{Pattern: "/api/", Handler: inner})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/things/00000000-0000-0000-0000-000000000042", nil))
	if rec.Code != http.StatusTeapot || len(rec.Header().Get("X-Request-Id")) != 32 {
		t.Fatalf("status %d, request id %q", rec.Code, rec.Header().Get("X-Request-Id"))
	}
	if !strings.Contains(buf.String(), `route="GET /api/v1/things/{id}"`) || strings.Contains(buf.String(), "0000042") {
		t.Errorf("the access line must carry the route pattern only: %s", buf.String())
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Header().Get("X-Request-Id") == "" {
		t.Error("probes also carry a request id")
	}
}

func TestReadyz(t *testing.T) {
	ok := map[string]Check{"a": func(context.Context) error { return nil }}
	bad := map[string]Check{"a": func(context.Context) error { return errors.New("down") }}

	rec := httptest.NewRecorder()
	NewHandler(quietLogger(), ok).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("ready status = %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	NewHandler(quietLogger(), bad).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unready status = %d", rec.Code)
	}
}

func TestRecoverer(t *testing.T) {
	h := recoverer(quietLogger(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", rec.Code)
	}
}
