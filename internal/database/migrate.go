package database

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

//go:embed migrations/*.sql
var embeddedMigrations embed.FS

// EmbeddedMigrations returns the SQL migrations compiled into the binary.
func EmbeddedMigrations() fs.FS {
	sub, err := fs.Sub(embeddedMigrations, "migrations")
	if err != nil {
		// Unreachable: the "migrations" directory is guaranteed by the go:embed pattern.
		panic(fmt.Sprintf("embedded migrations directory missing: %v", err))
	}
	return sub
}

// Migrator applies and reports versioned SQL migrations.
//
// Concurrent runs are serialized with a PostgreSQL session-level advisory lock,
// so several processes can start at once and each migration is applied once.
type Migrator struct {
	provider *goose.Provider
}

// Applied describes a migration that was applied by Up.
type Applied struct {
	Version  int64
	Source   string
	Duration time.Duration
}

// Status describes one known migration and whether it has been applied.
type Status struct {
	Version   int64
	Source    string
	Applied   bool
	AppliedAt time.Time
}

// NewMigrator creates a Migrator for the migrations in fsys. Use
// EmbeddedMigrations for the real schema.
func NewMigrator(db *sql.DB, fsys fs.FS) (*Migrator, error) {
	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return nil, fmt.Errorf("create migration lock: %w", err)
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db, fsys, goose.WithSessionLocker(locker))
	if err != nil {
		return nil, fmt.Errorf("create migration provider: %w", err)
	}
	return &Migrator{provider: provider}, nil
}

// Up applies all pending migrations in order and returns those it applied.
// Migrations already recorded as applied are never re-run. On failure, the
// migrations applied before the failing one are returned along with the error.
func (m *Migrator) Up(ctx context.Context) ([]Applied, error) {
	results, err := m.provider.Up(ctx)
	if err != nil {
		var partial *goose.PartialError
		if errors.As(err, &partial) && partial.Failed != nil {
			return toApplied(partial.Applied), fmt.Errorf("migration %s failed: %w", partial.Failed.Source.Path, partial.Err)
		}
		return nil, fmt.Errorf("apply migrations: %w", err)
	}
	return toApplied(results), nil
}

func toApplied(results []*goose.MigrationResult) []Applied {
	applied := make([]Applied, 0, len(results))
	for _, r := range results {
		if r == nil || r.Error != nil {
			continue
		}
		applied = append(applied, Applied{
			Version:  r.Source.Version,
			Source:   r.Source.Path,
			Duration: r.Duration,
		})
	}
	return applied
}

// Status lists every known migration with its applied state, ordered by version.
func (m *Migrator) Status(ctx context.Context) ([]Status, error) {
	raw, err := m.provider.Status(ctx)
	if err != nil {
		return nil, fmt.Errorf("read migration status: %w", err)
	}
	out := make([]Status, 0, len(raw))
	for _, s := range raw {
		out = append(out, Status{
			Version:   s.Source.Version,
			Source:    s.Source.Path,
			Applied:   s.State == goose.StateApplied,
			AppliedAt: s.AppliedAt,
		})
	}
	return out, nil
}

// Version returns the highest applied migration version, or 0 if none is applied.
func (m *Migrator) Version(ctx context.Context) (int64, error) {
	v, err := m.provider.GetDBVersion(ctx)
	if err != nil {
		return 0, fmt.Errorf("read database version: %w", err)
	}
	return v, nil
}
