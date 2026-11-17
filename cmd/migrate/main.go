// Command migrate applies and reports RelayForge database migrations.
//
// Usage:
//
//	migrate up       apply all pending migrations
//	migrate status   list migrations and whether they are applied
//
// The database is selected with RELAYFORGE_DATABASE_URL.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/Thiago-tods/relayforge/internal/database"
	"github.com/Thiago-tods/relayforge/internal/observability"
	"github.com/Thiago-tods/relayforge/internal/platform/config"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.LookupEnv, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// run executes the command and returns the process exit code: 0 on success,
// 2 for usage or configuration errors, 1 for runtime failures.
func run(ctx context.Context, args []string, lookup func(string) (string, bool), stdout, stderr io.Writer) int {
	if len(args) != 1 || (args[0] != "up" && args[0] != "status") {
		fmt.Fprintln(stderr, "usage: migrate <up|status>")
		return 2
	}

	dbCfg, err := config.LoadDatabaseFrom(lookup)
	if err == nil {
		err = dbCfg.Validate()
	}
	if err != nil {
		fmt.Fprintf(stderr, "migrate: invalid database configuration: %v\n", err)
		return 2
	}

	log := observability.NewLogger(stderr, "warn", "text", "migrate")
	pool, err := database.Open(ctx, dbCfg, log)
	if err != nil {
		fmt.Fprintf(stderr, "migrate: %v\n", err)
		return 1
	}
	defer pool.Close()

	db, err := pool.SQLDB()
	if err != nil {
		fmt.Fprintf(stderr, "migrate: %v\n", err)
		return 1
	}
	m, err := database.NewMigrator(db, database.EmbeddedMigrations())
	if err != nil {
		fmt.Fprintf(stderr, "migrate: %v\n", err)
		return 1
	}

	if args[0] == "up" {
		return runUp(ctx, m, stdout, stderr)
	}
	return runStatus(ctx, m, stdout, stderr)
}

func runUp(ctx context.Context, m *database.Migrator, stdout, stderr io.Writer) int {
	applied, err := m.Up(ctx)
	for _, a := range applied {
		fmt.Fprintf(stdout, "applied %s (%s)\n", a.Source, a.Duration.Round(time.Millisecond))
	}
	if err != nil {
		fmt.Fprintf(stderr, "migrate: %v\n", err)
		return 1
	}
	if len(applied) == 0 {
		fmt.Fprintln(stdout, "no pending migrations")
	}
	return 0
}

func runStatus(ctx context.Context, m *database.Migrator, stdout, stderr io.Writer) int {
	statuses, err := m.Status(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "migrate: %v\n", err)
		return 1
	}
	w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "VERSION\tSTATE\tAPPLIED AT\tFILE")
	for _, s := range statuses {
		state, at := "pending", "-"
		if s.Applied {
			state, at = "applied", s.AppliedAt.UTC().Format(time.RFC3339)
		}
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\n", s.Version, state, at, s.Source)
	}
	if err := w.Flush(); err != nil {
		fmt.Fprintf(stderr, "migrate: %v\n", err)
		return 1
	}
	return 0
}
