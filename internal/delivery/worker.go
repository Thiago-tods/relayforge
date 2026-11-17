package delivery

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Thiago-tods/relayforge/internal/database"
	"github.com/Thiago-tods/relayforge/internal/egress"
	"github.com/Thiago-tods/relayforge/internal/secretbox"
)

// Sender performs the outbound HTTP request. *egress.Client implements it.
type Sender interface {
	Do(ctx context.Context, req egress.Request) (egress.Response, error)
}

// WorkerConfig tunes a Worker.
type WorkerConfig struct {
	// WorkerID labels claims in the database (claimed_by). It is not a secret.
	WorkerID string
	// Concurrency is the maximum number of requests in flight.
	Concurrency int
	// BatchSize is how many deliveries one claim takes; at most Concurrency.
	BatchSize int
	// PollInterval is how long the worker waits when no work is due.
	PollInterval time.Duration
	// RecoveryInterval is how often expired leases are recovered.
	RecoveryInterval time.Duration
	// LeaseDuration is how long a claim is valid. It must comfortably exceed the
	// request timeout, because an expired lease makes the delivery recoverable.
	LeaseDuration time.Duration
	// ShutdownGrace is how long Run lets in-flight requests finish after its context ends.
	ShutdownGrace time.Duration
}

// DefaultWorkerConfig returns the production defaults.
func DefaultWorkerConfig() WorkerConfig {
	return WorkerConfig{
		WorkerID: "delivery-worker", Concurrency: 8, BatchSize: 8, PollInterval: time.Second,
		RecoveryInterval: 30 * time.Second, LeaseDuration: 2 * time.Minute, ShutdownGrace: 20 * time.Second,
	}
}

// Validate rejects configurations that would break the lease or bounds guarantees.
func (c WorkerConfig) Validate() error {
	switch {
	case c.WorkerID == "" || len(c.WorkerID) > 200:
		return errors.New("worker id must be 1 to 200 characters")
	case c.Concurrency < 1 || c.Concurrency > 64:
		return errors.New("concurrency must be between 1 and 64")
	case c.BatchSize < 1 || c.BatchSize > c.Concurrency:
		return errors.New("batch size must be between 1 and the concurrency")
	case c.PollInterval <= 0 || c.RecoveryInterval <= 0:
		return errors.New("poll and recovery intervals must be positive")
	case c.LeaseDuration < 30*time.Second:
		return errors.New("lease duration must be at least 30 seconds")
	case c.ShutdownGrace < 0:
		return errors.New("shutdown grace must not be negative")
	}
	return nil
}

// WorkerDeps are the collaborators of a Worker.
type WorkerDeps struct {
	Pool    *database.Pool
	Box     *secretbox.Box
	Sender  Sender
	Policy  RetryPolicy
	Config  WorkerConfig
	Log     *slog.Logger
	Metrics *Metrics
	// Now and Rand make time and jitter injectable. They default to the wall clock and math/rand.
	Now  func() time.Time
	Rand func() float64
	// AfterClaim, if set, runs after a delivery's claim is committed and before
	// its data is loaded. It is a seam for tests that change state in that window.
	AfterClaim func()
}

// Worker claims due deliveries, sends them, records the results, and recovers
// deliveries whose worker disappeared. Delivery is at-least-once: if a worker
// dies after the destination handled a request but before the result was stored,
// the delivery is retried, so receivers must deduplicate on X-RelayForge-Delivery-Id.
type Worker struct {
	d WorkerDeps
}

// NewWorker validates the dependencies and returns a Worker.
func NewWorker(d WorkerDeps) (*Worker, error) {
	switch {
	case d.Pool == nil || d.Box == nil || d.Sender == nil || d.Log == nil:
		return nil, errors.New("delivery worker: pool, box, sender and logger are required")
	}
	if err := d.Config.Validate(); err != nil {
		return nil, fmt.Errorf("delivery worker: %w", err)
	}
	if err := d.Policy.Validate(); err != nil {
		return nil, fmt.Errorf("delivery worker: %w", err)
	}
	if d.Metrics == nil {
		d.Metrics = &Metrics{}
	}
	if d.Now == nil {
		d.Now = func() time.Time { return time.Now().UTC() }
	}
	return &Worker{d: d}, nil
}

func (w *Worker) querier() (database.Querier, error) { return w.d.Pool.Querier() }

// claim takes up to n due deliveries in a short transaction that commits before any network I/O.
func (w *Worker) claim(ctx context.Context, n int) ([]Claimed, error) {
	var claimed []Claimed
	err := w.d.Pool.InTx(ctx, func(tx pgx.Tx) error {
		var err error
		claimed, err = ClaimDue(ctx, tx, w.d.Now(), n, w.d.Config.LeaseDuration, w.d.Config.WorkerID)
		return err
	})
	if err != nil {
		w.d.Metrics.ClaimErrors.Inc()
		return nil, err
	}
	for range claimed {
		w.d.Metrics.Claimed.Inc()
	}
	return claimed, nil
}

// RunOnce claims one batch and processes it with bounded concurrency, returning
// the number of deliveries handled. It is the unit Run repeats, and is useful
// for tests and one-shot jobs.
func (w *Worker) RunOnce(ctx context.Context) (int, error) {
	claimed, err := w.claim(ctx, w.d.Config.BatchSize)
	if err != nil {
		return 0, err
	}
	var wg sync.WaitGroup
	for _, c := range claimed {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.Process(ctx, c)
		}()
	}
	wg.Wait()
	return len(claimed), nil
}

// Process delivers one claimed delivery and stores the result. It never
// returns an error: failures are logged and counted, and an unrecorded result is
// repaired by lease recovery.
func (w *Worker) Process(ctx context.Context, c Claimed) {
	log := w.d.Log.With("delivery_id", c.DeliveryID, "destination_id", c.DestinationID, "organization_id", c.OrganizationID, "attempt", c.Attempt)
	if w.d.AfterClaim != nil {
		w.d.AfterClaim()
	}

	q, err := w.querier()
	if err != nil {
		log.Error("cannot load delivery", "error_class", "database_closed")
		return
	}
	data, err := LoadSendData(ctx, q, c.OrganizationID, c.DeliveryID)
	startedAt := w.d.Now()
	var (
		result AttemptResult
		httpMS time.Duration
		sent   bool
	)
	switch {
	case errors.Is(err, ErrNotFound):
		result = AttemptResult{Outcome: OutcomeBlockedByPolicy, ErrorClass: "data_missing"}
	case err != nil:
		if ctx.Err() == nil {
			log.Error("cannot load delivery", "error_class", "database_error")
		}
		return // the lease expires and recovery records the attempt
	case data.DestinationStatus != "ACTIVE":
		result = AttemptResult{Outcome: OutcomeAbandoned, ErrorClass: "destination_inactive"}
	default:
		req, ok := w.buildRequest(c, data, startedAt)
		if !ok {
			result = AttemptResult{Outcome: OutcomeAbandoned, ErrorClass: "secret_unavailable"}
			log.Error("cannot decrypt the destination secret", "error_class", "secret_unavailable", "key_version", data.KeyVersion)
			break
		}
		sendStart := time.Now()
		resp, derr := w.d.Sender.Do(ctx, req)
		httpMS = time.Since(sendStart)
		sent = true
		if derr != nil {
			var re *egress.RequestError
			class := egress.ClassProtocol
			if errors.As(derr, &re) {
				class = re.Class
			}
			classified := ResultFromRequestError(class)
			if classified.Interrupted {
				w.d.Metrics.Interrupted.Inc()
				log.Info("request interrupted by shutdown; the lease will be recovered")
				return
			}
			result = classified.Result
		} else {
			result = ResultFromResponse(resp.StatusCode, resp.RetryAfter, w.d.Now())
		}
	}
	if sent {
		w.d.Metrics.AttemptSeconds.Observe(httpMS)
	}

	finishedAt := w.d.Now()
	decision := w.policyFor(c.MaxAttempts).Decide(c.Attempt, result, finishedAt, w.d.Rand)
	rec := AttemptRecord{
		Number: c.Attempt, StartedAt: startedAt, FinishedAt: finishedAt,
		Outcome: result.Outcome, HTTPStatus: result.HTTPStatus, ErrorClass: result.ErrorClass,
	}

	// Finalize with a context that survives shutdown: a result that was obtained must be stored.
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	err = w.d.Pool.InTx(fctx, func(tx pgx.Tx) error {
		return Finish(fctx, tx, c.OrganizationID, c.DeliveryID, c.ClaimToken, rec, decision, finishedAt)
	})
	switch {
	case errors.Is(err, ErrStaleClaim):
		w.d.Metrics.StaleFinalizations.Inc()
		log.Warn("result discarded: the claim is no longer current")
		return
	case err != nil:
		w.d.Metrics.FinalizeErrors.Inc()
		log.Error("cannot store the delivery result; lease recovery will record the attempt", "error_class", "database_error")
		return
	}
	w.count(decision, result, data, finishedAt)
	attrs := []any{"outcome", string(result.Outcome), "status", string(decision.Status), "reason", decision.Reason, "duration_ms", httpMS.Milliseconds()}
	if result.HTTPStatus != 0 {
		attrs = append(attrs, "http_status", result.HTTPStatus)
	}
	if result.ErrorClass != "" {
		attrs = append(attrs, "error_class", result.ErrorClass)
	}
	if decision.Status == StatusSucceeded {
		log.Debug("delivery succeeded", attrs...)
	} else {
		log.Info("delivery attempt failed", attrs...)
	}
}

// policyFor returns the retry policy bounded by the delivery's own max_attempts:
// the schema requires a PENDING delivery to have attempts left, so the decision
// must never schedule a retry beyond what the row allows.
func (w *Worker) policyFor(maxAttempts int) RetryPolicy {
	p := w.d.Policy
	if maxAttempts > 0 && maxAttempts < p.MaxAttempts {
		p.MaxAttempts = maxAttempts
	}
	return p
}

func (w *Worker) count(d Decision, r AttemptResult, data SendData, now time.Time) {
	if r.Outcome == OutcomeAbandoned {
		w.d.Metrics.AbandonedAttempts.Inc()
	}
	switch d.Status {
	case StatusSucceeded:
		w.d.Metrics.Succeeded.Inc()
		if !data.CreatedAt.IsZero() {
			if age := now.Sub(data.CreatedAt); age > 0 {
				w.d.Metrics.EndToEndSeconds.Observe(age)
			} else {
				w.d.Metrics.EndToEndSeconds.Observe(0)
			}
		}
	case StatusPending:
		w.d.Metrics.RetriesScheduled.Inc()
	case StatusFailed:
		w.d.Metrics.Failed.Inc()
	}
}

// buildRequest decrypts the destination secret and builds the signed request.
// The secret and ciphertext come from the same row as the secret version sent in
// the headers, so a delivery is never signed with a secret of another version.
func (w *Worker) buildRequest(c Claimed, data SendData, signedAt time.Time) (egress.Request, bool) {
	secret, err := w.d.Box.Open(data.Ciphertext, DestinationSecretAAD(data.DestinationID), data.KeyVersion)
	if err != nil {
		return egress.Request{}, false
	}
	headers := OutboundHeaders(OutboundParams{
		Secret: string(secret), SecretVersion: data.SecretVersion, DeliveryID: data.DeliveryID, EventID: data.EventID,
		Attempt: c.Attempt, ContentType: data.ContentType, EventReceivedAt: data.EventReceivedAt, SignedAt: signedAt, Body: data.Payload,
	})
	return egress.Request{URL: data.URL, Headers: headers, Body: data.Payload}, true
}

const recoveryBatch = 20

// RecoverExpired finds deliveries left IN_FLIGHT by a worker that disappeared
// and records an ABANDONED attempt for each, then schedules a retry or fails the
// delivery when attempts are exhausted. It returns how many it recovered.
//
// Each batch runs in one transaction with FOR UPDATE SKIP LOCKED, so concurrent
// recovery processes take different rows and a worker that is finalizing at
// this moment is skipped. The attempt is inserted first and the delivery updated
// second, as the schema requires; a worker that finalizes later finds a different
// claim token and is rejected. Recovery never mutates earlier attempts and never
// skips an attempt number.
func (w *Worker) RecoverExpired(ctx context.Context) (int, error) {
	total := 0
	for {
		n, err := w.recoverBatch(ctx)
		total += n
		if err != nil {
			return total, err
		}
		if n < recoveryBatch {
			return total, nil
		}
	}
}

func (w *Worker) recoverBatch(ctx context.Context) (int, error) {
	recovered := 0
	err := w.d.Pool.InTx(ctx, func(tx pgx.Tx) error {
		now := w.d.Now()
		expired, err := ExpiredLeases(ctx, tx, now, recoveryBatch)
		if err != nil {
			return err
		}
		for _, e := range expired {
			finished := now
			if finished.Before(e.ClaimedAt) {
				finished = e.ClaimedAt
			}
			result := AttemptResult{Outcome: OutcomeAbandoned, ErrorClass: "lease_expired"}
			decision := w.policyFor(e.MaxAttempts).Decide(e.Attempt, result, finished, w.d.Rand)
			rec := AttemptRecord{Number: e.Attempt, StartedAt: e.ClaimedAt, FinishedAt: finished, Outcome: OutcomeAbandoned, ErrorClass: "lease_expired"}

			// A savepoint keeps one unrecoverable delivery from blocking the rest of the batch.
			sp, err := tx.Begin(ctx)
			if err != nil {
				return err
			}
			ferr := Finish(ctx, sp, e.OrganizationID, e.DeliveryID, e.ClaimToken, rec, decision, finished)
			if ferr != nil {
				if rbErr := sp.Rollback(ctx); rbErr != nil {
					return rbErr
				}
				w.d.Metrics.RecoveryErrors.Inc()
				w.d.Log.Error("cannot recover an expired lease", "delivery_id", e.DeliveryID, "organization_id", e.OrganizationID, "attempt", e.Attempt, "error_class", "recovery_failed")
				continue
			}
			if err := sp.Commit(ctx); err != nil {
				return err
			}
			recovered++
			w.d.Metrics.AbandonedAttempts.Inc()
			w.d.Metrics.ExpiredRecovered.Inc()
			if decision.Status == StatusFailed {
				w.d.Metrics.Failed.Inc()
			} else {
				w.d.Metrics.RetriesScheduled.Inc()
			}
			w.d.Log.Info("expired lease recovered", "delivery_id", e.DeliveryID, "organization_id", e.OrganizationID, "attempt", e.Attempt, "status", string(decision.Status))
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("recover expired leases: %w", err)
	}
	return recovered, nil
}

// CancelRevoked cancels pending deliveries of revoked destinations.
func (w *Worker) CancelRevoked(ctx context.Context) (int64, error) {
	q, err := w.querier()
	if err != nil {
		return 0, err
	}
	n, err := CancelForRevokedDestinations(ctx, q, w.d.Now())
	if err == nil {
		for range n {
			w.d.Metrics.CancelledRevoked.Inc()
		}
	}
	return n, err
}

// SampleGauges refreshes the queue gauges from PostgreSQL.
func (w *Worker) SampleGauges(ctx context.Context) error {
	q, err := w.querier()
	if err != nil {
		return err
	}
	c, err := QueueCounts(ctx, q, w.d.Now())
	if err != nil {
		return err
	}
	m := w.d.Metrics
	m.Pending.Set(c.Pending)
	m.DuePending.Set(c.DuePending)
	m.InFlight.Set(c.InFlight)
	m.ExpiredLeases.Set(c.ExpiredLeases)
	m.OutboxUnpublished.Set(c.OutboxUnpublished)
	return nil
}

// Run processes deliveries until ctx ends. At most Concurrency requests are in
// flight and nothing is queued beyond them: the worker only claims as many
// deliveries as it has free slots. A second goroutine recovers expired leases,
// cancels deliveries of revoked destinations and samples gauges.
//
// On shutdown Run stops claiming, gives in-flight requests ShutdownGrace to
// finish, then interrupts the rest. Interrupted requests are not recorded as
// failures; their leases expire and recovery records them as abandoned.
func (w *Worker) Run(ctx context.Context) error {
	cfg := w.d.Config
	execCtx, cancelExec := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelExec()

	sem := make(chan struct{}, cfg.Concurrency)
	var wg sync.WaitGroup

	maintenanceDone := make(chan struct{})
	go func() {
		defer close(maintenanceDone)
		w.maintain(ctx)
	}()

	for ctx.Err() == nil {
		if free := cap(sem) - len(sem); free > 0 {
			n := min(free, cfg.BatchSize)
			claimed, err := w.claim(ctx, n)
			if err != nil {
				if ctx.Err() == nil {
					w.d.Log.Error("cannot claim deliveries", "error_class", "database_error")
				}
			}
			for _, c := range claimed {
				sem <- struct{}{} // never blocks: at most `free` slots were requested
				wg.Add(1)
				go func() {
					defer wg.Done()
					defer func() { <-sem }()
					w.Process(execCtx, c)
				}()
			}
			if err == nil && len(claimed) == n {
				continue // more work is probably waiting
			}
		}
		timer := time.NewTimer(cfg.PollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}

	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(cfg.ShutdownGrace):
		cancelExec()
		<-finished
	}
	<-maintenanceDone
	return nil
}

// maintain runs recovery, cancellation and gauge sampling on a timer until ctx ends.
func (w *Worker) maintain(ctx context.Context) {
	ticker := time.NewTicker(w.d.Config.RecoveryInterval)
	defer ticker.Stop()
	for {
		if _, err := w.RecoverExpired(ctx); err != nil && ctx.Err() == nil {
			w.d.Log.Error("lease recovery failed", "error_class", "database_error")
		}
		if _, err := w.CancelRevoked(ctx); err != nil && ctx.Err() == nil {
			w.d.Log.Error("cancelling revoked deliveries failed", "error_class", "database_error")
		}
		if err := w.SampleGauges(ctx); err != nil && ctx.Err() == nil {
			w.d.Log.Error("sampling delivery gauges failed", "error_class", "database_error")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
