package relay_test

import (
	"context"
	crand "crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Thiago-tods/relayforge/internal/app/apptest"
	"github.com/Thiago-tods/relayforge/internal/delivery"
	"github.com/Thiago-tods/relayforge/internal/messaging"
	"github.com/Thiago-tods/relayforge/internal/messaging/natstest"
	"github.com/Thiago-tods/relayforge/internal/observability"
	"github.com/Thiago-tods/relayforge/internal/relay"
)

var bg = context.Background()

type fixture struct {
	env  *apptest.Env
	user apptest.User
	pub  string
	sec  string
	ep   string
	conn *messaging.Conn
	cons *messaging.Consumer
	seq  atomic.Int64
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	env := apptest.New(t)
	u := env.Register(t, "owner@example.com")
	ep := env.CreateEndpoint(t, u)
	conn, err := messaging.Connect(natstest.Config(t), "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(conn.Close)
	if err := conn.EnsureStream(bg); err != nil {
		t.Fatal(err)
	}
	cons, err := conn.EnsureConsumer(bg)
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{env: env, user: u, pub: ep.Endpoint.PublicID, sec: ep.SigningSecret, ep: ep.Endpoint.ID, conn: conn, cons: cons}
}

func (f *fixture) event(t *testing.T) string {
	t.Helper()
	res, err := f.env.Send(f.pub, f.env.Signed(f.sec, []byte(`{"a":1}`), fmt.Sprintf("k-%d", f.seq.Add(1))))
	if err != nil {
		t.Fatal(err)
	}
	return res.EventID
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func (f *fixture) publisher(t *testing.T, m *relay.Metrics) *relay.Publisher {
	t.Helper()
	cfg := relay.DefaultPublisherConfig()
	cfg.BatchSize = 10
	p, err := relay.NewPublisher(f.env.Pool, f.conn, cfg, quiet(), m)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func (f *fixture) consumer(t *testing.T, m *relay.Metrics) *relay.FanOutConsumer {
	t.Helper()
	c, err := relay.NewFanOutConsumer(f.env.Pool, f.cons, relay.DefaultConsumerConfig(), quiet(), m)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// subscribe creates a destination (secret sealed as in production) and subscribes
// it to the endpoint, back-dated so events ingested a moment ago are covered.
func (f *fixture) subscribe(t *testing.T) string {
	t.Helper()
	id := newUUID(t)
	secret, err := delivery.GenerateDestinationSecret()
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := f.env.Box.Seal([]byte(secret), delivery.DestinationSecretAAD(id))
	if err != nil {
		t.Fatal(err)
	}
	f.env.Exec(t, `INSERT INTO destinations (id, organization_id, name, url, signing_secret_ciphertext, encryption_key_version, created_by_user_id)
		VALUES ($1, $2, 'd', 'https://hooks.example.com/x', $3, $4, $5)`, id, f.user.Org.ID, sealed, f.env.Box.Version(), f.user.Principal.User.ID)
	f.env.Exec(t, `INSERT INTO subscriptions (organization_id, ingestion_endpoint_id, destination_id, created_by_user_id, created_at)
		VALUES ($1, $2, $3, $4, now() - interval '1 hour')`, f.user.Org.ID, f.ep, id, f.user.Principal.User.ID)
	return id
}

func TestPublisherMarksPublishedOnlyAfterTheAcknowledgment(t *testing.T) {
	f := newFixture(t)
	ev := f.event(t)
	m := relay.NewMetrics(observability.NewRegistry())

	n, err := f.publisher(t, m).RunOnce(bg)
	if err != nil || n != 1 {
		t.Fatalf("published %d (%v)", n, err)
	}
	if f.env.Count(t, "SELECT count(*) FROM outbox WHERE event_id = $1 AND published_at IS NOT NULL", ev) != 1 {
		t.Fatal("row not marked published")
	}
	info, _ := f.conn.StreamInfo(bg)
	if info.State.Msgs != 1 {
		t.Fatalf("stream holds %d messages", info.State.Msgs)
	}
	if m.Published.Value() != 1 {
		t.Fatal("metric not updated")
	}
	if n, _ := f.publisher(t, m).RunOnce(bg); n != 0 {
		t.Fatal("a published row was published again")
	}
}

func TestPublishFailureLeavesTheRowUnpublishedAndBacksOff(t *testing.T) {
	f := newFixture(t)
	ev := f.event(t)
	m := relay.NewMetrics(observability.NewRegistry())
	// A publisher whose context is fine but whose broker rejects: publish to a stream that does not exist.
	cfg := natstest.Config(t)
	cfg.Stream = "MISSING"
	cfg.URL = natstest.URL(t)
	broken, err := messaging.Connect(cfg, "broken")
	if err != nil {
		t.Fatal(err)
	}
	defer broken.Close()
	p, err := relay.NewPublisher(f.env.Pool, broken, relay.DefaultPublisherConfig(), quiet(), m)
	if err != nil {
		t.Fatal(err)
	}
	pctx, cancel := context.WithTimeout(bg, 3*time.Second)
	defer cancel()
	n, err := p.RunOnce(pctx)
	if err != nil || n != 0 {
		t.Fatalf("published %d (%v), want 0", n, err)
	}
	if f.env.Count(t, "SELECT count(*) FROM outbox WHERE event_id = $1 AND published_at IS NULL AND attempts = 1 AND available_at > now()", ev) != 1 {
		t.Fatal("the row must stay unpublished, count the attempt and be delayed")
	}
	if m.PublishErrors.Value() != 1 {
		t.Fatalf("publish errors metric = %d", m.PublishErrors.Value())
	}
}

func TestCrashAfterPublishRepublishesWithoutDuplicatingTheMessage(t *testing.T) {
	f := newFixture(t)
	ev := f.event(t)
	var outboxID string
	q, _ := f.env.Pool.Querier()
	if err := q.QueryRow(bg, "SELECT id::text FROM outbox WHERE event_id = $1", ev).Scan(&outboxID); err != nil {
		t.Fatal(err)
	}
	// The message reached JetStream; the process died before published_at was set.
	body, _ := json.Marshal(relay.Envelope{OutboxID: outboxID, OrganizationID: f.user.Org.ID, EventID: ev})
	if _, err := f.conn.PublishAck(bg, outboxID, body); err != nil {
		t.Fatal(err)
	}
	m := relay.NewMetrics(observability.NewRegistry())
	if n, err := f.publisher(t, m).RunOnce(bg); err != nil || n != 1 {
		t.Fatalf("republish: %d %v", n, err)
	}
	info, _ := f.conn.StreamInfo(bg)
	if info.State.Msgs != 1 {
		t.Fatalf("stream holds %d copies, want 1", info.State.Msgs)
	}
	if m.PublishDuplicate.Value() != 1 {
		t.Fatal("the duplicate was not detected")
	}
}

func TestConcurrentPublishersNeverClaimTheSameRow(t *testing.T) {
	f := newFixture(t)
	for i := 0; i < 6; i++ {
		f.event(t)
	}
	var total atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n, err := f.publisher(t, nil).RunOnce(bg)
			if err != nil {
				t.Error(err)
			}
			total.Add(int64(n))
		}()
	}
	wg.Wait()
	if total.Load() != 6 {
		t.Fatalf("published %d rows in total, want 6", total.Load())
	}
	info, _ := f.conn.StreamInfo(bg)
	if info.State.Msgs != 6 {
		t.Fatalf("stream holds %d messages, want 6", info.State.Msgs)
	}
}

func TestEndToEndOutboxToDelivery(t *testing.T) {
	f := newFixture(t)
	dest := f.subscribe(t)
	ev := f.event(t)
	m := relay.NewMetrics(observability.NewRegistry())
	if _, err := f.publisher(t, m).RunOnce(bg); err != nil {
		t.Fatal(err)
	}
	c := f.consumer(t, m)
	if n, err := c.RunOnce(bg); err != nil || n != 1 {
		t.Fatalf("consumed %d (%v)", n, err)
	}
	if f.env.Count(t, "SELECT count(*) FROM deliveries WHERE event_id = $1 AND destination_id = $2 AND status = 'PENDING'", ev, dest) != 1 {
		t.Fatal("no delivery created")
	}
	if m.FanOutCreated.Value() != 1 {
		t.Fatal("metric not updated")
	}
	time.Sleep(300 * time.Millisecond)
	if n, _ := c.RunOnce(bg); n != 0 {
		t.Fatal("an acknowledged message was delivered again")
	}
}

func TestDuplicateMessagesCreateOneDelivery(t *testing.T) {
	f := newFixture(t)
	f.subscribe(t)
	ev := f.event(t)
	body, _ := json.Marshal(relay.Envelope{OutboxID: "x", OrganizationID: f.user.Org.ID, EventID: ev})
	for _, id := range []string{"dup-a", "dup-b", "dup-c"} { // distinct ids defeat broker dedup on purpose
		if _, err := f.conn.PublishAck(bg, id, body); err != nil {
			t.Fatal(err)
		}
	}
	c := f.consumer(t, nil)
	for i := 0; i < 3; i++ {
		if _, err := c.RunOnce(bg); err != nil {
			t.Fatal(err)
		}
	}
	if got := f.env.Count(t, "SELECT count(*) FROM deliveries WHERE event_id = $1", ev); got != 1 {
		t.Fatalf("%d deliveries for one event, want 1", got)
	}
}

func TestMalformedMessagesAreTerminatedNotRetriedForever(t *testing.T) {
	f := newFixture(t)
	for i, body := range []string{"not json", `{"organization_id":"","event_id":""}`} {
		if _, err := f.conn.PublishAck(bg, fmt.Sprintf("bad-%d", i), []byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	m := relay.NewMetrics(observability.NewRegistry())
	c := f.consumer(t, m)
	if _, err := c.RunOnce(bg); err != nil {
		t.Fatal(err)
	}
	if m.PoisonMessages.Value() != 2 {
		t.Fatalf("poison = %d, want 2", m.PoisonMessages.Value())
	}
	time.Sleep(500 * time.Millisecond)
	if n, _ := c.RunOnce(bg); n != 0 {
		t.Fatal("a terminated message came back")
	}
}

func TestMessageForAnotherOrganizationCreatesNothing(t *testing.T) {
	f := newFixture(t)
	f.subscribe(t)
	ev := f.event(t)
	other := f.env.Register(t, "other@example.com")
	body, _ := json.Marshal(relay.Envelope{OutboxID: "x", OrganizationID: other.Org.ID, EventID: ev})
	if _, err := f.conn.PublishAck(bg, "cross", body); err != nil {
		t.Fatal(err)
	}
	if _, err := f.consumer(t, nil).RunOnce(bg); err != nil {
		t.Fatal(err)
	}
	if f.env.Count(t, "SELECT count(*) FROM deliveries") != 0 {
		t.Fatal("a cross-tenant message created a delivery")
	}
}

func TestReconciliationRepairsALostMessage(t *testing.T) {
	f := newFixture(t)
	f.subscribe(t)
	ev := f.event(t) // never published: the broker "lost" it
	f.env.Exec(t, "UPDATE webhook_events SET received_at = now() - interval '10 minutes'")
	m := relay.NewMetrics(observability.NewRegistry())
	n, err := f.consumer(t, m).Reconcile(bg)
	if err != nil || n != 1 {
		t.Fatalf("reconciled %d (%v)", n, err)
	}
	if f.env.Count(t, "SELECT count(*) FROM deliveries WHERE event_id = $1", ev) != 1 || m.Reconciled.Value() != 1 {
		t.Fatal("delivery was not created")
	}
}

func TestRunLoopsStopOnCancellation(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(bg)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = f.publisher(t, nil).Run(ctx) }()
	go func() { defer wg.Done(); _ = f.consumer(t, nil).Run(ctx) }()
	time.Sleep(300 * time.Millisecond)
	cancel()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("run loops did not stop")
	}
}

func TestConfigValidation(t *testing.T) {
	if err := (relay.PublisherConfig{}).Validate(); err == nil {
		t.Error("zero publisher config accepted")
	}
	if err := (relay.ConsumerConfig{}).Validate(); err == nil {
		t.Error("zero consumer config accepted")
	}
	if err := relay.DefaultPublisherConfig().Validate(); err != nil {
		t.Error(err)
	}
	if err := relay.DefaultConsumerConfig().Validate(); err != nil {
		t.Error(err)
	}
}

func newUUID(t *testing.T) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := crand.Read(b); err != nil {
		t.Fatal(err)
	}
	b[6], b[8] = (b[6]&0x0f)|0x40, (b[8]&0x3f)|0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
