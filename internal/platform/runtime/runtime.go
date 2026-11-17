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

	"github.com/relayforge/relayforge/internal/observability"
	"github.com/relayforge/relayforge/internal/platform/config"
	"github.com/relayforge/relayforge/internal/platform/httpserver"
)

// Run starts the named service and blocks until it receives SIGINT or SIGTERM
// (or the server fails), then shuts down gracefully. It returns a process exit code.
func Run(service, defaultHTTPAddr string) int {
	cfg, err := config.Load(defaultHTTPAddr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: invalid configuration: %v\n", service, err)
		return 2
	}
	log := observability.NewLogger(os.Stdout, cfg.LogLevel, cfg.LogFormat, service)

	srv := httpserver.New(cfg.HTTPAddr, httpserver.NewHandler(log, nil), cfg.ReadHeaderTimeout)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

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
