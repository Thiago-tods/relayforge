// Package messaging wraps NATS JetStream for RelayForge: stream and consumer
// configuration, acknowledged publishing and a bounded pull consumer.
//
// JetStream is a durable transport, not the record of truth. PostgreSQL decides
// what was accepted and what must be delivered; a message that is lost, expires or
// reaches its delivery limit is repaired by the reconciliation sweep (see
// internal/delivery). Everything here therefore assumes at-least-once delivery
// and duplicates.
package messaging

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Subject carries one message per accepted webhook event.
const Subject = "relayforge.events.accepted"

// Config describes the stream and the fan-out consumer. Zero values are not valid;
// use DefaultConfig.
type Config struct {
	URL    string
	Stream string
	// Consumer is the durable consumer name. Durable means its position survives restarts.
	Consumer string
	// Duplicates is JetStream's deduplication window for Nats-Msg-Id. A publisher
	// that crashed after the acknowledgment and republishes inside the window
	// does not create a second message; outside it, consumers deduplicate.
	Duplicates time.Duration
	// MaxAge bounds how long unconsumed messages are kept. Reconciliation recovers
	// anything that expires.
	MaxAge time.Duration
	// MaxDeliver bounds redelivery of one message. After it, JetStream stops
	// redelivering and reconciliation takes over.
	MaxDeliver int
	// AckWait is how long a consumer may take before the message is redelivered.
	AckWait time.Duration
	// BackOff are the redelivery delays after an acknowledgment timeout. The server
	// uses them instead of AckWait, so BackOff[0] is the effective time a handler
	// has before its message is redelivered: keep it well above the handler's duration.
	BackOff []time.Duration
	// FetchBatch and FetchWait bound one pull from the consumer.
	FetchBatch int
	FetchWait  time.Duration
}

// DefaultConfig returns the documented defaults.
func DefaultConfig(url string) Config {
	return Config{
		URL: url, Stream: "RELAYFORGE_EVENTS", Consumer: "delivery-fanout",
		Duplicates: 2 * time.Hour, MaxAge: 24 * time.Hour, MaxDeliver: 10, AckWait: 30 * time.Second,
		BackOff:    []time.Duration{30 * time.Second, time.Minute, 5 * time.Minute, 15 * time.Minute, 30 * time.Minute},
		FetchBatch: 16, FetchWait: 2 * time.Second,
	}
}

// Validate rejects settings the server would refuse or that would be unsafe.
func (c Config) Validate() error {
	switch {
	case c.URL == "":
		return errors.New("NATS URL is required")
	case c.Stream == "" || c.Consumer == "":
		return errors.New("stream and consumer names are required")
	case c.Duplicates <= 0 || c.MaxAge <= 0 || c.AckWait <= 0 || c.FetchWait <= 0:
		return errors.New("durations must be positive")
	case c.MaxDeliver < 1 || c.MaxDeliver > 100:
		return errors.New("max deliver must be between 1 and 100")
	case len(c.BackOff) > c.MaxDeliver:
		return errors.New("the backoff list must not be longer than max deliver")
	case c.FetchBatch < 1 || c.FetchBatch > 256:
		return errors.New("fetch batch must be between 1 and 256")
	}
	return nil
}

// Conn is a connection with its JetStream context.
type Conn struct {
	nc  *nats.Conn
	js  jetstream.JetStream
	cfg Config
}

// Connect opens a connection and verifies that JetStream is available.
func Connect(cfg Config, name string) (*Conn, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	nc, err := nats.Connect(cfg.URL, nats.Name(name), nats.MaxReconnects(-1), nats.ReconnectWait(time.Second), nats.Timeout(5*time.Second))
	if err != nil {
		return nil, errors.New("cannot connect to NATS") // the URL may carry credentials: never echo it
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("create JetStream context: %w", err)
	}
	return &Conn{nc: nc, js: js, cfg: cfg}, nil
}

// Close drains and closes the connection.
func (c *Conn) Close() {
	if c != nil && c.nc != nil {
		_ = c.nc.Drain()
	}
}

// EnsureStream creates the stream, or updates it to the configuration below. The
// call is idempotent and safe to run from every process at start-up.
//
//	storage      file (survives server restarts)
//	retention    limits, with MaxAge: messages are removed by age, not by consumption,
//	             so a consumer that is slow or down loses nothing until MaxAge
//	duplicates   deduplication window for Nats-Msg-Id
//	replicas     1 (a single local server; raise for a cluster)
//	deny delete  and deny purge: nothing removes history except MaxAge
func (c *Conn) EnsureStream(ctx context.Context) error {
	_, err := c.js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name: c.cfg.Stream, Subjects: []string{Subject}, Storage: jetstream.FileStorage, Retention: jetstream.LimitsPolicy,
		MaxAge: c.cfg.MaxAge, Duplicates: c.cfg.Duplicates, Replicas: 1, Discard: jetstream.DiscardOld,
		DenyDelete: true, DenyPurge: true, Description: "Accepted webhook events (PostgreSQL is the source of truth)",
	})
	if err != nil {
		return fmt.Errorf("ensure stream: %w", err)
	}
	return nil
}

// PublishAck publishes data with a stable message id and waits for the server's
// acknowledgment, so a nil return means JetStream stored the message. Publishing
// the same id inside the duplicate window is acknowledged without storing a second copy.
func (c *Conn) PublishAck(ctx context.Context, msgID string, data []byte) (duplicate bool, err error) {
	ack, err := c.js.PublishMsg(ctx, &nats.Msg{Subject: Subject, Data: data}, jetstream.WithMsgID(msgID))
	if err != nil {
		return false, fmt.Errorf("publish: %w", err)
	}
	return ack.Duplicate, nil
}

// Message is one message handed to a handler. Exactly one of Ack, Nak or Term must be called.
type Message struct {
	Data []byte
	// Delivered is how many times the server has delivered this message (1 on the first).
	Delivered uint64
	msg       jetstream.Msg
}

// Ack confirms the message was fully processed and its effects are durable.
func (m *Message) Ack() error { return m.msg.Ack() }

// Nak asks for redelivery after the consumer's backoff.
func (m *Message) Nak() error { return m.msg.Nak() }

// Term tells the server never to redeliver the message (a poison message).
func (m *Message) Term() error { return m.msg.Term() }

// Consumer pulls messages from the durable consumer.
type Consumer struct {
	conn *Conn
	cons jetstream.Consumer
}

// EnsureConsumer creates or updates the durable pull consumer: explicit
// acknowledgments, bounded redelivery with backoff, and all messages from the start.
func (c *Conn) EnsureConsumer(ctx context.Context) (*Consumer, error) {
	cons, err := c.js.CreateOrUpdateConsumer(ctx, c.cfg.Stream, jetstream.ConsumerConfig{
		Durable: c.cfg.Consumer, AckPolicy: jetstream.AckExplicitPolicy, DeliverPolicy: jetstream.DeliverAllPolicy,
		FilterSubject: Subject, AckWait: c.cfg.AckWait, MaxDeliver: c.cfg.MaxDeliver, BackOff: c.cfg.BackOff,
		MaxAckPending: c.cfg.FetchBatch * 4,
	})
	if err != nil {
		return nil, fmt.Errorf("ensure consumer: %w", err)
	}
	return &Consumer{conn: c, cons: cons}, nil
}

// Fetch pulls up to the configured batch, waiting at most FetchWait. It never
// holds more than FetchBatch messages, so memory use is bounded.
func (c *Consumer) Fetch(ctx context.Context) ([]*Message, error) {
	batch, err := c.cons.Fetch(c.conn.cfg.FetchBatch, jetstream.FetchMaxWait(c.conn.cfg.FetchWait))
	if err != nil {
		return nil, fmt.Errorf("fetch: %w", err)
	}
	var out []*Message
	for msg := range batch.Messages() {
		var delivered uint64 = 1
		if md, err := msg.Metadata(); err == nil {
			delivered = md.NumDelivered
		}
		out = append(out, &Message{Data: msg.Data(), Delivered: delivered, msg: msg})
	}
	if err := batch.Error(); err != nil && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, nats.ErrTimeout) && ctx.Err() == nil {
		return out, fmt.Errorf("fetch: %w", err)
	}
	return out, nil
}

// StreamInfo reports the stream's message count, for tests and monitoring.
func (c *Conn) StreamInfo(ctx context.Context) (*jetstream.StreamInfo, error) {
	s, err := c.js.Stream(ctx, c.cfg.Stream)
	if err != nil {
		return nil, err
	}
	return s.Info(ctx)
}

// ConsumerInfo reports the consumer's state.
func (c *Conn) ConsumerInfo(ctx context.Context) (*jetstream.ConsumerInfo, error) {
	s, err := c.js.Stream(ctx, c.cfg.Stream)
	if err != nil {
		return nil, err
	}
	cons, err := s.Consumer(ctx, c.cfg.Consumer)
	if err != nil {
		return nil, err
	}
	return cons.Info(ctx)
}
