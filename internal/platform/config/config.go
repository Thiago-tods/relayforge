// Package config loads RelayForge process configuration from environment variables.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Thiago-tods/relayforge/internal/secretbox"
)

// Config holds settings shared by every RelayForge process.
type Config struct {
	// Env is the deployment environment: development, test or production.
	Env string
	// LogLevel is one of debug, info, warn or error.
	LogLevel string
	// LogFormat is either json or text.
	LogFormat string
	// HTTPAddr is the listen address of the process HTTP server.
	HTTPAddr string
	// ShutdownTimeout bounds graceful shutdown.
	ShutdownTimeout time.Duration
	// ReadHeaderTimeout protects the HTTP server against slow clients.
	ReadHeaderTimeout time.Duration
	// SessionLifetime is the absolute lifetime of an authenticated session.
	SessionLifetime time.Duration
	// CookieSecure sets the Secure attribute on the session cookie. It defaults
	// to true in production and must not be disabled there.
	CookieSecure bool
	// EncryptionKey is the 32-byte AES-256-GCM key for signing secrets at rest
	// (RELAYFORGE_ENCRYPTION_KEY, base64). Empty when unset; processes that manage
	// or authenticate ingestion endpoints refuse to start without it. It must
	// never be logged.
	EncryptionKey []byte
	// IngestPerEndpointPerMinute and IngestPerIPPerMinute limit ingestion requests.
	IngestPerEndpointPerMinute int
	IngestPerIPPerMinute       int
	// LoginFailuresPerAccount and LoginFailuresPerIP cap failed logins per 15 minutes.
	LoginFailuresPerAccount int
	LoginFailuresPerIP      int
	// MetricsEnabled mounts GET /metrics (Prometheus text format). Off by default
	// because the endpoint is unauthenticated; restrict it at the network level.
	MetricsEnabled bool
}

const (
	defaultEnv               = "development"
	defaultLogLevel          = "info"
	defaultLogFormat         = "json"
	defaultShutdownTimeout   = 15 * time.Second
	defaultReadHeaderTimeout = 5 * time.Second
	defaultSessionLifetime   = 12 * time.Hour
	maxSessionLifetime       = 30 * 24 * time.Hour
	maxRateLimit             = 1_000_000
)

// Load reads configuration from the process environment.
// defaultHTTPAddr is used when RELAYFORGE_HTTP_ADDR is unset.
func Load(defaultHTTPAddr string) (Config, error) {
	return LoadFrom(os.LookupEnv, defaultHTTPAddr)
}

// LoadFrom reads configuration using the supplied lookup function.
func LoadFrom(lookup func(string) (string, bool), defaultHTTPAddr string) (Config, error) {
	get := func(key, def string) string {
		if v, ok := lookup(key); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
		return def
	}

	cfg := Config{
		Env:       strings.ToLower(get("RELAYFORGE_ENV", defaultEnv)),
		LogLevel:  strings.ToLower(get("RELAYFORGE_LOG_LEVEL", defaultLogLevel)),
		LogFormat: strings.ToLower(get("RELAYFORGE_LOG_FORMAT", defaultLogFormat)),
		HTTPAddr:  get("RELAYFORGE_HTTP_ADDR", defaultHTTPAddr),
	}

	var err error
	if cfg.ShutdownTimeout, err = duration(get("RELAYFORGE_SHUTDOWN_TIMEOUT", ""), defaultShutdownTimeout); err != nil {
		return Config{}, fmt.Errorf("RELAYFORGE_SHUTDOWN_TIMEOUT: %w", err)
	}
	if cfg.ReadHeaderTimeout, err = duration(get("RELAYFORGE_READ_HEADER_TIMEOUT", ""), defaultReadHeaderTimeout); err != nil {
		return Config{}, fmt.Errorf("RELAYFORGE_READ_HEADER_TIMEOUT: %w", err)
	}

	if cfg.SessionLifetime, err = duration(get("RELAYFORGE_SESSION_LIFETIME", ""), defaultSessionLifetime); err != nil {
		return Config{}, fmt.Errorf("RELAYFORGE_SESSION_LIFETIME: %w", err)
	}
	cfg.CookieSecure = cfg.Env == "production"
	if raw := get("RELAYFORGE_COOKIE_SECURE", ""); raw != "" {
		if cfg.CookieSecure, err = strconv.ParseBool(raw); err != nil {
			return Config{}, fmt.Errorf("RELAYFORGE_COOKIE_SECURE: invalid boolean %q", raw)
		}
	}

	if raw := get("RELAYFORGE_ENCRYPTION_KEY", ""); raw != "" {
		if cfg.EncryptionKey, err = secretbox.ParseKey(raw); err != nil {
			return Config{}, fmt.Errorf("RELAYFORGE_ENCRYPTION_KEY: %w", err)
		}
	}
	for _, s := range []struct {
		key string
		dst *int
		def int
	}{
		{"RELAYFORGE_INGEST_RATE_PER_ENDPOINT", &cfg.IngestPerEndpointPerMinute, 600},
		{"RELAYFORGE_INGEST_RATE_PER_IP", &cfg.IngestPerIPPerMinute, 300},
		{"RELAYFORGE_LOGIN_FAILURES_PER_ACCOUNT", &cfg.LoginFailuresPerAccount, 10},
		{"RELAYFORGE_LOGIN_FAILURES_PER_IP", &cfg.LoginFailuresPerIP, 30},
	} {
		*s.dst = s.def
		if raw := get(s.key, ""); raw != "" {
			n, perr := strconv.Atoi(raw)
			if perr != nil {
				return Config{}, fmt.Errorf("%s: invalid integer %q", s.key, raw)
			}
			*s.dst = n
		}
	}
	if raw := get("RELAYFORGE_METRICS_ENABLED", ""); raw != "" {
		if cfg.MetricsEnabled, err = strconv.ParseBool(raw); err != nil {
			return Config{}, fmt.Errorf("RELAYFORGE_METRICS_ENABLED: invalid boolean %q", raw)
		}
	}

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate reports the first invalid setting.
func (c Config) Validate() error {
	switch c.Env {
	case "development", "test", "production":
	default:
		return fmt.Errorf("RELAYFORGE_ENV: unsupported value %q", c.Env)
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("RELAYFORGE_LOG_LEVEL: unsupported value %q", c.LogLevel)
	}
	switch c.LogFormat {
	case "json", "text":
	default:
		return fmt.Errorf("RELAYFORGE_LOG_FORMAT: unsupported value %q", c.LogFormat)
	}
	if c.HTTPAddr == "" {
		return fmt.Errorf("RELAYFORGE_HTTP_ADDR: must not be empty")
	}
	if c.ShutdownTimeout <= 0 || c.ReadHeaderTimeout <= 0 {
		return fmt.Errorf("timeouts must be positive")
	}
	if c.SessionLifetime <= 0 || c.SessionLifetime > maxSessionLifetime {
		return fmt.Errorf("RELAYFORGE_SESSION_LIFETIME: must be positive and at most %s", maxSessionLifetime)
	}
	for name, v := range map[string]int{
		"RELAYFORGE_INGEST_RATE_PER_ENDPOINT":   c.IngestPerEndpointPerMinute,
		"RELAYFORGE_INGEST_RATE_PER_IP":         c.IngestPerIPPerMinute,
		"RELAYFORGE_LOGIN_FAILURES_PER_ACCOUNT": c.LoginFailuresPerAccount,
		"RELAYFORGE_LOGIN_FAILURES_PER_IP":      c.LoginFailuresPerIP,
	} {
		if v < 1 || v > maxRateLimit {
			return fmt.Errorf("%s: must be between 1 and %d", name, maxRateLimit)
		}
	}
	if c.Env == "production" && !c.CookieSecure {
		return fmt.Errorf("RELAYFORGE_COOKIE_SECURE: must be true in production")
	}
	return nil
}

// duration accepts Go duration strings ("30s") or a plain number of seconds ("30").
func duration(raw string, def time.Duration) (time.Duration, error) {
	if raw == "" {
		return def, nil
	}
	if d, err := time.ParseDuration(raw); err == nil {
		return d, nil
	}
	secs, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q", raw)
	}
	return time.Duration(secs) * time.Second, nil
}
