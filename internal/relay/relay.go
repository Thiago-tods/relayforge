// Package relay moves accepted events from the PostgreSQL outbox to JetStream
// (Publisher) and from JetStream to deliveries (FanOutConsumer).
//
// Guarantees, stated precisely:
//   - At-least-once everywhere. A crash between a successful publish and the
//     published_at update republishes the row; JetStream drops the duplicate
//     inside its window (same Nats-Msg-Id) and the fan-out is idempotent anyway.
//   - published_at is set only after the server acknowledged the message.
//   - A message is acknowledged only after the fan-out transaction committed.
//   - Nothing here is exactly-once. PostgreSQL stays the source of truth and
//     delivery.Reconcile repairs anything the broker loses.
package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Thiago-tods/relayforge/internal/database"
	"github.com/Thiago-tods/relayforge/internal/delivery"
	"github.com/Thiago-tods/relayforge/internal/messaging"
	"github.com/Thiago-tods/relayforge/internal/observability"
)

// Envelope is the JetStream message body: identifiers only, never the payload.
type Envelope struct {
	OutboxID       string `json:"outbox_id"`
	OrganizationID string `json:"organization_id"`
	EventID        string `json:"event_id"`
}

// Metrics are label-free counters; nil fields ignore updates.
type Metrics struct {
	Published        *observability.Counter
	PublishDuplicate *observability.Counter
	PublishErrors    *observability.Counter
	FanOutMessages   *observability.Counter
	FanOutCreated    *observability.Counter
	FanOutErrors     *observability.Counter
	PoisonMessages   *observability.Counter
	Reconciled       *observability.Counter
	ReconcileErrors  *observability.Counter
}

// NewMetrics registers the relay metrics. reg may be nil.
func NewMetrics(reg *observability.Registry) *Metrics {
	return &Metrics{
		Published:        reg.Counter("relayforge_outbox_published_total", "Outbox records acknowledged by JetStream."),
		PublishDuplicate: reg.Counter("relayforge_outbox_publish_duplicates_total", "Publishes JetStream recognised as duplicates."),
		PublishErrors:    reg.Counter("relayforge_outbox_publish_errors_total", "Failed publishes; the record is retried later."),
		FanOutMessages:   reg.Counter("relayforge_fanout_messages_total", "Messages processed by the fan-out consumer."),
		FanOutCreated:    reg.Counter("relayforge_fanout_deliveries_created_total", "Deliveries created by fan-out."),
		FanOutErrors:     reg.Counter("relayforge_fanout_errors_total", "Fan-out failures; the message is redelivered."),
		PoisonMessages:   reg.Counter("relayforge_fanout_poison_messages_total", "Malformed messages terminated without retry."),
		Reconciled:       reg.Counter("relayforge_reconciled_deliveries_total", "Deliveries created by the reconciliation sweep."),
		ReconcileErrors:  reg.Counter("relayforge_reconcile_errors_total", "Failed reconciliation sweeps."),
	}
}

// PublisherConfig bounds the outbox publisher.
type PublisherConfig struct {
	BatchSize    int
	PollInterval time.Duration
	// Lease is how long a claimed row stays invisible to other publishers. If the
	// publisher dies, the row becomes due again after the lease.
	Lease time.Duration
	// MaxBackoff caps the delay after failed publishes.
	MaxBackoff time.Duration
}

// DefaultPublisherConfig returns the documented defaults.
func DefaultPublisherConfig() PublisherConfig {
	return PublisherConfig{BatchSize: 50, PollInterval: time.Second, Lease: time.Minute, MaxBackoff: 5 * time.Minute}
}

// Validate rejects unusable settings.
func (c PublisherConfig) Validate() error {
	if c.BatchSize < 1 || c.BatchSize > 1000 || c.PollInterval <= 0 || c.Lease < 5*time.Second || c.MaxBackoff <= 0 {
		return errors.New("invalid outbox publisher configuration")
	}
	return nil
}

// Publisher publishes unpublished outbox rows to JetStream.
type Publisher struct {
	pool *database.Pool
	conn *messaging.Conn
	cfg  PublisherConfig
	log  *slog.Logger
	m    *Metrics
	now  func() time.Time
	beat *observability.Heartbeat
}

// Check reports whether the publish loop is making progress (see observability.Heartbeat).
func (p *Publisher) Check(context.Context) error {
	return p.beat.Check(max(10*p.cfg.PollInterval, 30*time.Second))
}

// NewPublisher validates its inputs.
func NewPublisher(pool *database.Pool, conn *messaging.Conn, cfg PublisherConfig, log *slog.Logger, m *Metrics) (*Publisher, error) {
	if pool == nil || conn == nil || log == nil {
		return nil, errors.New("publisher: pool, connection and logger are required")
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if m == nil {
		m = &Metrics{}
	}
	return &Publisher{pool: pool, conn: conn, cfg: cfg, log: log, m: m, now: func() time.Time { return time.Now().UTC() }, beat: observability.NewHeartbeat()}, nil
}

type claimedRow struct {
	ID, OrgID, EventID string
	Attempts           int
}

// claim leases up to BatchSize due rows by pushing available_at forward, in a
// short transaction that is committed before anything is published.
func (p *Publisher) claim(ctx context.Context) ([]claimedRow, error) {
	var rows []claimedRow
	err := p.pool.InTx(ctx, func(tx pgx.Tx) error {
		rs, err := tx.Query(ctx, `
			WITH due AS (
				SELECT id FROM outbox
				 WHERE published_at IS NULL AND available_at <= $1
				 ORDER BY available_at, id LIMIT $2
				 FOR UPDATE SKIP LOCKED)
			UPDATE outbox o SET attempts = o.attempts + 1, available_at = $1::timestamptz + make_interval(secs => $3)
			  FROM due WHERE o.id = due.id
			RETURNING o.id::text, o.organization_id::text, o.event_id::text, o.attempts`,
			p.now(), p.cfg.BatchSize, p.cfg.Lease.Seconds())
		if err != nil {
			return err
		}
		defer rs.Close()
		for rs.Next() {
			var r claimedRow
			if err := rs.Scan(&r.ID, &r.OrgID, &r.EventID, &r.Attempts); err != nil {
				return err
			}
			rows = append(rows, r)
		}
		return rs.Err()
	})
	return rows, err
}

// RunOnce claims and publishes one batch and returns how many rows were published.
func (p *Publisher) RunOnce(ctx context.Context) (int, error) {
	rows, err := p.claim(ctx)
	if err != nil {
		return 0, fmt.Errorf("claim outbox rows: %w", err)
	}
	published := 0
	for _, r := range rows {
		if ctx.Err() != nil {
			break // unpublished rows keep their lease and become due again
		}
		body, _ := json.Marshal(Envelope{OutboxID: r.ID, OrganizationID: r.OrgID, EventID: r.EventID})
		dup, err := p.conn.PublishAck(ctx, r.ID, body)
		if err != nil {
			p.m.PublishErrors.Inc()
			p.log.Warn("outbox publish failed", "error_class", "publish_error", "attempts", r.Attempts,
				"outbox_id", r.ID, "event_id", r.EventID, "organization_id", r.OrgID)
			p.backoff(ctx, r)
			continue
		}
		if dup {
			p.m.PublishDuplicate.Inc()
		}
		// Only now, after the acknowledgment, is the row marked published.
		q, qerr := p.pool.Querier()
		if qerr == nil {
			_, qerr = q.Exec(ctx, "UPDATE outbox SET published_at = $2 WHERE id = $1 AND published_at IS NULL", r.ID, p.now())
		}
		if qerr != nil {
			// The message is in the stream; the row will be republished after its lease and deduplicated.
			p.log.Error("mark outbox published failed", "error_class", "database_error", "outbox_id", r.ID, "event_id", r.EventID, "organization_id", r.OrgID)
			continue
		}
		p.m.Published.Inc()
		published++
	}
	return published, nil
}

// backoff makes a failed row due again after an exponentially growing delay (capped).
func (p *Publisher) backoff(ctx context.Context, r claimedRow) {
	d := time.Second << min(r.Attempts, 20)
	if d > p.cfg.MaxBackoff || d <= 0 {
		d = p.cfg.MaxBackoff
	}
	if q, err := p.pool.Querier(); err == nil {
		_, _ = q.Exec(ctx, "UPDATE outbox SET available_at = $2 WHERE id = $1 AND published_at IS NULL", r.ID, p.now().Add(d))
	}
}

// Run polls until ctx is cancelled.
func (p *Publisher) Run(ctx context.Context) error {
	t := time.NewTicker(p.cfg.PollInterval)
	defer t.Stop()
	for {
		p.beat.Beat()
		n, err := p.RunOnce(ctx)
		if err != nil && ctx.Err() == nil {
			p.m.PublishErrors.Inc()
			p.log.Error("outbox poll failed", "error_class", "database_error")
		}
		if n == p.cfg.BatchSize {
			continue // a full batch: more is probably waiting
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// ConsumerConfig bounds the fan-out consumer and the reconciliation sweep.
type ConsumerConfig struct {
	ReconcileInterval time.Duration
	Grace             time.Duration
	Lookback          time.Duration
	ReconcileLimit    int
}

// DefaultConsumerConfig returns the documented defaults.
func DefaultConsumerConfig() ConsumerConfig {
	return ConsumerConfig{ReconcileInterval: time.Minute, Grace: 2 * time.Minute, Lookback: 48 * time.Hour, ReconcileLimit: 500}
}

// Validate rejects unusable settings.
func (c ConsumerConfig) Validate() error {
	if c.ReconcileInterval <= 0 || c.Grace <= 0 || c.Lookback <= c.Grace || c.ReconcileLimit < 1 {
		return errors.New("invalid fan-out consumer configuration")
	}
	return nil
}

// FanOutConsumer turns accepted-event messages into deliveries.
type FanOutConsumer struct {
	pool *database.Pool
	cons *messaging.Consumer
	cfg  ConsumerConfig
	log  *slog.Logger
	m    *Metrics
	now  func() time.Time
	beat *observability.Heartbeat
}

// Check reports whether the consume-and-reconcile loop is making progress. One turn
// is a fetch (FetchWait) plus message handling, and reconciliation runs inside the same
// loop, so the limit leaves room for a sweep.
func (c *FanOutConsumer) Check(context.Context) error {
	return c.beat.Check(max(3*c.cfg.ReconcileInterval, 2*time.Minute))
}

// NewFanOutConsumer validates its inputs.
func NewFanOutConsumer(pool *database.Pool, cons *messaging.Consumer, cfg ConsumerConfig, log *slog.Logger, m *Metrics) (*FanOutConsumer, error) {
	if pool == nil || cons == nil || log == nil {
		return nil, errors.New("consumer: pool, consumer and logger are required")
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if m == nil {
		m = &Metrics{}
	}
	return &FanOutConsumer{pool: pool, cons: cons, cfg: cfg, log: log, m: m, now: func() time.Time { return time.Now().UTC() }, beat: observability.NewHeartbeat()}, nil
}

// RunOnce pulls one batch and processes it. It returns the number of messages handled.
func (c *FanOutConsumer) RunOnce(ctx context.Context) (int, error) {
	msgs, err := c.cons.Fetch(ctx)
	for _, m := range msgs {
		c.handle(ctx, m)
	}
	return len(msgs), err
}

func (c *FanOutConsumer) handle(ctx context.Context, m *messaging.Message) {
	c.m.FanOutMessages.Inc()
	var env Envelope
	if err := json.Unmarshal(m.Data, &env); err != nil || env.OrganizationID == "" || env.EventID == "" {
		c.m.PoisonMessages.Inc()
		c.log.Warn("malformed fan-out message terminated", "error_class", "poison_message", "delivered", m.Delivered)
		_ = m.Term() // retrying cannot fix a malformed message
		return
	}
	var created int64
	err := c.pool.InTx(ctx, func(tx pgx.Tx) error {
		var err error
		created, err = delivery.FanOut(ctx, tx, env.OrganizationID, env.EventID, c.now())
		return err
	})
	if err != nil {
		c.m.FanOutErrors.Inc()
		c.log.Warn("fan-out failed; message will be redelivered", "error_class", "fanout_error", "delivered", m.Delivered,
			"outbox_id", env.OutboxID, "event_id", env.EventID, "organization_id", env.OrganizationID)
		_ = m.Nak()
		return
	}
	// Acknowledge only after the transaction committed.
	if err := m.Ack(); err != nil {
		c.log.Warn("ack failed; message will be redelivered and fan-out is idempotent", "error_class", "ack_error",
			"outbox_id", env.OutboxID, "event_id", env.EventID, "organization_id", env.OrganizationID)
		return
	}
	for range created {
		c.m.FanOutCreated.Inc()
	}
}

// Reconcile runs one reconciliation sweep.
func (c *FanOutConsumer) Reconcile(ctx context.Context) (int64, error) {
	var n int64
	err := c.pool.InTx(ctx, func(tx pgx.Tx) error {
		var err error
		n, err = delivery.Reconcile(ctx, tx, c.now(), c.cfg.Grace, c.cfg.Lookback, c.cfg.ReconcileLimit)
		return err
	})
	if err != nil {
		c.m.ReconcileErrors.Inc()
		return 0, err
	}
	for range n {
		c.m.Reconciled.Inc()
	}
	return n, nil
}

// Run consumes and reconciles until ctx is cancelled.
func (c *FanOutConsumer) Run(ctx context.Context) error {
	next := time.Now()
	for ctx.Err() == nil {
		c.beat.Beat()
		if _, err := c.RunOnce(ctx); err != nil && ctx.Err() == nil {
			c.log.Warn("fan-out fetch failed", "error_class", "fetch_error")
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
			}
		}
		if !time.Now().Before(next) {
			if _, err := c.Reconcile(ctx); err != nil && ctx.Err() == nil {
				c.log.Warn("reconciliation failed", "error_class", "reconcile_error")
			}
			next = time.Now().Add(c.cfg.ReconcileInterval)
		}
	}
	return nil
}
