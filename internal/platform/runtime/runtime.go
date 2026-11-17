// Package runtime wires configuration, logging, the HTTP server and graceful
// shutdown into a single entry point shared by every RelayForge binary.
package runtime

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/Thiago-tods/relayforge/internal/database"
	"github.com/Thiago-tods/relayforge/internal/observability"
	"github.com/Thiago-tods/relayforge/internal/platform/config"
	"github.com/Thiago-tods/relayforge/internal/platform/httpserver"
)

// Options selects the optional dependencies a process owns.
type Options struct {
	// RequireDatabase makes the process open a PostgreSQL pool at startup,
	// refuse to start when it cannot connect, report it in /readyz and close it
	// during graceful shutdown. Processes that do not use PostgreSQL yet must
	// leave it false so they do not hold unused connections.
	RequireDatabase bool
}

// Run starts the named service and blocks until it receives SIGINT or SIGTERM
// (or the server fails), then shuts down gracefully. It returns a process exit code.
func Run(service, defaultHTTPAddr string, opts Options) int {
	cfg, err := config.Load(defaultHTTPAddr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: invalid configuration: %v\n", service, err)
		return 2
	}
	log := observability.NewLogger(os.Stdout, cfg.LogLevel, cfg.LogFormat, service)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	checks := map[string]httpserver.Check{}
	if opts.RequireDatabase {
		dbCfg, err := config.LoadDatabase()
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: invalid database configuration: %v\n", service, err)
			return 2
		}
		if err := dbCfg.Validate(); err != nil {
			fmt.Fprintf(os.Stderr, "%s: invalid database configuration: %v\n", service, err)
			return 2
		}
		pool, err := database.Open(ctx, dbCfg, log)
		if err != nil {
			log.Error("database unavailable at startup", "error", err)
			return 1
		}
		// Closed after the HTTP server has drained, because defers run on return.
		defer pool.Close()
		checks["postgres"] = pool.Check
	}

	srv := httpserver.New(cfg.HTTPAddr, httpserver.NewHandler(log, checks), cfg.ReadHeaderTimeout)

	errCh := make(chan error, 1)
	go func() {
		log.Info("starting http server", "addr", cfg.HTTPAddr, "env", cfg.Env)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case err := <-errCh:
		if err != nil {
			log.Error("http server failed", "error", err)
			return 1
		}
	case <-ctx.Done():
		log.Info("shutdown signal received")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("graceful shutdown failed", "error", err)
		return 1
	}
	log.Info("shutdown complete")
	return 0
}
