package ingest

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Thiago-tods/relayforge/internal/database"
	"github.com/Thiago-tods/relayforge/internal/observability"
	"github.com/Thiago-tods/relayforge/internal/platform/apperror"
	"github.com/Thiago-tods/relayforge/internal/ratelimit"
	"github.com/Thiago-tods/relayforge/internal/secretbox"
)

// Client-facing errors. Messages never contain secrets, payloads or internals.
var (
	ErrNotFound            = apperror.New(http.StatusNotFound, "not_found", "ingestion endpoint not found")
	ErrPaused              = apperror.New(http.StatusServiceUnavailable, "endpoint_paused", "the ingestion endpoint is paused")
	ErrUnsupportedMedia    = apperror.New(http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json")
	ErrInvalidJSON         = apperror.New(http.StatusBadRequest, "invalid_json", "request body is not valid JSON")
	ErrInvalidTimestamp    = apperror.New(http.StatusBadRequest, "invalid_timestamp", "X-RelayForge-Timestamp is missing or malformed")
	ErrTimestampWindow     = apperror.New(http.StatusUnauthorized, "timestamp_out_of_window", "timestamp is outside the allowed window")
	ErrInvalidSignature    = apperror.New(http.StatusBadRequest, "invalid_signature", "X-RelayForge-Signature is missing or malformed")
	ErrSignatureMismatch   = apperror.New(http.StatusUnauthorized, "signature_mismatch", "signature verification failed")
	ErrInvalidIdempotency  = apperror.New(http.StatusBadRequest, "invalid_idempotency_key", "Idempotency-Key must be 1 to 255 printable ASCII characters")
	ErrIdempotencyConflict = apperror.New(http.StatusConflict, "idempotency_conflict", "the idempotency key was already used with a different payload")
)

// Config holds ingestion settings.
type Config struct {
	// TimestampSkew is the accepted difference between producer and server clocks.
	TimestampSkew time.Duration
	// PerEndpointPerMinute limits authenticated requests per endpoint.
	PerEndpointPerMinute int
	// PerIPPerMinute limits requests per client address before authentication.
	PerIPPerMinute int
}

// DefaultConfig returns the documented defaults.
func DefaultConfig() Config {
	return Config{TimestampSkew: DefaultTimestampSkew, PerEndpointPerMinute: 600, PerIPPerMinute: 300}
}

// Metrics are the ingestion counters and latency histograms. All fields may be nil.
type Metrics struct {
	Requests, SignatureFailures, TimestampFailures, Replays, Conflicts, Accepted, Rejected, TxFailures *observability.Counter
	RequestSeconds, SignatureSeconds, TxSeconds                                                        *observability.Histogram
}

// NewMetrics registers the ingestion metrics. reg may be nil.
func NewMetrics(reg *observability.Registry) *Metrics {
	return &Metrics{
		Requests:          reg.Counter("relayforge_ingest_requests_total", "Ingestion requests received."),
		SignatureFailures: reg.Counter("relayforge_ingest_signature_failures_total", "Requests rejected for a missing, malformed or wrong signature."),
		TimestampFailures: reg.Counter("relayforge_ingest_timestamp_failures_total", "Requests rejected for a missing, malformed or expired timestamp."),
		Replays:           reg.Counter("relayforge_ingest_idempotency_replays_total", "Requests answered from an earlier identical event."),
		Conflicts:         reg.Counter("relayforge_ingest_idempotency_conflicts_total", "Requests reusing an idempotency key with a different payload."),
		Accepted:          reg.Counter("relayforge_ingest_accepted_total", "New events accepted and committed."),
		Rejected:          reg.Counter("relayforge_ingest_rejected_total", "Requests rejected for any reason."),
		TxFailures:        reg.Counter("relayforge_ingest_db_tx_failures_total", "Ingestion database transactions that failed."),
		RequestSeconds:    reg.Histogram("relayforge_ingest_request_seconds", "Total ingestion request processing time."),
		SignatureSeconds:  reg.Histogram("relayforge_ingest_signature_seconds", "Time spent decrypting the secret and verifying the signature."),
		TxSeconds:         reg.Histogram("relayforge_ingest_db_tx_seconds", "Time spent in the ingestion database transaction."),
	}
}

// Service accepts signed events.
type Service struct {
	pool    *database.Pool
	box     *secretbox.Box
	cfg     Config
	now     func() time.Time
	log     *slog.Logger
	metrics *Metrics
}

// NewService creates the ingestion service. now may be nil; metrics may be nil.
func NewService(pool *database.Pool, box *secretbox.Box, cfg Config, now func() time.Time, log *slog.Logger, metrics *Metrics) *Service {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	if metrics == nil {
		metrics = &Metrics{}
	}
	return &Service{pool: pool, box: box, cfg: cfg, now: now, log: log, metrics: metrics}
}

// Request is a signed ingestion request. Body is the exact bytes received.
type Request struct {
	ContentType    string
	Timestamp      string
	Signature      string
	IdempotencyKey string
	Body           []byte
}

// Result is the outcome of an accepted request. Duplicate is true when an
// earlier identical request had already created the event.
type Result struct {
	EventID   string
	Duplicate bool
}

func (s *Service) querier() (database.Querier, error) { return s.pool.Querier() }

// Lookup performs the cheap, pre-authentication steps: validate the public
// identifier, apply the per-client rate limit, load the endpoint and check its
// status. Unknown and revoked endpoints are indistinguishable (404). Callers
// read the request body only after Lookup succeeds.
func (s *Service) Lookup(ctx context.Context, publicID, clientIP string) (Endpoint, error) {
	s.metrics.Requests.Inc()
	if !ValidPublicID(publicID) {
		s.metrics.Rejected.Inc()
		return Endpoint{}, ErrNotFound
	}
	db, err := s.querier()
	if err != nil {
		return Endpoint{}, err
	}
	if clientIP != "" && s.cfg.PerIPPerMinute > 0 {
		res, err := ratelimit.Hit(ctx, db, ratelimit.Bucket("ingest-ip", clientIP), time.Minute, s.cfg.PerIPPerMinute, s.now())
		if err != nil {
			return Endpoint{}, err
		}
		if !res.Allowed {
			s.metrics.Rejected.Inc()
			return Endpoint{}, ratelimit.Limited(res.RetryAfter)
		}
	}
	ep, err := EndpointByPublicID(ctx, db, publicID)
	if errors.Is(err, ErrEndpointNotFound) {
		s.metrics.Rejected.Inc()
		return Endpoint{}, ErrNotFound
	}
	if err != nil {
		return Endpoint{}, err
	}
	switch ep.Status {
	case StatusActive:
		return ep, nil
	case StatusPaused:
		s.metrics.Rejected.Inc()
		return Endpoint{}, ErrPaused
	default:
		s.metrics.Rejected.Inc()
		return Endpoint{}, ErrNotFound
	}
}

// Accept authenticates and persists one event for an endpoint returned by Lookup.
func (s *Service) Accept(ctx context.Context, ep Endpoint, req Request) (res Result, err error) {
	start := time.Now()
	defer func() {
		s.metrics.RequestSeconds.Observe(time.Since(start))
		if err != nil {
			s.metrics.Rejected.Inc()
		}
	}()

	if mt, _, perr := mime.ParseMediaType(req.ContentType); perr != nil || mt != "application/json" {
		return Result{}, ErrUnsupportedMedia
	}
	if !json.Valid(req.Body) {
		return Result{}, ErrInvalidJSON
	}

	if _, terr := ParseTimestamp(req.Timestamp, s.now(), s.cfg.TimestampSkew); terr != nil {
		s.metrics.TimestampFailures.Inc()
		if errors.Is(terr, ErrTimestampExpired) {
			return Result{}, ErrTimestampWindow
		}
		return Result{}, ErrInvalidTimestamp
	}
	if serr := ValidateSignatureFormat(req.Signature); serr != nil {
		s.metrics.SignatureFailures.Inc()
		return Result{}, ErrInvalidSignature
	}

	sigStart := time.Now()
	secret, oerr := s.box.Open(ep.SigningSecretCiphertext, SecretAAD(ep.ID), ep.EncryptionKeyVersion)
	if oerr != nil {
		// A server-side problem (wrong key, corrupted row), not a client error.
		s.log.Error("cannot decrypt endpoint signing secret", "endpoint_id", ep.ID, "error", oerr)
		return Result{}, fmt.Errorf("decrypt signing secret: %w", oerr)
	}
	ok, verr := VerifySignature(string(secret), req.Timestamp, req.Body, req.Signature)
	s.metrics.SignatureSeconds.Observe(time.Since(sigStart))
	if verr != nil || !ok {
		s.metrics.SignatureFailures.Inc()
		s.log.Warn("ingestion signature rejected", "endpoint_id", ep.ID)
		return Result{}, ErrSignatureMismatch
	}

	db, qerr := s.querier()
	if qerr != nil {
		return Result{}, qerr
	}
	if s.cfg.PerEndpointPerMinute > 0 {
		rl, rerr := ratelimit.Hit(ctx, db, ratelimit.Bucket("ingest-endpoint", ep.ID), time.Minute, s.cfg.PerEndpointPerMinute, s.now())
		if rerr != nil {
			return Result{}, rerr
		}
		if !rl.Allowed {
			return Result{}, ratelimit.Limited(rl.RetryAfter)
		}
	}

	if ierr := ValidateIdempotencyKey(req.IdempotencyKey); ierr != nil {
		return Result{}, ErrInvalidIdempotency
	}
	hash := PayloadHash(req.Body)
	eventID, uerr := NewUUID()
	if uerr != nil {
		return Result{}, uerr
	}

	txStart := time.Now()
	err = s.pool.InTx(ctx, func(tx pgx.Tx) error {
		return s.persist(ctx, tx, ep, req, hash, eventID, &res)
	})
	s.metrics.TxSeconds.Observe(time.Since(txStart))
	if err != nil {
		var ae *apperror.Error
		if !errors.As(err, &ae) {
			s.metrics.TxFailures.Inc()
		}
		return Result{}, err
	}
	switch {
	case res.Duplicate:
		s.metrics.Replays.Inc()
	default:
		s.metrics.Accepted.Inc()
	}
	return res, nil
}

// persist runs inside one transaction: re-check the endpoint under a shared row
// lock, insert the event (PostgreSQL arbitrates duplicates) and, only for a new
// event, the outbox record.
func (s *Service) persist(ctx context.Context, tx pgx.Tx, ep Endpoint, req Request, hash []byte, eventID string, res *Result) error {
	status, secretVersion, err := LockEndpointShared(ctx, tx, ep.ID)
	if errors.Is(err, ErrEndpointNotFound) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	switch status {
	case StatusPaused:
		return ErrPaused
	case StatusRevoked:
		return ErrNotFound
	}
	if secretVersion != ep.SecretVersion {
		// The secret was rotated after the request was verified; the old secret is no longer valid.
		s.log.Warn("ingestion rejected: secret rotated during request", "endpoint_id", ep.ID)
		return ErrSignatureMismatch
	}

	inserted, err := InsertEvent(ctx, tx, NewEvent{
		ID: eventID, OrganizationID: ep.OrganizationID, EndpointID: ep.ID, IdempotencyKey: req.IdempotencyKey,
		ContentType: "application/json", Payload: req.Body, PayloadSHA256: hash,
	})
	if err != nil {
		return err
	}
	if !inserted {
		existingID, existingHash, err := EventByIdempotencyKey(ctx, tx, ep.ID, req.IdempotencyKey)
		if err != nil {
			return err
		}
		if string(existingHash) != string(hash) {
			s.metrics.Conflicts.Inc()
			return ErrIdempotencyConflict
		}
		*res = Result{EventID: existingID, Duplicate: true}
		return nil
	}

	envelope, err := json.Marshal(map[string]any{
		"schema": 1, "eventId": eventID, "organizationId": ep.OrganizationID,
		"ingestionEndpointId": ep.ID, "payloadSha256": hex.EncodeToString(hash),
	})
	if err != nil {
		return fmt.Errorf("encode outbox payload: %w", err)
	}
	if err := InsertOutbox(ctx, tx, ep.OrganizationID, eventID, MessageEventAccepted, envelope); err != nil {
		return err
	}
	*res = Result{EventID: eventID}
	return nil
}
