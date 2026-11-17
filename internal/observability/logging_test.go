package observability

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestNewLoggerJSON(t *testing.T) {
	var buf bytes.Buffer
	NewLogger(&buf, "info", "json", "api").Info("hello", "k", "v")

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("output is not JSON: %v", err)
	}
	if rec["service"] != "api" || rec["msg"] != "hello" || rec["k"] != "v" {
		t.Fatalf("unexpected record: %v", rec)
	}
}

func TestNewLoggerLevelFilter(t *testing.T) {
	var buf bytes.Buffer
	NewLogger(&buf, "error", "text", "api").Info("hidden")
	if buf.Len() != 0 {
		t.Fatalf("expected info record to be filtered, got %q", buf.String())
	}
}
