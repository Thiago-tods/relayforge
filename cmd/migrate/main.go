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
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) != 1 || (args[0] != "up" && args[0] != "status") {
		fmt.Fprintln(os.Stderr, "usage: migrate <up|status>")
		return 2
	}

	dbCfg, err := config.LoadDatabase()
	if err != nil {
		fmt.Fprintf(os.Stderr, "migrate: invalid database configuration: %v\n", err)
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log := observability.NewLogger(os.Stderr, "warn", "text", "migrate")
	pool, err := database.Open(ctx, dbCfg, log)
	if err != nil {
		fmt.Fprintf(os.Stderr, "migrate: %v\n", err)
		return 1
	}
	defer pool.Close()

	m, err := database.NewMigrator(pool.SQLDB(), database.EmbeddedMigrations())
	if err != nil {
		fmt.Fprintf(os.Stderr, "migrate: %v\n", err)
		return 1
	}

	if args[0] == "up" {
		applied, err := m.Up(ctx)
		for _, a := range applied {
			fmt.Printf("applied %s (%s)\n", a.Source, a.Duration.Round(time.Millisecond))
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "migrate: %v\n", err)
			return 1
		}
		if len(applied) == 0 {
			fmt.Println("no pending migrations")
		}
		return 0
	}

	statuses, err := m.Status(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "migrate: %v\n", err)
		return 1
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "VERSION\tSTATE\tAPPLIED AT\tFILE")
	for _, s := range statuses {
		state, at := "pending", "-"
		if s.Applied {
			state, at = "applied", s.AppliedAt.UTC().Format(time.RFC3339)
		}
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\n", s.Version, state, at, s.Source)
	}
	if err := w.Flush(); err != nil {
		fmt.Fprintf(os.Stderr, "migrate: %v\n", err)
		return 1
	}
	return 0
}
