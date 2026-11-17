package ingest_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Thiago-tods/relayforge/internal/app"
	"github.com/Thiago-tods/relayforge/internal/app/apptest"
	"github.com/Thiago-tods/relayforge/internal/ingest"
	"github.com/Thiago-tods/relayforge/internal/platform/apperror"
)

var bg = context.Background()

type fixture struct {
	env      *apptest.Env
	owner    apptest.User
	endpoint app.EndpointWithSecret
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	env := apptest.New(t)
	owner := env.Register(t, "owner@example.com")
	return fixture{env: env, owner: owner, endpoint: env.CreateEndpoint(t, owner)}
}

func (f fixture) send(req ingest.Request) (ingest.Result, error) {
	return f.env.Send(f.endpoint.Endpoint.PublicID, req)
}

func (f fixture) events(t *testing.T) int {
	return f.env.Count(t, "SELECT count(*) FROM webhook_events")
}
func (f fixture) outbox(t *testing.T) int { return f.env.Count(t, "SELECT count(*) FROM outbox") }

func requireAppError(t *testing.T, err error, want *apperror.Error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("got %v, want %s (%d)", err, want.Code, want.Status)
	}
}

func TestEndpointSecretIsEncryptedAtRest(t *testing.T) {
	f := newFixture(t)
	secret := f.endpoint.SigningSecret
	if !strings.HasPrefix(secret, "rfs_") || len(secret) < 40 {
		t.Fatalf("unexpected secret shape: %q", secret[:4])
	}
	if n := f.env.Count(t, "SELECT count(*) FROM ingestion_endpoints t WHERE t::text LIKE '%' || $1 || '%'", secret); n != 0 {
		t.Fatal("the raw signing secret is stored in PostgreSQL")
	}
	if n := f.env.Count(t, "SELECT count(*) FROM audit_events t WHERE t::text LIKE '%' || $1 || '%'", secret); n != 0 {
		t.Fatal("the raw signing secret appears in the audit log")
	}

	ep, err := f.env.Ingest.Lookup(bg, f.endpoint.Endpoint.PublicID, "")
	if err != nil {
		t.Fatal(err)
	}
	plain, err := f.env.Box.Open(ep.SigningSecretCiphertext, ingest.SecretAAD(ep.ID), ep.EncryptionKeyVersion)
	if err != nil || string(plain) != secret {
		t.Fatalf("stored ciphertext does not decrypt to the secret: %v", err)
	}
	if ep.EncryptionKeyVersion != 1 || bytes.Contains(ep.SigningSecretCiphertext, []byte(secret)) {
		t.Fatal("ciphertext must record the key version and must not contain the secret")
	}
}

func TestCiphertextIsBoundToItsEndpoint(t *testing.T) {
	f := newFixture(t)
	other := f.env.CreateEndpoint(t, f.owner)
	// Copy endpoint A's ciphertext onto endpoint B: it must not decrypt there.
	f.env.Exec(t, `UPDATE ingestion_endpoints SET signing_secret_ciphertext =
		(SELECT signing_secret_ciphertext FROM ingestion_endpoints WHERE public_id = $1) WHERE public_id = $2`,
		f.endpoint.Endpoint.PublicID, other.Endpoint.PublicID)
	_, err := f.env.Send(other.Endpoint.PublicID, f.env.Signed(f.endpoint.SigningSecret, []byte(`{}`), "k1"))
	var ae *apperror.Error
	if err == nil || errors.As(err, &ae) {
		t.Fatalf("a transplanted ciphertext must be a server error, not a client error: %v", err)
	}
	if f.events(t) != 0 {
		t.Fatal("event created from a transplanted ciphertext")
	}
}

func TestAcceptPersistsExactPayloadAndOutboxAtomically(t *testing.T) {
	f := newFixture(t)
	body := []byte("{ \"order\" : 42,\n  \"items\":[1,2 , 3] }") // odd spacing must survive byte for byte
	res, err := f.send(f.env.Signed(f.endpoint.SigningSecret, body, "order-42"))
	if err != nil || res.Duplicate || res.EventID == "" {
		t.Fatalf("accept: %+v %v", res, err)
	}

	q, _ := f.env.Pool.Querier()
	var stored, sha []byte
	var ct, status string
	if err := q.QueryRow(bg, "SELECT payload, payload_sha256, content_type, status FROM webhook_events WHERE id = $1", res.EventID).
		Scan(&stored, &sha, &ct, &status); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored, body) || !bytes.Equal(sha, ingest.PayloadHash(body)) || ct != "application/json" || status != "RECEIVED" {
		t.Fatalf("stored event differs from the request: %q %x %s %s", stored, sha, ct, status)
	}

	var published *time.Time
	var attempts int
	var msgType, envelope string
	if err := q.QueryRow(bg, "SELECT published_at, attempts, message_type, payload::text FROM outbox WHERE event_id = $1", res.EventID).
		Scan(&published, &attempts, &msgType, &envelope); err != nil {
		t.Fatal(err)
	}
	if published != nil || attempts != 0 || msgType != "webhook_event.accepted" {
		t.Fatalf("outbox record must be unpublished: %v %d %s", published, attempts, msgType)
	}
	if strings.Contains(envelope, "order") || !strings.Contains(envelope, res.EventID) {
		t.Fatalf("outbox envelope must reference the event without copying the payload: %s", envelope)
	}
	if f.env.Metrics == nil {
		t.Fatal("metrics registry missing")
	}
}

func TestAcceptedEventUsesTheSignedBytesNotANormalizedForm(t *testing.T) {
	f := newFixture(t)
	body := []byte(`{"b":1,"a":2}`)
	req := f.env.Signed(f.endpoint.SigningSecret, body, "k")
	// Re-serialized JSON is semantically equal but not the signed bytes.
	req2 := req
	req2.Body = []byte(`{"a":2,"b":1}`)
	requireAppError(t, func() error { _, err := f.send(req2); return err }(), ingest.ErrSignatureMismatch)
	if _, err := f.send(req); err != nil {
		t.Fatal(err)
	}
}

func TestRejectedRequestsLeaveNothingBehind(t *testing.T) {
	f := newFixture(t)
	good := f.env.Signed(f.endpoint.SigningSecret, []byte(`{"ok":true}`), "key-1")
	mutate := func(fn func(*ingest.Request)) ingest.Request { r := good; fn(&r); return r }
	now := f.env.Clock.Now().Unix()
	signAt := func(secret string, ts int64, body []byte) ingest.Request {
		s := formatInt(ts)
		return ingest.Request{ContentType: "application/json", Timestamp: s, Signature: ingest.Sign(secret, s, body), IdempotencyKey: "key-1", Body: body}
	}

	cases := []struct {
		name string
		req  ingest.Request
		want *apperror.Error
	}{
		{"wrong secret", f.env.Signed("rfs_not-the-secret", []byte(`{"ok":true}`), "key-1"), ingest.ErrSignatureMismatch},
		{"tampered body", mutate(func(r *ingest.Request) { r.Body = []byte(`{"ok":false}`) }), ingest.ErrSignatureMismatch},
		{"missing signature", mutate(func(r *ingest.Request) { r.Signature = "" }), ingest.ErrInvalidSignature},
		{"malformed signature", mutate(func(r *ingest.Request) { r.Signature = "sha256=" + r.Signature }), ingest.ErrInvalidSignature},
		{"uppercase signature", mutate(func(r *ingest.Request) { r.Signature = strings.ToUpper(r.Signature) }), ingest.ErrInvalidSignature},
		{"missing timestamp", mutate(func(r *ingest.Request) { r.Timestamp = "" }), ingest.ErrInvalidTimestamp},
		{"malformed timestamp", mutate(func(r *ingest.Request) { r.Timestamp = "yesterday" }), ingest.ErrInvalidTimestamp},
		{"expired timestamp", signAt(f.endpoint.SigningSecret, now-301, []byte(`{"ok":true}`)), ingest.ErrTimestampWindow},
		{"future timestamp", signAt(f.endpoint.SigningSecret, now+301, []byte(`{"ok":true}`)), ingest.ErrTimestampWindow},
		{"missing idempotency key", mutate(func(r *ingest.Request) { r.IdempotencyKey = "" }), ingest.ErrInvalidIdempotency},
		{"idempotency key with space", mutate(func(r *ingest.Request) { r.IdempotencyKey = "a b" }), ingest.ErrInvalidIdempotency},
		{"wrong content type", mutate(func(r *ingest.Request) { r.ContentType = "text/plain" }), ingest.ErrUnsupportedMedia},
		{"missing content type", mutate(func(r *ingest.Request) { r.ContentType = "" }), ingest.ErrUnsupportedMedia},
		{"invalid json", mutate(func(r *ingest.Request) { r.Body = []byte(`{"ok":`) }), ingest.ErrInvalidJSON},
		{"empty body", mutate(func(r *ingest.Request) { r.Body = nil }), ingest.ErrInvalidJSON},
	}
	for _, c := range cases {
		if _, err := f.send(c.req); !errors.Is(err, c.want) {
			t.Errorf("%s: got %v, want %s", c.name, err, c.want.Code)
		}
	}
	if f.events(t) != 0 || f.outbox(t) != 0 {
		t.Fatalf("rejected requests persisted data: %d events, %d outbox", f.events(t), f.outbox(t))
	}
	// The unmodified request still works afterwards.
	if _, err := f.send(good); err != nil {
		t.Fatal(err)
	}
}

func TestTimestampWindowBoundaries(t *testing.T) {
	f := newFixture(t)
	now := f.env.Clock.Now().Unix()
	for i, c := range []struct {
		offset int64
		ok     bool
	}{{-300, true}, {300, true}, {-301, false}, {301, false}, {0, true}} {
		body := []byte(`{"n":1}`)
		ts := formatInt(now + c.offset)
		req := ingest.Request{ContentType: "application/json", Timestamp: ts, Signature: ingest.Sign(f.endpoint.SigningSecret, ts, body),
			IdempotencyKey: "boundary-" + formatInt(int64(i)), Body: body}
		_, err := f.send(req)
		if (err == nil) != c.ok {
			t.Errorf("offset %d: err=%v, want ok=%v", c.offset, err, c.ok)
		}
	}
}

func TestUnknownPausedAndRevokedEndpoints(t *testing.T) {
	f := newFixture(t)
	req := f.env.Signed(f.endpoint.SigningSecret, []byte(`{}`), "k")
	orgID, epID, pub := f.owner.Org.ID, f.endpoint.Endpoint.ID, f.endpoint.Endpoint.PublicID

	if _, err := f.env.Send("ep_AAAAAAAAAAAAAAAAAAAAAA", req); !errors.Is(err, ingest.ErrNotFound) {
		t.Fatalf("unknown endpoint: %v", err)
	}
	for _, bad := range []string{"", "nope", "../../etc/passwd", strings.Repeat("a", 300), pub + "x"} {
		if _, err := f.env.Send(bad, req); !errors.Is(err, ingest.ErrNotFound) {
			t.Errorf("identifier %q: %v", bad, err)
		}
	}

	if _, err := f.env.Svc.PauseEndpoint(bg, f.owner.Principal, orgID, epID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.send(req); !errors.Is(err, ingest.ErrPaused) {
		t.Fatalf("paused endpoint accepted an event: %v", err)
	}
	if _, err := f.env.Svc.ActivateEndpoint(bg, f.owner.Principal, orgID, epID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.send(req); err != nil {
		t.Fatalf("activated endpoint must accept events: %v", err)
	}

	if _, err := f.env.Svc.RevokeEndpoint(bg, f.owner.Principal, orgID, epID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.send(f.env.Signed(f.endpoint.SigningSecret, []byte(`{}`), "k2")); !errors.Is(err, ingest.ErrNotFound) {
		t.Fatalf("revoked endpoint must look unknown: %v", err)
	}
	if _, err := f.env.Svc.ActivateEndpoint(bg, f.owner.Principal, orgID, epID); !errors.Is(err, app.ErrEndpointRevoked) {
		t.Fatalf("a revoked endpoint must not be reactivated: %v", err)
	}
	if f.events(t) != 1 {
		t.Fatalf("events = %d, want only the one accepted while active", f.events(t))
	}
}

func TestIdempotency(t *testing.T) {
	f := newFixture(t)
	body := []byte(`{"amount": 10}`)
	first, err := f.send(f.env.Signed(f.endpoint.SigningSecret, body, "pay-1"))
	if err != nil || first.Duplicate {
		t.Fatalf("first: %+v %v", first, err)
	}

	again, err := f.send(f.env.Signed(f.endpoint.SigningSecret, body, "pay-1"))
	if err != nil || !again.Duplicate || again.EventID != first.EventID {
		t.Fatalf("replay must return the original event: %+v %v", again, err)
	}
	if f.events(t) != 1 || f.outbox(t) != 1 {
		t.Fatalf("replay created data: %d events, %d outbox", f.events(t), f.outbox(t))
	}

	_, err = f.send(f.env.Signed(f.endpoint.SigningSecret, []byte(`{"amount": 11}`), "pay-1"))
	requireAppError(t, err, ingest.ErrIdempotencyConflict)
	if f.events(t) != 1 || f.outbox(t) != 1 {
		t.Fatal("a conflicting request must not persist anything")
	}

	// Keys are case-sensitive and scoped per endpoint.
	if res, err := f.send(f.env.Signed(f.endpoint.SigningSecret, body, "PAY-1")); err != nil || res.Duplicate {
		t.Fatalf("keys are case sensitive: %+v %v", res, err)
	}
	other := f.env.CreateEndpoint(t, f.owner)
	res, err := f.env.Send(other.Endpoint.PublicID, f.env.Signed(other.SigningSecret, body, "pay-1"))
	if err != nil || res.Duplicate || res.EventID == first.EventID {
		t.Fatalf("the same key on another endpoint is independent: %+v %v", res, err)
	}
	if f.events(t) != 3 || f.outbox(t) != 3 {
		t.Fatalf("expected 3 events and 3 outbox records, got %d and %d", f.events(t), f.outbox(t))
	}
}

func TestConcurrentDuplicatesCreateExactlyOneEventAndOutboxRecord(t *testing.T) {
	f := newFixture(t)
	body := []byte(`{"race":true}`)
	const n = 24
	results := make([]ingest.Result, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results[i], errs[i] = f.send(f.env.Signed(f.endpoint.SigningSecret, body, "same-key"))
		}()
	}
	close(start)
	wg.Wait()

	fresh := 0
	ids := map[string]bool{}
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("request %d failed: %v", i, errs[i])
		}
		ids[results[i].EventID] = true
		if !results[i].Duplicate {
			fresh++
		}
	}
	if fresh != 1 || len(ids) != 1 {
		t.Fatalf("%d requests created an event and %d distinct ids were returned; want exactly 1 and 1", fresh, len(ids))
	}
	if f.events(t) != 1 || f.outbox(t) != 1 {
		t.Fatalf("%d events and %d outbox records, want exactly 1 and 1", f.events(t), f.outbox(t))
	}
}

func TestConcurrentDifferentPayloadsOnOneKeyAllowExactlyOne(t *testing.T) {
	f := newFixture(t)
	const n = 12
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			body := []byte(`{"writer":` + formatInt(int64(i)) + `}`)
			_, errs[i] = f.send(f.env.Signed(f.endpoint.SigningSecret, body, "contended"))
		}()
	}
	wg.Wait()
	wins := 0
	for _, err := range errs {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, ingest.ErrIdempotencyConflict):
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	if wins != 1 || f.events(t) != 1 || f.outbox(t) != 1 {
		t.Fatalf("wins=%d events=%d outbox=%d, want 1 each", wins, f.events(t), f.outbox(t))
	}
}

func TestOutboxFailureRollsBackTheEvent(t *testing.T) {
	f := newFixture(t)
	f.env.Exec(t, "ALTER TABLE outbox ADD CONSTRAINT outbox_block CHECK (false) NOT VALID")
	_, err := f.send(f.env.Signed(f.endpoint.SigningSecret, []byte(`{"x":1}`), "atomic"))
	if err == nil {
		t.Fatal("expected the failing outbox insert to fail the request")
	}
	var ae *apperror.Error
	if errors.As(err, &ae) {
		t.Fatalf("a database failure must not be a client error: %v", err)
	}
	if f.events(t) != 0 || f.outbox(t) != 0 {
		t.Fatalf("event committed without its outbox record: %d events", f.events(t))
	}
	// After the fault is removed the same idempotency key is usable: nothing was reserved.
	f.env.Exec(t, "ALTER TABLE outbox DROP CONSTRAINT outbox_block")
	if res, err := f.send(f.env.Signed(f.endpoint.SigningSecret, []byte(`{"x":1}`), "atomic")); err != nil || res.Duplicate {
		t.Fatalf("retry after rollback: %+v %v", res, err)
	}
}

func TestSecretRotation(t *testing.T) {
	f := newFixture(t)
	oldSecret := f.endpoint.SigningSecret
	if _, err := f.send(f.env.Signed(oldSecret, []byte(`{}`), "before")); err != nil {
		t.Fatal(err)
	}

	rot, err := f.env.Svc.RotateSecret(bg, f.owner.Principal, f.owner.Org.ID, f.endpoint.Endpoint.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rot.SigningSecret == oldSecret || !strings.HasPrefix(rot.SigningSecret, "rfs_") {
		t.Fatal("rotation must return a new secret")
	}
	if _, err := f.send(f.env.Signed(oldSecret, []byte(`{}`), "after-old")); !errors.Is(err, ingest.ErrSignatureMismatch) {
		t.Fatalf("the old secret must stop working: %v", err)
	}
	if _, err := f.send(f.env.Signed(rot.SigningSecret, []byte(`{}`), "after-new")); err != nil {
		t.Fatalf("the new secret must work: %v", err)
	}
	for _, secret := range []string{oldSecret, rot.SigningSecret} {
		if f.env.Count(t, "SELECT count(*) FROM ingestion_endpoints t WHERE t::text LIKE '%' || $1 || '%'", secret) != 0 ||
			f.env.Count(t, "SELECT count(*) FROM audit_events t WHERE t::text LIKE '%' || $1 || '%'", secret) != 0 {
			t.Fatal("a signing secret leaked into the database in plaintext")
		}
	}
	if f.env.Count(t, "SELECT count(*) FROM audit_events WHERE action = 'ingestion_endpoint.secret_rotated'") != 1 {
		t.Fatal("rotation must be audited once")
	}
}

func TestConcurrentRotationAndIngestionStayConsistent(t *testing.T) {
	f := newFixture(t)
	oldSecret := f.endpoint.SigningSecret

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	accepted := map[string]bool{}
	var unexpected []error
	for w := range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				key := "w" + formatInt(int64(w)) + "-" + formatInt(int64(i))
				res, err := f.send(f.env.Signed(oldSecret, []byte(`{"w":1}`), key))
				mu.Lock()
				switch {
				case err == nil:
					accepted[res.EventID] = true
				case errors.Is(err, ingest.ErrSignatureMismatch):
				default:
					unexpected = append(unexpected, err)
				}
				mu.Unlock()
			}
		}()
	}

	time.Sleep(150 * time.Millisecond)
	rot, err := f.env.Svc.RotateSecret(bg, f.owner.Principal, f.owner.Org.ID, f.endpoint.Endpoint.ID)
	if err != nil {
		t.Fatal(err)
	}
	// From here on, the old secret must never be accepted.
	after := f.events(t)
	time.Sleep(150 * time.Millisecond)
	close(stop)
	wg.Wait()

	if len(unexpected) > 0 {
		t.Fatalf("unexpected errors during rotation: %v", unexpected[0])
	}
	if got := f.events(t); got != after {
		t.Fatalf("%d events were accepted with the old secret after rotation committed", got-after)
	}
	if got := f.events(t); got != len(accepted) || got != f.outbox(t) {
		t.Fatalf("events=%d accepted=%d outbox=%d must all agree", got, len(accepted), f.outbox(t))
	}
	if _, err := f.send(f.env.Signed(rot.SigningSecret, []byte(`{}`), "post")); err != nil {
		t.Fatalf("new secret rejected: %v", err)
	}
}

func TestRateLimitsAreEnforcedAndDoNotLetUnsignedTrafficStarveProducers(t *testing.T) {
	f := newFixture(t)
	cfg := f.env.IngestConfig
	cfg.PerEndpointPerMinute = 3
	cfg.PerIPPerMinute = 5
	limited := ingest.NewService(f.env.Pool, f.env.Box, cfg, f.env.Clock.Now, nopLogger(), nil)
	pub := f.endpoint.Endpoint.PublicID
	send := func(ip string, req ingest.Request) error {
		ep, err := limited.Lookup(bg, pub, ip)
		if err != nil {
			return err
		}
		_, err = limited.Accept(bg, ep, req)
		return err
	}

	// An attacker spamming unsigned requests from one address exhausts only that address's budget.
	for i := 0; i < 5; i++ {
		if err := send("198.51.100.9", ingest.Request{ContentType: "application/json", Body: []byte(`{}`)}); errors.Is(err, ingest.ErrNotFound) {
			t.Fatalf("request %d: %v", i, err)
		}
	}
	err := send("198.51.100.9", ingest.Request{})
	var ae *apperror.Error
	if !errors.As(err, &ae) || ae.Status != 429 || ae.RetryAfter <= 0 {
		t.Fatalf("expected 429 with Retry-After, got %v", err)
	}
	// ...and the legitimate producer on another address is unaffected, because
	// unsigned requests never touch the per-endpoint budget.
	for i := 0; i < 3; i++ {
		if err := send("203.0.113.5", f.env.Signed(f.endpoint.SigningSecret, []byte(`{}`), "ok-"+formatInt(int64(i)))); err != nil {
			t.Fatalf("legitimate request %d: %v", i, err)
		}
	}
	if err := send("203.0.113.5", f.env.Signed(f.endpoint.SigningSecret, []byte(`{}`), "ok-4")); !errors.As(err, &ae) || ae.Status != 429 {
		t.Fatalf("per-endpoint limit: %v", err)
	}
	if f.events(t) != 3 {
		t.Fatalf("events = %d, want 3", f.events(t))
	}
	// The next window starts fresh.
	f.env.Clock.Advance(61 * time.Second)
	if err := send("203.0.113.5", f.env.Signed(f.endpoint.SigningSecret, []byte(`{}`), "ok-5")); err != nil {
		t.Fatalf("after the window: %v", err)
	}
}

func TestMetricsCountOutcomes(t *testing.T) {
	f := newFixture(t)
	body := []byte(`{"m":1}`)
	f.send(f.env.Signed(f.endpoint.SigningSecret, body, "m1"))
	f.send(f.env.Signed(f.endpoint.SigningSecret, body, "m1"))
	f.send(f.env.Signed("rfs_wrong", body, "m2"))
	f.send(f.env.Signed(f.endpoint.SigningSecret, []byte(`{"m":2}`), "m1"))

	var out strings.Builder
	f.env.Metrics.Render(&out)
	for _, want := range []string{
		"relayforge_ingest_requests_total 4", "relayforge_ingest_accepted_total 1", "relayforge_ingest_idempotency_replays_total 1",
		"relayforge_ingest_signature_failures_total 1", "relayforge_ingest_idempotency_conflicts_total 1",
		"relayforge_ingest_rejected_total 2", "relayforge_ingest_request_seconds_count 4",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("metrics missing %q:\n%s", want, out.String())
		}
	}
	for _, banned := range []string{f.endpoint.SigningSecret, f.endpoint.Endpoint.PublicID, "m1"} {
		if strings.Contains(out.String(), banned) {
			t.Errorf("metrics expose %q", banned)
		}
	}
}
