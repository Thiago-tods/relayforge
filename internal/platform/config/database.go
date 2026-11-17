package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Database holds PostgreSQL connection and pool settings.
type Database struct {
	// URL is the PostgreSQL connection string. It may contain credentials and
	// must never be logged; use RedactedURL for diagnostics.
	URL string
	// MaxConns is the maximum number of pooled connections.
	MaxConns int32
	// MinConns is the number of connections the pool keeps open.
	MinConns int32
	// MaxConnLifetime is the maximum age of a pooled connection.
	MaxConnLifetime time.Duration
	// MaxConnIdleTime is how long an idle connection may stay open.
	MaxConnIdleTime time.Duration
	// HealthCheckPeriod is how often the pool checks idle connections.
	HealthCheckPeriod time.Duration
	// ConnectTimeout bounds the initial connection and ping.
	ConnectTimeout time.Duration
}

// Defaults for optional database settings.
const (
	defaultDBMaxConns          = 10
	defaultDBMinConns          = 0
	defaultDBMaxConnLifetime   = time.Hour
	defaultDBMaxConnIdleTime   = 30 * time.Minute
	defaultDBHealthCheckPeriod = time.Minute
	defaultDBConnectTimeout    = 10 * time.Second
)

// LoadDatabase reads database configuration from the process environment.
func LoadDatabase() (Database, error) {
	return LoadDatabaseFrom(os.LookupEnv)
}

// LoadDatabaseFrom reads database configuration using the supplied lookup function.
// RELAYFORGE_DATABASE_URL has no default: credentials are never hardcoded.
func LoadDatabaseFrom(lookup func(string) (string, bool)) (Database, error) {
	get := func(key string) string {
		if v, ok := lookup(key); ok {
			return strings.TrimSpace(v)
		}
		return ""
	}

	db := Database{URL: get("RELAYFORGE_DATABASE_URL")}

	var err error
	if db.MaxConns, err = int32Setting(get("RELAYFORGE_DB_MAX_CONNS"), defaultDBMaxConns); err != nil {
		return Database{}, fmt.Errorf("RELAYFORGE_DB_MAX_CONNS: %w", err)
	}
	if db.MinConns, err = int32Setting(get("RELAYFORGE_DB_MIN_CONNS"), defaultDBMinConns); err != nil {
		return Database{}, fmt.Errorf("RELAYFORGE_DB_MIN_CONNS: %w", err)
	}
	if db.MaxConnLifetime, err = duration(get("RELAYFORGE_DB_MAX_CONN_LIFETIME"), defaultDBMaxConnLifetime); err != nil {
		return Database{}, fmt.Errorf("RELAYFORGE_DB_MAX_CONN_LIFETIME: %w", err)
	}
	if db.MaxConnIdleTime, err = duration(get("RELAYFORGE_DB_MAX_CONN_IDLE_TIME"), defaultDBMaxConnIdleTime); err != nil {
		return Database{}, fmt.Errorf("RELAYFORGE_DB_MAX_CONN_IDLE_TIME: %w", err)
	}
	if db.HealthCheckPeriod, err = duration(get("RELAYFORGE_DB_HEALTH_CHECK_PERIOD"), defaultDBHealthCheckPeriod); err != nil {
		return Database{}, fmt.Errorf("RELAYFORGE_DB_HEALTH_CHECK_PERIOD: %w", err)
	}
	if db.ConnectTimeout, err = duration(get("RELAYFORGE_DB_CONNECT_TIMEOUT"), defaultDBConnectTimeout); err != nil {
		return Database{}, fmt.Errorf("RELAYFORGE_DB_CONNECT_TIMEOUT: %w", err)
	}
	return db, nil
}

// Validate reports the first invalid setting. Error messages never contain the URL.
func (d Database) Validate() error {
	if d.URL == "" {
		return errors.New("RELAYFORGE_DATABASE_URL: must be set")
	}
	u, err := url.Parse(d.URL)
	if err != nil {
		return errors.New("RELAYFORGE_DATABASE_URL: not a valid URL")
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return errors.New("RELAYFORGE_DATABASE_URL: scheme must be postgres or postgresql")
	}
	if d.MaxConns < 1 {
		return errors.New("RELAYFORGE_DB_MAX_CONNS: must be at least 1")
	}
	if d.MinConns < 0 {
		return errors.New("RELAYFORGE_DB_MIN_CONNS: must not be negative")
	}
	if d.MinConns > d.MaxConns {
		return errors.New("RELAYFORGE_DB_MIN_CONNS: must not exceed RELAYFORGE_DB_MAX_CONNS")
	}
	for name, v := range map[string]time.Duration{
		"RELAYFORGE_DB_MAX_CONN_LIFETIME":   d.MaxConnLifetime,
		"RELAYFORGE_DB_MAX_CONN_IDLE_TIME":  d.MaxConnIdleTime,
		"RELAYFORGE_DB_HEALTH_CHECK_PERIOD": d.HealthCheckPeriod,
		"RELAYFORGE_DB_CONNECT_TIMEOUT":     d.ConnectTimeout,
	} {
		if v <= 0 {
			return fmt.Errorf("%s: must be positive", name)
		}
	}
	return nil
}

// RedactedURL returns the connection URL with the password removed, for diagnostics.
func (d Database) RedactedURL() string {
	u, err := url.Parse(d.URL)
	if err != nil {
		return "<invalid>"
	}
	return u.Redacted()
}

func int32Setting(raw string, def int32) (int32, error) {
	if raw == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(raw, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid integer %q", raw)
	}
	return int32(n), nil
}
