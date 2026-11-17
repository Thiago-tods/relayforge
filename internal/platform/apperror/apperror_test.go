package apperror

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
