// Package runtime wires configuration, logging, the HTTP server and graceful
// shutdown into a single entry point shared by every RelayForge binary.
package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/Thiago-tods/relayforge/internal/app"
	"github.com/Thiago-tods/relayforge/internal/database"
	"github.com/Thiago-tods/relayforge/internal/delivery"
	"github.com/Thiago-tods/relayforge/internal/egress"
	"github.com/Thiago-tods/relayforge/internal/httpapi"
	"github.com/Thiago-tods/relayforge/internal/identity"
	"github.com/Thiago-tods/relayforge/internal/ingest"
	"github.com/Thiago-tods/relayforge/internal/messaging"
	"github.com/Thiago-tods/relayforge/internal/observability"
	"github.com/Thiago-tods/relayforge/internal/platform/config"
	"github.com/Thiago-tods/relayforge/internal/platform/httpserver"
	"github.com/Thiago-tods/relayforge/internal/ratelimit"
	"github.com/Thiago-tods/relayforge/internal/relay"
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
	// RunDeliveryWorker runs the outbound delivery worker. It requires RequireDatabase
	// and RELAYFORGE_ENCRYPTION_KEY, and is stopped and awaited before the pool closes.
	RunDeliveryWorker bool
	// RunRelay runs the outbox publisher and the fan-out consumer. It requires
	// RequireDatabase and RELAYFORGE_NATS_URL, and is stopped and awaited before
	// the pool closes.
	RunRelay bool
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

		// One registry per process: every role registers its metrics here and /metrics is
		// mounted once, so roles can be combined in one process without a duplicate route.
		var registry *observability.Registry
		if cfg.MetricsEnabled {
			registry = observability.NewRegistry()
			mounts = append(mounts, httpserver.Mount{Pattern: "GET /metrics", Handler: registry.Handler()})
			stopGauges := startPoolGauges(ctx, registry, pool)
			defer stopGauges()
		}

		if opts.RunDeliveryWorker {
			stop, code := startDeliveryWorker(ctx, service, cfg, pool, registry, log, stderr, checks)
			if code != 0 {
				return code
			}
			defer stop()
		}

		if opts.RunRelay {
			stop, code := startRelay(ctx, service, cfg, pool, registry, log, stderr, checks)
			if code != 0 {
				return code
			}
			defer stop()
		}

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

// startPoolGauges samples the connection pool into gauges until stopped. The returned
// function stops the sampler and waits for it, so the pool can be closed afterwards.
func startPoolGauges(ctx context.Context, reg *observability.Registry, pool *database.Pool) (stop func()) {
	acquired := reg.Gauge("relayforge_db_pool_acquired_connections", "Connections currently in use.")
	idle := reg.Gauge("relayforge_db_pool_idle_connections", "Open connections waiting for work.")
	total := reg.Gauge("relayforge_db_pool_total_connections", "Open connections, in use or idle.")
	maxConns := reg.Gauge("relayforge_db_pool_max_connections", "Configured connection limit.")
	sample := func() {
		s := pool.Stats()
		acquired.Set(int64(s.AcquiredConns))
		idle.Set(int64(s.IdleConns))
		total.Set(int64(s.TotalConns))
		maxConns.Set(int64(s.MaxConns))
	}
	sample()
	pctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-pctx.Done():
				return
			case <-t.C:
				sample()
			}
		}
	}()
	return func() { cancel(); <-done }
}

// effectiveShutdownGrace returns the worker's grace period, reduced so that the grace
// plus the time to store interrupted results fits in the process shutdown timeout.
func effectiveShutdownGrace(grace, shutdownTimeout time.Duration) time.Duration {
	return min(grace, max(shutdownTimeout-delivery.FinalizeTimeout, 0))
}

// startDeliveryWorker builds the delivery worker and runs it in one goroutine.
// The returned stop function cancels it and waits for it, so the caller can
// close the database pool afterwards. A non-zero code is a startup failure.
func startDeliveryWorker(ctx context.Context, service string, cfg config.Config, pool *database.Pool, registry *observability.Registry,
	log *slog.Logger, stderr io.Writer, checks map[string]httpserver.Check) (stop func(), code int) {
	if len(cfg.EncryptionKey) == 0 {
		fmt.Fprintf(stderr, "%s: RELAYFORGE_ENCRYPTION_KEY is required (32 bytes, base64)\n", service)
		return nil, 2
	}
	box, err := secretbox.New(cfg.EncryptionKey, 1)
	if err != nil {
		fmt.Fprintf(stderr, "%s: invalid RELAYFORGE_ENCRYPTION_KEY\n", service)
		return nil, 2
	}
	clientCfg := egress.DefaultClientConfig()
	clientCfg.RequestTimeout = cfg.Delivery.RequestTimeout
	client, err := egress.NewClient(clientCfg)
	if err != nil {
		fmt.Fprintf(stderr, "%s: invalid delivery client configuration: %v\n", service, err)
		return nil, 2
	}
	host, _ := os.Hostname()
	// ShutdownTimeout bounds the whole shutdown, and the worker runs concurrently with
	// the HTTP drain. Leave room for storing the results of interrupted requests.
	grace := effectiveShutdownGrace(cfg.Delivery.ShutdownGrace, cfg.ShutdownTimeout)
	if grace != cfg.Delivery.ShutdownGrace {
		log.Warn("delivery shutdown grace reduced to fit the shutdown timeout",
			"configured", cfg.Delivery.ShutdownGrace.String(), "effective", grace.String(), "shutdown_timeout", cfg.ShutdownTimeout.String())
	}
	worker, err := delivery.NewWorker(delivery.WorkerDeps{
		Pool: pool, Box: box, Sender: client, Policy: delivery.DefaultRetryPolicy(), Log: log, Metrics: delivery.NewMetrics(registry),
		Config: delivery.WorkerConfig{
			WorkerID:    fmt.Sprintf("%s-%s-%d", service, host, os.Getpid()),
			Concurrency: cfg.Delivery.Concurrency, BatchSize: cfg.Delivery.BatchSize, PollInterval: cfg.Delivery.PollInterval,
			RecoveryInterval: cfg.Delivery.RecoveryInterval, LeaseDuration: cfg.Delivery.LeaseDuration, ShutdownGrace: grace,
		},
	})
	if err != nil {
		fmt.Fprintf(stderr, "%s: invalid delivery worker configuration: %v\n", service, err)
		return nil, 2
	}

	checks["delivery_worker"] = worker.Check
	wctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := worker.Run(wctx); err != nil {
			log.Error("delivery worker stopped", "error_class", "worker_error")
		}
	}()
	log.Info("delivery worker started", "concurrency", cfg.Delivery.Concurrency)
	return func() { cancel(); <-done }, 0
}

// startRelay connects to JetStream, ensures the stream and consumer exist, and runs
// the outbox publisher and the fan-out consumer (with reconciliation). The returned
// stop function cancels both and waits for them. A non-zero code is a startup failure.
func startRelay(ctx context.Context, service string, cfg config.Config, pool *database.Pool, registry *observability.Registry,
	log *slog.Logger, stderr io.Writer, checks map[string]httpserver.Check) (stop func(), code int) {
	if cfg.NATS.URL == "" {
		fmt.Fprintf(stderr, "%s: RELAYFORGE_NATS_URL is required\n", service)
		return nil, 2
	}
	mcfg := messaging.DefaultConfig(cfg.NATS.URL)
	mcfg.Stream, mcfg.Consumer = cfg.NATS.Stream, cfg.NATS.Consumer
	if err := mcfg.Validate(); err != nil {
		fmt.Fprintf(stderr, "%s: invalid NATS configuration: %v\n", service, err)
		return nil, 2
	}
	conn, err := messaging.Connect(mcfg, service)
	if err != nil {
		log.Error("NATS unavailable at startup", "error_class", "nats_unavailable")
		return nil, 1
	}
	sctx, scancel := context.WithTimeout(ctx, 30*time.Second)
	defer scancel()
	if err := conn.EnsureStream(sctx); err != nil {
		conn.Close()
		log.Error("cannot configure the JetStream stream", "error_class", "nats_stream")
		return nil, 1
	}
	cons, err := conn.EnsureConsumer(sctx)
	if err != nil {
		conn.Close()
		log.Error("cannot configure the JetStream consumer", "error_class", "nats_consumer")
		return nil, 1
	}

	m := relay.NewMetrics(registry)
	pub, err := relay.NewPublisher(pool, conn, relay.DefaultPublisherConfig(), log, m)
	if err == nil {
		var fan *relay.FanOutConsumer
		if fan, err = relay.NewFanOutConsumer(pool, cons, relay.DefaultConsumerConfig(), log, m); err == nil {
			checks["nats"] = conn.Check
			checks["outbox_publisher"] = pub.Check
			checks["fanout_consumer"] = fan.Check
			rctx, cancel := context.WithCancel(ctx)
			var wg sync.WaitGroup
			wg.Add(2)
			go func() { defer wg.Done(); _ = pub.Run(rctx) }()
			go func() { defer wg.Done(); _ = fan.Run(rctx) }()
			log.Info("outbox relay started", "stream", mcfg.Stream)
			return func() { cancel(); wg.Wait(); conn.Close() }, 0
		}
	}
	conn.Close()
	fmt.Fprintf(stderr, "%s: invalid relay configuration: %v\n", service, err)
	return nil, 2
}
