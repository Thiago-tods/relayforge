// Package httpserver builds the HTTP server shared by all RelayForge processes.
package httpserver

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/Thiago-tods/relayforge/internal/platform/apperror"
)

// Check reports whether a dependency is ready to serve traffic.
type Check func(ctx context.Context) error

// NewHandler returns a mux with health endpoints and panic recovery:
//
//	GET /healthz  liveness: the process is running
//	GET /readyz   readiness: all registered checks pass
func NewHandler(log *slog.Logger, checks map[string]Check) http.Handler {
	return NewHandlerWith(log, checks)
}

// Mount attaches an application handler at a ServeMux pattern such as "/api/".
type Mount struct {
	Pattern string
	Handler http.Handler
}

// NewHandlerWith is NewHandler plus application handlers. The health endpoints
// are always registered first and cannot be shadowed.
func NewHandlerWith(log *slog.Logger, checks map[string]Check, mounts ...Mount) http.Handler {
	mux := http.NewServeMux()
	for _, m := range mounts {
		mux.Handle(m.Pattern, m.Handler)
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		results := make(map[string]string, len(checks))
		ready := true
		for name, check := range checks {
			if err := check(ctx); err != nil {
				ready = false
				results[name] = "unavailable"
				log.Warn("readiness check failed", "check", name, "error", err)
				continue
			}
			results[name] = "ok"
		}
		status, code := "ok", http.StatusOK
		if !ready {
			status, code = "unavailable", http.StatusServiceUnavailable
		}
		writeJSON(w, code, map[string]any{"status": status, "checks": results})
	})
	return recoverer(log, mux)
}

// New creates an http.Server with conservative timeouts.
func New(addr string, handler http.Handler, readHeaderTimeout time.Duration) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		// Bounds header memory; request bodies are limited separately by handlers.
		MaxHeaderBytes: 64 << 10,
	}
}

func recoverer(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Error("panic recovered", "panic", rec, "path", r.URL.Path)
				apperror.Write(w, nil)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
