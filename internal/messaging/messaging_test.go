package messaging_test

import (
	"context"
	"testing"
	"time"

	"github.com/Thiago-tods/relayforge/internal/messaging"
	"github.com/Thiago-tods/relayforge/internal/messaging/natstest"
)

func setup(t *testing.T) (*messaging.Conn, *messaging.Consumer, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	conn, err := messaging.Connect(natstest.Config(t), "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(conn.Close)
	if err := conn.EnsureStream(ctx); err != nil {
		t.Fatal(err)
	}
	cons, err := conn.EnsureConsumer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return conn, cons, ctx
}

func TestCheckReflectsTheConnection(t *testing.T) {
	conn, _, ctx := setup(t)
	if err := conn.Check(ctx); err != nil {
		t.Fatalf("a connected client must be ready: %v", err)
	}
	conn.Close()
	if err := conn.Check(ctx); err == nil {
		t.Fatal("a closed connection must not be ready")
	}
	var nilConn *messaging.Conn
	if err := nilConn.Check(ctx); err == nil {
		t.Fatal("a nil connection must not be ready")
	}
}

func TestConfigValidation(t *testing.T) {
	good := messaging.DefaultConfig("nats://127.0.0.1:4222")
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*messaging.Config){
		"no url":           func(c *messaging.Config) { c.URL = "" },
		"zero max deliver": func(c *messaging.Config) { c.MaxDeliver = 0 },
		"unbounded":        func(c *messaging.Config) { c.MaxDeliver = 1000 },
		"long backoff":     func(c *messaging.Config) { c.MaxDeliver = 2 },
		"zero ack wait":    func(c *messaging.Config) { c.AckWait = 0 },
		"huge batch":       func(c *messaging.Config) { c.FetchBatch = 100000 },
	} {
		c := good
		mutate(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestConnectErrorDoesNotEchoTheURL(t *testing.T) {
	cfg := messaging.DefaultConfig("nats://user:SECRETPASS@127.0.0.1:1")
	_, err := messaging.Connect(cfg, "test")
	if err == nil {
		t.Fatal("connected to a closed port")
	}
	if got := err.Error(); contains(got, "SECRETPASS") || contains(got, "127.0.0.1") {
		t.Fatalf("error leaks the URL: %q", got)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestEnsureIsIdempotent(t *testing.T) {
	conn, _, ctx := setup(t)
	if err := conn.EnsureStream(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.EnsureConsumer(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestPublishDeduplicatesByMessageID(t *testing.T) {
	conn, cons, ctx := setup(t)
	if dup, err := conn.PublishAck(ctx, "id-1", []byte("a")); err != nil || dup {
		t.Fatalf("first publish: dup=%v err=%v", dup, err)
	}
	if dup, err := conn.PublishAck(ctx, "id-1", []byte("a")); err != nil || !dup {
		t.Fatalf("second publish must be reported as a duplicate: dup=%v err=%v", dup, err)
	}
	info, err := conn.StreamInfo(ctx)
	if err != nil || info.State.Msgs != 1 {
		t.Fatalf("stream holds %v messages (%v), want 1", info.State.Msgs, err)
	}
	msgs, err := cons.Fetch(ctx)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("fetched %d (%v)", len(msgs), err)
	}
	_ = msgs[0].Ack()
}

func TestUnackedMessagesAreRedeliveredAndAckStopsIt(t *testing.T) {
	conn, cons, ctx := setup(t)
	if _, err := conn.PublishAck(ctx, "id-2", []byte("x")); err != nil {
		t.Fatal(err)
	}
	msgs, _ := cons.Fetch(ctx)
	if len(msgs) != 1 || msgs[0].Delivered != 1 {
		t.Fatalf("first fetch: %d messages", len(msgs))
	}
	_ = msgs[0].Nak()
	deadline := time.Now().Add(10 * time.Second)
	var again []*messaging.Message
	for time.Now().Before(deadline) && len(again) == 0 {
		again, _ = cons.Fetch(ctx)
	}
	if len(again) != 1 || again[0].Delivered != 2 {
		t.Fatalf("a nacked message must be redelivered with a higher count: %d", len(again))
	}
	_ = again[0].Ack()
	time.Sleep(500 * time.Millisecond)
	if more, _ := cons.Fetch(ctx); len(more) != 0 {
		t.Fatal("an acknowledged message was redelivered")
	}
}

func TestRedeliveryIsBounded(t *testing.T) {
	conn, cons, ctx := setup(t) // MaxDeliver is 4 in the test configuration
	if _, err := conn.PublishAck(ctx, "id-3", []byte("poison")); err != nil {
		t.Fatal(err)
	}
	deliveries := 0
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		msgs, _ := cons.Fetch(ctx)
		for _, m := range msgs {
			deliveries++
			_ = m.Nak()
		}
	}
	if deliveries != 4 {
		t.Fatalf("message delivered %d times, want exactly MaxDeliver (4)", deliveries)
	}
}

func TestTermStopsRedelivery(t *testing.T) {
	conn, cons, ctx := setup(t)
	if _, err := conn.PublishAck(ctx, "id-4", []byte("bad")); err != nil {
		t.Fatal(err)
	}
	msgs, _ := cons.Fetch(ctx)
	if len(msgs) != 1 {
		t.Fatal("no message")
	}
	_ = msgs[0].Term()
	time.Sleep(time.Second)
	if more, _ := cons.Fetch(ctx); len(more) != 0 {
		t.Fatal("a terminated message was redelivered")
	}
}

func TestFetchIsBoundedByTheBatchSize(t *testing.T) {
	conn, cons, ctx := setup(t) // batch of 8
	for i := 0; i < 20; i++ {
		if _, err := conn.PublishAck(ctx, "m-"+string(rune('a'+i)), []byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	msgs, _ := cons.Fetch(ctx)
	if len(msgs) > 8 || len(msgs) == 0 {
		t.Fatalf("fetched %d, want 1..8", len(msgs))
	}
	for _, m := range msgs {
		_ = m.Ack()
	}
}

func TestStreamIsProtectedFromDeletion(t *testing.T) {
	conn, _, ctx := setup(t)
	info, err := conn.StreamInfo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	c := info.Config
	if !c.DenyDelete || !c.DenyPurge || c.Duplicates <= 0 || c.MaxAge <= 0 {
		t.Fatalf("stream configuration is not hardened: %+v", c)
	}
}

// Regression for the CI failure "subjects overlap with an existing stream": tests
// share one server, so every stream needs its own subject.
func TestStreamsOnASharedServerNeedDistinctSubjects(t *testing.T) {
	url := natstest.URL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	open := func(cfg messaging.Config) *messaging.Conn {
		c, err := messaging.Connect(cfg, "test")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(c.Close)
		return c
	}
	a, b := natstest.ConfigAt(t, url), natstest.ConfigAt(t, url)
	if a.Subject == b.Subject {
		t.Fatal("test configurations must not share a subject")
	}
	if err := open(a).EnsureStream(ctx); err != nil {
		t.Fatal(err)
	}
	if err := open(b).EnsureStream(ctx); err != nil {
		t.Fatalf("a second stream with its own subject must be accepted: %v", err)
	}
	b.Stream, b.Subject = "OTHER_"+b.Stream, a.Subject
	if err := open(b).EnsureStream(ctx); err == nil {
		t.Fatal("the server accepted overlapping subjects; the isolation this test guards is not needed")
	}
}
