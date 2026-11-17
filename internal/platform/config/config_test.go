package config

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

func lookupFrom(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func TestLoadFromDefaults(t *testing.T) {
	cfg, err := LoadFrom(lookupFrom(nil), ":8080")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.HTTPAddr != ":8080" || cfg.Env != "development" || cfg.LogFormat != "json" {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
	if cfg.ShutdownTimeout != 15*time.Second {
		t.Fatalf("unexpected shutdown timeout: %v", cfg.ShutdownTimeout)
	}
}

func TestLoadFromOverrides(t *testing.T) {
	cfg, err := LoadFrom(lookupFrom(map[string]string{
		"RELAYFORGE_ENV":              "PRODUCTION",
		"RELAYFORGE_LOG_LEVEL":        "debug",
		"RELAYFORGE_HTTP_ADDR":        "127.0.0.1:9000",
		"RELAYFORGE_SHUTDOWN_TIMEOUT": "30",
	}), ":8080")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Env != "production" || cfg.LogLevel != "debug" || cfg.HTTPAddr != "127.0.0.1:9000" || cfg.ShutdownTimeout != 30*time.Second {
		t.Fatalf("unexpected config: %+v", cfg)
	}
}

func TestLoadFromInvalid(t *testing.T) {
	cases := map[string]string{
		"RELAYFORGE_ENV":              "somewhere",
		"RELAYFORGE_LOG_LEVEL":        "verbose",
		"RELAYFORGE_LOG_FORMAT":       "xml",
		"RELAYFORGE_SHUTDOWN_TIMEOUT": "soon",
	}
	for key, val := range cases {
		if _, err := LoadFrom(lookupFrom(map[string]string{key: val}), ":8080"); err == nil {
			t.Errorf("%s=%q: expected error", key, val)
		}
	}
}

func TestSessionAndCookieDefaults(t *testing.T) {
	dev, err := LoadFrom(lookupFrom(nil), ":8080")
	if err != nil || dev.SessionLifetime != 12*time.Hour || dev.CookieSecure {
		t.Fatalf("development defaults: %+v, %v", dev, err)
	}
	prod, err := LoadFrom(lookupFrom(map[string]string{"RELAYFORGE_ENV": "production"}), ":8080")
	if err != nil || !prod.CookieSecure {
		t.Fatalf("production must default to secure cookies: %+v, %v", prod, err)
	}
}

func TestSessionAndCookieValidation(t *testing.T) {
	bad := []map[string]string{
		{"RELAYFORGE_ENV": "production", "RELAYFORGE_COOKIE_SECURE": "false"},
		{"RELAYFORGE_COOKIE_SECURE": "maybe"},
		{"RELAYFORGE_SESSION_LIFETIME": "0"},
		{"RELAYFORGE_SESSION_LIFETIME": "-5m"},
		{"RELAYFORGE_SESSION_LIFETIME": "9999h"},
		{"RELAYFORGE_SESSION_LIFETIME": "forever"},
	}
	for _, env := range bad {
		if _, err := LoadFrom(lookupFrom(env), ":8080"); err == nil {
			t.Errorf("%v: expected error", env)
		}
	}
	cfg, err := LoadFrom(lookupFrom(map[string]string{"RELAYFORGE_COOKIE_SECURE": "true", "RELAYFORGE_SESSION_LIFETIME": "2h"}), ":8080")
	if err != nil || !cfg.CookieSecure || cfg.SessionLifetime != 2*time.Hour {
		t.Fatalf("explicit values: %+v, %v", cfg, err)
	}
}

func TestEncryptionKeyConfiguration(t *testing.T) {
	good := base64.StdEncoding.EncodeToString(make([]byte, 32))
	cfg, err := LoadFrom(lookupFrom(map[string]string{"RELAYFORGE_ENCRYPTION_KEY": good}), ":8080")
	if err != nil || len(cfg.EncryptionKey) != 32 {
		t.Fatalf("valid key rejected: %v", err)
	}
	if cfg, err := LoadFrom(lookupFrom(nil), ":8080"); err != nil || cfg.EncryptionKey != nil {
		t.Fatalf("an unset key must stay unset, never be generated: %v %v", cfg.EncryptionKey, err)
	}
	for name, bad := range map[string]string{
		"not base64": "%%%%", "16 bytes": base64.StdEncoding.EncodeToString(make([]byte, 16)),
		"64 bytes": base64.StdEncoding.EncodeToString(make([]byte, 64)),
	} {
		_, err := LoadFrom(lookupFrom(map[string]string{"RELAYFORGE_ENCRYPTION_KEY": bad}), ":8080")
		if err == nil {
			t.Errorf("%s: expected error", name)
		} else if strings.Contains(err.Error(), bad) {
			t.Errorf("%s: error echoes the key", name)
		}
	}
}

func TestRateLimitConfiguration(t *testing.T) {
	cfg, err := LoadFrom(lookupFrom(nil), ":8080")
	if err != nil || cfg.IngestPerEndpointPerMinute != 600 || cfg.IngestPerIPPerMinute != 300 ||
		cfg.LoginFailuresPerAccount != 10 || cfg.LoginFailuresPerIP != 30 || cfg.MetricsEnabled {
		t.Fatalf("defaults: %+v %v", cfg, err)
	}
	cfg, err = LoadFrom(lookupFrom(map[string]string{"RELAYFORGE_LOGIN_FAILURES_PER_ACCOUNT": "3", "RELAYFORGE_METRICS_ENABLED": "true"}), ":8080")
	if err != nil || cfg.LoginFailuresPerAccount != 3 || !cfg.MetricsEnabled {
		t.Fatalf("overrides: %+v %v", cfg, err)
	}
	for _, env := range []map[string]string{
		{"RELAYFORGE_INGEST_RATE_PER_IP": "0"}, {"RELAYFORGE_INGEST_RATE_PER_ENDPOINT": "-1"},
		{"RELAYFORGE_LOGIN_FAILURES_PER_IP": "abc"}, {"RELAYFORGE_LOGIN_FAILURES_PER_ACCOUNT": "99999999"},
		{"RELAYFORGE_METRICS_ENABLED": "perhaps"},
	} {
		if _, err := LoadFrom(lookupFrom(env), ":8080"); err == nil {
			t.Errorf("%v: expected error", env)
		}
	}
}
