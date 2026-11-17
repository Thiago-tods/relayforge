package database_test

import (
	"database/sql"
	"io/fs"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/Thiago-tods/relayforge/internal/database"
	"github.com/Thiago-tods/relayforge/internal/database/dbtest"
	"github.com/Thiago-tods/relayforge/internal/platform/config"
)

func newMigrator(t *testing.T, pool *database.Pool, fsys fs.FS) *database.Migrator {
	t.Helper()
	m, err := database.NewMigrator(mustSQLDB(t, pool), fsys)
	if err != nil {
		t.Fatalf("new migrator: %v", err)
	}
	return m
}

func mustSQLDB(t *testing.T, pool *database.Pool) *sql.DB {
	t.Helper()
	db, err := pool.SQLDB()
	if err != nil {
		t.Fatalf("SQLDB: %v", err)
	}
	return db
}

func openPool(t *testing.T) *database.Pool {
	t.Helper()
	return openPoolFor(t, dbtest.NewDatabase(t))
}

func openPoolFor(t *testing.T, cfg config.Database) *database.Pool {
	t.Helper()
	pool, err := database.Open(testContext(t), cfg, quietLogger())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestEmbeddedMigrationsApplyOnce(t *testing.T) {
	ctx := testContext(t)
	pool := openPool(t)
	m := newMigrator(t, pool, database.EmbeddedMigrations())

	before, err := m.Status(ctx)
	if err != nil {
		t.Fatalf("status before: %v", err)
	}
	if len(before) == 0 || before[0].Applied {
		t.Fatalf("expected pending embedded migrations, got %+v", before)
	}

	applied, err := m.Up(ctx)
	if err != nil {
		t.Fatalf("up: %v", err)
	}
	if len(applied) != len(before) {
		t.Fatalf("applied %d migrations, want %d", len(applied), len(before))
	}

	again, err := m.Up(ctx)
	if err != nil {
		t.Fatalf("second up: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("second run re-applied %d migrations", len(again))
	}

	after, err := m.Status(ctx)
	if err != nil {
		t.Fatalf("status after: %v", err)
	}
	for _, s := range after {
		if !s.Applied || s.AppliedAt.IsZero() {
			t.Fatalf("migration %s not reported as applied: %+v", s.Source, s)
		}
	}
	if v, err := m.Version(ctx); err != nil || v != after[len(after)-1].Version {
		t.Fatalf("version = %d (err %v), want %d", v, err, after[len(after)-1].Version)
	}

	var exists bool
	err = mustSQLDB(t, pool).QueryRowContext(ctx,
		"SELECT EXISTS (SELECT 1 FROM pg_proc WHERE proname = 'relayforge_set_updated_at')").Scan(&exists)
	if err != nil || !exists {
		t.Fatalf("expected relayforge_set_updated_at to exist: %v (exists=%v)", err, exists)
	}
}

func TestMigrationFailureIsReportedAndStopsRun(t *testing.T) {
	ctx := testContext(t)
	pool := openPool(t)
	fsys := fstest.MapFS{
		"00001_ok.sql": {Data: []byte("-- +goose Up\nCREATE TABLE t_ok (id int);\n-- +goose Down\nDROP TABLE t_ok;\n")},
		"00002_bad.sql": {Data: []byte("-- +goose Up\nCREATE TABLE t_bad (id int);\nSELECT * FROM table_that_does_not_exist;\n" +
			"-- +goose Down\nDROP TABLE t_bad;\n")},
		"00003_after.sql": {Data: []byte("-- +goose Up\nCREATE TABLE t_after (id int);\n-- +goose Down\nDROP TABLE t_after;\n")},
	}
	m := newMigrator(t, pool, fsys)

	applied, err := m.Up(ctx)
	if err == nil {
		t.Fatal("expected migration failure")
	}
	if len(applied) != 1 || applied[0].Version != 1 {
		t.Fatalf("expected only migration 1 applied before failure, got %+v", applied)
	}

	statuses, err := m.Status(ctx)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	wantApplied := map[int64]bool{1: true, 2: false, 3: false}
	for _, s := range statuses {
		if s.Applied != wantApplied[s.Version] {
			t.Fatalf("migration %d applied=%v, want %v", s.Version, s.Applied, wantApplied[s.Version])
		}
	}

	// The failed migration runs in a transaction, so its partial work is rolled back.
	var exists bool
	if err := mustSQLDB(t, pool).QueryRowContext(ctx,
		"SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 't_bad')").Scan(&exists); err != nil {
		t.Fatalf("query: %v", err)
	}
	if exists {
		t.Fatal("failed migration left partial changes behind")
	}
}

func TestConcurrentMigrationsApplyEachMigrationOnce(t *testing.T) {
	ctx := testContext(t)
	cfg := dbtest.NewDatabase(t)

	const runners = 4
	var wg sync.WaitGroup
	totals := make([]int, runners)
	errs := make([]error, runners)

	for i := range runners {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pool, err := database.Open(ctx, cfg, quietLogger())
			if err != nil {
				errs[i] = err
				return
			}
			defer pool.Close()
			db, err := pool.SQLDB()
			if err != nil {
				errs[i] = err
				return
			}
			m, err := database.NewMigrator(db, database.EmbeddedMigrations())
			if err != nil {
				errs[i] = err
				return
			}
			applied, err := m.Up(ctx)
			errs[i] = err
			totals[i] = len(applied)
		}()
	}
	wg.Wait()

	sum := 0
	for i := range runners {
		if errs[i] != nil {
			t.Fatalf("runner %d failed: %v", i, errs[i])
		}
		sum += totals[i]
	}

	pool := openPoolFor(t, cfg)
	statuses, err := newMigrator(t, pool, database.EmbeddedMigrations()).Status(ctx)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if sum != len(statuses) {
		t.Fatalf("migrations applied %d times in total, want exactly %d", sum, len(statuses))
	}
}
