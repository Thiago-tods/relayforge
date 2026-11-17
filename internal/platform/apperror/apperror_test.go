package apperror

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestWriteAppError(t *testing.T) {
	rec := httptest.NewRecorder()
	Write(rec, New(http.StatusNotFound, "not_found", "missing"))
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), `"code":"not_found"`) {
		t.Fatalf("unexpected response: %d %s", rec.Code, rec.Body.String())
	}
}

func TestWriteHidesInternalErrors(t *testing.T) {
	rec := httptest.NewRecorder()
	Write(rec, errors.New("db password leaked"))
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "leaked") {
		t.Fatalf("unexpected response: %d %s", rec.Code, rec.Body.String())
	}
}

func TestWriteSetsRetryAfter(t *testing.T) {
	e := New(http.StatusTooManyRequests, "rate_limited", "slow down")
	e.RetryAfter = 90*time.Second + 400*time.Millisecond
	rec := httptest.NewRecorder()
	Write(rec, e)
	if got := rec.Header().Get("Retry-After"); got != "91" {
		t.Fatalf("Retry-After = %q, want 91 (rounded up)", got)
	}
	rec = httptest.NewRecorder()
	Write(rec, New(http.StatusNotFound, "not_found", "x"))
	if rec.Header().Get("Retry-After") != "" {
		t.Fatal("Retry-After must only be set when requested")
	}
}

func TestIsMatchesRecreatedErrorsByStatusAndCode(t *testing.T) {
	base := New(http.StatusBadRequest, "x", "m")
	wrapped := Wrap(http.StatusBadRequest, "x", "other message", errors.New("cause"))
	if !errors.Is(wrapped, base) || errors.Is(wrapped, New(http.StatusBadRequest, "y", "m")) {
		t.Fatal("Is must compare status and code only")
	}
}
