package database_test

import (
	"context"
	"strings"
	"testing"

	"github.com/Thiago-tods/relayforge/internal/app/apptest"
)

// ingestFixture holds two organizations, each with one endpoint, inserted with
// raw SQL so constraint violations can be provoked directly.
type ingestFixture struct {
	schemaFixture
	org2, user2, ep1, ep2 string
}

const (
	cipher = "'\\x" + "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f'"
	sha32  = "'\\x" + "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff'"
)

func newIngestFixture(t *testing.T) ingestFixture {
	t.Helper()
	f := ingestFixture{schemaFixture: newSchemaFixture(t)}
	q, _ := f.env.Pool.Querier()
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(q.QueryRow(ctx, "INSERT INTO organizations (name, slug) VALUES ('Org Two','org-two') RETURNING id").Scan(&f.org2))
	must(q.QueryRow(ctx, "INSERT INTO users (email, normalized_email, password_hash, display_name) VALUES ('u2@x.io','u2@x.io','h','U2') RETURNING id").Scan(&f.user2))
	ins := "INSERT INTO ingestion_endpoints (id, organization_id, public_id, name, signing_secret_ciphertext, encryption_key_version, created_by_user_id) " +
		"VALUES (gen_random_uuid(), $1, $2, 'ep', " + cipher + ", 1, $3) RETURNING id"
	must(q.QueryRow(ctx, ins, f.orgID, "ep_"+strings.Repeat("A", 22), f.userID).Scan(&f.ep1))
	must(q.QueryRow(ctx, ins, f.org2, "ep_"+strings.Repeat("B", 22), f.user2).Scan(&f.ep2))
	return f
}

func TestIngestionEndpointConstraints(t *testing.T) {
	f := newIngestFixture(t)
	ins := "INSERT INTO ingestion_endpoints (id, organization_id, public_id, name, status, signing_secret_ciphertext, encryption_key_version, created_by_user_id, revoked_at) " +
		"VALUES (gen_random_uuid(), $1, $2, $3, $4, %s, $5, $6, $7)"
	row := func(cipherLit string) string { return strings.Replace(ins, "%s", cipherLit, 1) }

	f.reject(t, "duplicate public id", row(cipher), f.orgID, "ep_"+strings.Repeat("A", 22), "n", "ACTIVE", 1, f.userID, nil)
	for _, bad := range []string{"x", "ep_short", "ep_" + strings.Repeat("A", 23), "ep_" + strings.Repeat("!", 22), "EP_" + strings.Repeat("C", 22), "ep_../../" + strings.Repeat("C", 13)} {
		f.reject(t, "public id "+bad, row(cipher), f.orgID, bad, "n", "ACTIVE", 1, f.userID, nil)
	}
	pub := "ep_" + strings.Repeat("C", 22)
	f.reject(t, "empty name", row(cipher), f.orgID, pub, "", "ACTIVE", 1, f.userID, nil)
	f.reject(t, "long name", row(cipher), f.orgID, pub, strings.Repeat("n", 101), "ACTIVE", 1, f.userID, nil)
	f.reject(t, "invalid status", row(cipher), f.orgID, pub, "n", "DELETED", 1, f.userID, nil)
	f.reject(t, "revoked without timestamp", row(cipher), f.orgID, pub, "n", "REVOKED", 1, f.userID, nil)
	f.reject(t, "active with revoked timestamp", row(cipher), f.orgID, pub, "n", "ACTIVE", 1, f.userID, "2026-01-01T00:00:00Z")
	f.reject(t, "zero key version", row(cipher), f.orgID, pub, "n", "ACTIVE", 0, f.userID, nil)
	f.reject(t, "too short ciphertext", row("'\\x0011'"), f.orgID, pub, "n", "ACTIVE", 1, f.userID, nil)
	f.reject(t, "unknown organization", row(cipher), "00000000-0000-4000-8000-000000000000", pub, "n", "ACTIVE", 1, f.userID, nil)
	f.reject(t, "unknown creator", row(cipher), f.orgID, pub, "n", "ACTIVE", 1, "00000000-0000-4000-8000-000000000000", nil)
	f.accept(t, "valid endpoint", row(cipher), f.orgID, pub, "n", "ACTIVE", 1, f.userID, nil)
	f.accept(t, "valid revoked endpoint", row(cipher), f.orgID, "ep_"+strings.Repeat("D", 22), "n", "REVOKED", 1, f.userID, "2026-01-01T00:00:00Z")
}

func (f ingestFixture) insertEvent() string {
	return "INSERT INTO webhook_events (id, organization_id, ingestion_endpoint_id, idempotency_key, payload, payload_sha256, content_type) " +
		"VALUES (gen_random_uuid(), $1, $2, $3, $4, " + sha32 + ", 'application/json')"
}

func TestWebhookEventConstraints(t *testing.T) {
	f := newIngestFixture(t)
	ins := f.insertEvent()
	f.accept(t, "valid event", ins, f.orgID, f.ep1, "key-1", []byte(`{}`))
	f.reject(t, "duplicate idempotency key on the same endpoint", ins, f.orgID, f.ep1, "key-1", []byte(`{"x":1}`))
	f.accept(t, "same key on another endpoint", ins, f.org2, f.ep2, "key-1", []byte(`{}`))
	// Tenant consistency is relational: an event cannot claim another organization's endpoint.
	f.reject(t, "endpoint of another organization", ins, f.org2, f.ep1, "key-2", []byte(`{}`))
	f.reject(t, "unknown endpoint", ins, f.orgID, "00000000-0000-4000-8000-000000000000", "key-3", []byte(`{}`))
	f.reject(t, "empty key", ins, f.orgID, f.ep1, "", []byte(`{}`))
	f.reject(t, "key with whitespace", ins, f.orgID, f.ep1, "a b", []byte(`{}`))
	f.reject(t, "key with control char", ins, f.orgID, f.ep1, "a\tb", []byte(`{}`))
	f.reject(t, "overlong key", ins, f.orgID, f.ep1, strings.Repeat("k", 256), []byte(`{}`))
	f.reject(t, "empty payload", ins, f.orgID, f.ep1, "key-4", []byte{})
	f.reject(t, "payload over 1 MiB", ins, f.orgID, f.ep1, "key-5", make([]byte, 1<<20+1))
	f.accept(t, "payload of exactly 1 MiB", ins, f.orgID, f.ep1, "key-6", append([]byte(`"`), append(make([]byte, 1<<20-2), '"')...))
	f.reject(t, "bad hash length", "INSERT INTO webhook_events (id, organization_id, ingestion_endpoint_id, idempotency_key, payload, payload_sha256, content_type) "+
		"VALUES (gen_random_uuid(), $1, $2, 'k9', '{}', '\\x0011', 'application/json')", f.orgID, f.ep1)
	f.reject(t, "bad status", "UPDATE webhook_events SET status = 'DONE'")
	f.reject(t, "deleting an endpoint that has events", "DELETE FROM ingestion_endpoints WHERE id = $1", f.ep1)
}

func TestOutboxConstraints(t *testing.T) {
	f := newIngestFixture(t)
	var event string
	q, _ := f.env.Pool.Querier()
	if err := q.QueryRow(context.Background(), f.insertEvent()+" RETURNING id", f.orgID, f.ep1, "k", []byte(`{}`)).Scan(&event); err != nil {
		t.Fatal(err)
	}
	ins := "INSERT INTO outbox (organization_id, event_id, message_type, payload) VALUES ($1, $2, 'webhook_event.accepted', $3)"
	f.reject(t, "event of another organization", ins, f.org2, event, `{}`)
	f.reject(t, "unknown event", ins, f.orgID, "00000000-0000-4000-8000-000000000000", `{}`)
	f.reject(t, "non-object payload", ins, f.orgID, event, `[1]`)
	f.accept(t, "valid outbox record", ins, f.orgID, event, `{"schema":1}`)
	f.reject(t, "second outbox record for one event", ins, f.orgID, event, `{}`)
	f.reject(t, "negative attempts", "UPDATE outbox SET attempts = -1")
	f.reject(t, "deleting an event that has an outbox record", "DELETE FROM webhook_events WHERE id = $1", event)

	// Defaults: unpublished, no attempts, available now.
	if n := f.env.Count(t, "SELECT count(*) FROM outbox WHERE published_at IS NULL AND attempts = 0 AND available_at <= now()"); n != 1 {
		t.Fatal("a new outbox record must be unpublished, untried and immediately available")
	}
	// The publisher index covers only unpublished rows.
	if n := f.env.Count(t, "SELECT count(*) FROM pg_indexes WHERE indexname = 'outbox_unpublished_idx' AND indexdef LIKE '%published_at IS NULL%'"); n != 1 {
		t.Fatal("the partial index for unpublished outbox rows is missing")
	}
}

func TestRateLimitCounterConstraints(t *testing.T) {
	f := newIngestFixture(t)
	f.accept(t, "valid counter", "INSERT INTO rate_limit_counters (bucket, window_start, count) VALUES ('b', now(), 1)")
	f.reject(t, "duplicate window", "INSERT INTO rate_limit_counters (bucket, window_start, count) VALUES ('b', (SELECT window_start FROM rate_limit_counters LIMIT 1), 1)")
	f.reject(t, "negative count", "INSERT INTO rate_limit_counters (bucket, window_start, count) VALUES ('c', now(), -1)")
	f.reject(t, "empty bucket", "INSERT INTO rate_limit_counters (bucket, window_start, count) VALUES ('', now(), 1)")
}

func TestIngestionMigrationsAreTheOnlyNewObjects(t *testing.T) {
	env := apptest.New(t)
	for _, table := range []string{"ingestion_endpoints", "webhook_events", "outbox", "rate_limit_counters"} {
		if env.Count(t, "SELECT count(*) FROM information_schema.tables WHERE table_name = $1", table) != 1 {
			t.Errorf("table %s missing", table)
		}
	}
	if n := env.Count(t, "SELECT count(*) FROM pg_trigger WHERE tgname = 'ingestion_endpoints_set_updated_at'"); n != 1 {
		t.Error("updated_at trigger missing on ingestion_endpoints")
	}
}
