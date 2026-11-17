// Package runtime wires configuration, logging, the HTTP server and graceful
// shutdown into a single entry point shared by every RelayForge binary.
package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Thiago-tods/relayforge/internal/app"
	"github.com/Thiago-tods/relayforge/internal/database"
	"github.com/Thiago-tods/relayforge/internal/httpapi"
	"github.com/Thiago-tods/relayforge/internal/identity"
	"github.com/Thiago-tods/relayforge/internal/ingest"
	"github.com/Thiago-tods/relayforge/internal/observability"
	"github.com/Thiago-tods/relayforge/internal/platform/config"
	"github.com/Thiago-tods/relayforge/internal/platform/httpserver"
	"github.com/Thiago-tods/relayforge/internal/ratelimit"
	"github.com/Thiago-tods/relayforge/internal/secretbox"
)

// Options selects the optional dependencies a process owns.
type Options struct {
	// RequireDatabase makes the process open a PostgreSQL pool at startup,
	// refuse to start when it cannot connect, report it in /readyz and close it
	// during graceful shutdown. Processes that do not use PostgreSQL yet must
	// leave it false so they do not hold unused connections.
	RequireDatabase bool
	// ServeAPI mounts the /api/v1 HTTP API. It requires RequireDatabase.
	ServeAPI bool
}

// Run starts the named service and blocks until it receives SIGINT or SIGTERM
// (or the server fails), then shuts down gracefully. It returns a process exit code.
func Run(service, defaultHTTPAddr string, opts Options) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return run(ctx, service, defaultHTTPAddr, opts, os.LookupEnv, os.Stdout, os.Stderr, nil)
}

// run is Run with its environment injected so it can be tested. It stops when
// ctx is cancelled. ready, if not nil, is called with the bound listen address
// once the HTTP server accepts connections, which lets tests use port 0.
// Exit codes: 0 clean shutdown, 1 runtime failure, 2 invalid configuration.
func run(ctx context.Context, service, defaultHTTPAddr string, opts Options,
	lookup func(string) (string, bool), stdout, stderr io.Writer, ready func(addr string)) int {
	cfg, err := config.LoadFrom(lookup, defaultHTTPAddr)
	if err != nil {
		fmt.Fprintf(stderr, "%s: invalid configuration: %v\n", service, err)
		return 2
	}
	log := observability.NewLogger(stdout, cfg.LogLevel, cfg.LogFormat, service)

	checks := map[string]httpserver.Check{}
	var mounts []httpserver.Mount
	if opts.RequireDatabase {
		dbCfg, err := config.LoadDatabaseFrom(lookup)
		if err == nil {
			err = dbCfg.Validate()
		}
		if err != nil {
			fmt.Fprintf(stderr, "%s: invalid database configuration: %v\n", service, err)
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

		if opts.ServeAPI {
			if len(cfg.EncryptionKey) == 0 {
				fmt.Fprintf(stderr, "%s: RELAYFORGE_ENCRYPTION_KEY is required (32 bytes, base64)\n", service)
				return 2
			}
			box, err := secretbox.New(cfg.EncryptionKey, 1)
			if err != nil {
				fmt.Fprintf(stderr, "%s: invalid RELAYFORGE_ENCRYPTION_KEY\n", service)
				return 2
			}
			var registry *observability.Registry
			if cfg.MetricsEnabled {
				registry = observability.NewRegistry()
				mounts = append(mounts, httpserver.Mount{Pattern: "GET /metrics", Handler: registry.Handler()})
			}
			appCfg := app.DefaultConfig()
			appCfg.SessionLifetime = cfg.SessionLifetime
			appCfg.Box = box
			appCfg.LoginFailuresPerAccount = cfg.LoginFailuresPerAccount
			appCfg.LoginFailuresPerIP = cfg.LoginFailuresPerIP
			svc, err := app.New(pool, identity.NewHasher(identity.DefaultArgon2Params), appCfg, nil, log)
			if err != nil {
				log.Error("cannot initialize application service", "error", err)
				return 1
			}
			ingestCfg := ingest.DefaultConfig()
			ingestCfg.PerEndpointPerMinute = cfg.IngestPerEndpointPerMinute
			ingestCfg.PerIPPerMinute = cfg.IngestPerIPPerMinute
			ing := ingest.NewService(pool, box, ingestCfg, nil, log, ingest.NewMetrics(registry))
			mounts = append(mounts, httpserver.Mount{Pattern: "/api/", Handler: httpapi.New(svc, ing, log, cfg.CookieSecure).Handler()})

			// One owned goroutine removes expired rate-limit counters; it is stopped
			// and awaited before the pool is closed.
			prunerCtx, stopPruner := context.WithCancel(ctx)
			prunerDone := make(chan struct{})
			go func() {
				defer close(prunerDone)
				ratelimit.RunPruner(prunerCtx, pool, time.Hour, 24*time.Hour, func(err error) {
					log.Warn("rate limit pruning failed", "error", err)
				})
			}()
			defer func() { stopPruner(); <-prunerDone }()
		}
	}

	srv := httpserver.New(cfg.HTTPAddr, httpserver.NewHandlerWith(log, checks, mounts...), cfg.ReadHeaderTimeout)

	ln, err := net.Listen("tcp", cfg.HTTPAddr)
	if err != nil {
		log.Error("http listen failed", "addr", cfg.HTTPAddr, "error", err)
		return 1
	}
	log.Info("starting http server", "addr", ln.Addr().String(), "env", cfg.Env)

	errCh := make(chan error, 1)
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()
	if ready != nil {
		ready(ln.Addr().String())
	}

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
