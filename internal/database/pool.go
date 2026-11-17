// Package database owns PostgreSQL connectivity and schema migrations.
//
// A process that needs PostgreSQL opens exactly one Pool through Open, shares it
// with the components that need it, and closes it during graceful shutdown.
package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/Thiago-tods/relayforge/internal/platform/config"
)

// checkTimeout bounds a single readiness probe so a hung database cannot hang /readyz.
const checkTimeout = 2 * time.Second

// ErrNotInitialized is returned when a Pool that was never opened is used.
var ErrNotInitialized = errors.New("database pool is not initialized")

// Pool is a PostgreSQL connection pool with a single owner.
type Pool struct {
	pool *pgxpool.Pool
	log  *slog.Logger

	sqlOnce sync.Once
	sqlDB   *sql.DB

	closeOnce sync.Once
}

// Stats is a credential-free snapshot of pool usage.
type Stats struct {
	AcquiredConns int32
	IdleConns     int32
	TotalConns    int32
	MaxConns      int32
}

// Open validates cfg, creates the pool and verifies connectivity with a ping.
// The caller owns the returned Pool and must Close it. The connection URL and
// password never appear in returned errors or logs.
func Open(ctx context.Context, cfg config.Database, log *slog.Logger) (*Pool, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid database configuration: %w", err)
	}

	pcfg, err := pgxpool.ParseConfig(cfg.URL)
	if err != nil {
		// pgx parse errors can echo parts of the URL, so the cause is dropped.
		return nil, errors.New("invalid database configuration: RELAYFORGE_DATABASE_URL cannot be parsed")
	}
	pcfg.MaxConns = cfg.MaxConns
	pcfg.MinConns = cfg.MinConns
	pcfg.MaxConnLifetime = cfg.MaxConnLifetime
	pcfg.MaxConnIdleTime = cfg.MaxConnIdleTime
	pcfg.HealthCheckPeriod = cfg.HealthCheckPeriod

	ctx, cancel := context.WithTimeout(ctx, cfg.ConnectTimeout)
	defer cancel()

	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		return nil, redact(fmt.Errorf("create database pool: %w", err), cfg.URL)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, redact(fmt.Errorf("connect to database: %w", err), cfg.URL)
	}

	log.Info("database pool opened",
		"max_conns", cfg.MaxConns, "min_conns", cfg.MinConns, "target", cfg.RedactedURL())
	return &Pool{pool: pool, log: log}, nil
}

// Check verifies that the database answers within a short bounded timeout.
// It is suitable for use as a readiness check.
func (p *Pool) Check(ctx context.Context) error {
	if p == nil || p.pool == nil {
		return ErrNotInitialized
	}
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	if err := p.pool.Ping(ctx); err != nil {
		return fmt.Errorf("database ping failed: %w", err)
	}
	return nil
}

// Stats returns a snapshot of pool usage.
func (p *Pool) Stats() Stats {
	if p == nil || p.pool == nil {
		return Stats{}
	}
	s := p.pool.Stat()
	return Stats{
		AcquiredConns: s.AcquiredConns(),
		IdleConns:     s.IdleConns(),
		TotalConns:    s.TotalConns(),
		MaxConns:      s.MaxConns(),
	}
}

// SQLDB returns a database/sql handle backed by the same pool. It exists for
// libraries, such as the migration runner, that require database/sql. It is
// closed together with the Pool.
func (p *Pool) SQLDB() *sql.DB {
	p.sqlOnce.Do(func() { p.sqlDB = stdlib.OpenDBFromPool(p.pool) })
	return p.sqlDB
}

// Close releases the pool. It is safe to call more than once.
func (p *Pool) Close() {
	if p == nil {
		return
	}
	p.closeOnce.Do(func() {
		if p.sqlDB != nil {
			if err := p.sqlDB.Close(); err != nil {
				p.log.Warn("closing database/sql handle failed", "error", err)
			}
		}
		p.pool.Close()
		p.log.Info("database pool closed")
	})
}

// redactedError carries a credential-free message while preserving the cause
// for errors.Is and errors.As.
type redactedError struct {
	msg   string
	cause error
}

func (e *redactedError) Error() string { return e.msg }
func (e *redactedError) Unwrap() error { return e.cause }

// redact removes the connection URL and its password from err's message.
func redact(err error, rawURL string) error {
	msg := err.Error()
	msg = strings.ReplaceAll(msg, rawURL, "<redacted-url>")
	if u, parseErr := url.Parse(rawURL); parseErr == nil && u.User != nil {
		if pw, ok := u.User.Password(); ok && pw != "" {
			msg = strings.ReplaceAll(msg, pw, "<redacted>")
		}
	}
	return &redactedError{msg: msg, cause: err}
}
