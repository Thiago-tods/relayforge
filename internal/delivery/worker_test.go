package delivery_test

import (
	"context"
	crand "crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Thiago-tods/relayforge/internal/app/apptest"
	"github.com/Thiago-tods/relayforge/internal/database"
	"github.com/Thiago-tods/relayforge/internal/delivery"
	"github.com/Thiago-tods/relayforge/internal/egress"
	"github.com/Thiago-tods/relayforge/internal/observability"
	"github.com/Thiago-tods/relayforge/internal/secretbox"
)

var bg = context.Background()

// ---- fixtures ----

type fixture struct {
	env    *apptest.Env
	owner  apptest.User
	ep     string // ingestion endpoint id
	pub    string
	secret string
	keySeq atomic.Int64
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	env := apptest.New(t)
	owner := env.Register(t, "owner@example.com")
	ep := env.CreateEndpoint(t, owner)
	return &fixture{env: env, owner: owner, ep: ep.Endpoint.ID, pub: ep.Endpoint.PublicID, secret: ep.SigningSecret}
}

// event ingests a real event (and its outbox row) and returns its id.
func (f *fixture) event(t *testing.T, payload string) string {
	t.Helper()
	key := fmt.Sprintf("k-%d", f.keySeq.Add(1))
	res, err := f.env.Send(f.pub, f.env.Signed(f.secret, []byte(payload), key))
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	return res.EventID
}

type dest struct{ ID, Secret string }

const destinationURL = "https://hooks.example.com/relayforge/abc?token=SECRETTOKEN"

// destination inserts an ACTIVE destination whose secret is sealed like production does.
func (f *fixture) destination(t *testing.T, org, user, name string) dest {
	t.Helper()
	id, err := newUUID()
	if err != nil {
		t.Fatal(err)
	}
	secret, err := delivery.GenerateDestinationSecret()
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := f.env.Box.Seal([]byte(secret), delivery.DestinationSecretAAD(id))
	if err != nil {
		t.Fatal(err)
	}
	q, _ := f.env.Pool.Querier()
	_, err = q.Exec(bg, `INSERT INTO destinations (id, organization_id, name, url, signing_secret_ciphertext, encryption_key_version, created_by_user_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`, id, org, name, destinationURL, sealed, f.env.Box.Version(), user)
	if err != nil {
		t.Fatalf("insert destination: %v", err)
	}
	return dest{ID: id, Secret: secret}
}

func newUUID() (string, error) {
	q := make([]byte, 16)
	if _, err := crand.Read(q); err != nil {
		return "", err
	}
	q[6], q[8] = (q[6]&0x0f)|0x40, (q[8]&0x3f)|0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", q[0:4], q[4:6], q[6:8], q[8:10], q[10:]), nil
}

// pending inserts a PENDING delivery that is due now (by the fake clock).
func (f *fixture) pending(t *testing.T, org, eventID, destID string, maxAttempts int) string {
	t.Helper()
	q, _ := f.env.Pool.Querier()
	var id string
	err := q.QueryRow(bg, `INSERT INTO deliveries (organization_id, event_id, destination_id, max_attempts, next_attempt_at)
		VALUES ($1, $2, $3, $4, $5) RETURNING id`, org, eventID, destID, maxAttempts, f.env.Clock.Now().Add(-time.Second)).Scan(&id)
	if err != nil {
		t.Fatalf("insert delivery: %v", err)
	}
	return id
}

func (f *fixture) state(t *testing.T, id string) (status string, attempts int, claimToken *string) {
	t.Helper()
	q, _ := f.env.Pool.Querier()
	if err := q.QueryRow(bg, "SELECT status, attempt_count, claim_token::text FROM deliveries WHERE id = $1", id).Scan(&status, &attempts, &claimToken); err != nil {
		t.Fatal(err)
	}
	return
}

type attemptRow struct {
	Number     int
	Outcome    string
	HTTPStatus *int
	ErrorClass *string
}

func (f *fixture) attempts(t *testing.T, id string) []attemptRow {
	t.Helper()
	q, _ := f.env.Pool.Querier()
	rows, err := q.Query(bg, "SELECT attempt_number, outcome, http_status, error_class FROM delivery_attempts WHERE delivery_id = $1 ORDER BY attempt_number", id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []attemptRow
	for rows.Next() {
		var a attemptRow
		var st *int16
		if err := rows.Scan(&a.Number, &a.Outcome, &st, &a.ErrorClass); err != nil {
			t.Fatal(err)
		}
		if st != nil {
			v := int(*st)
			a.HTTPStatus = &v
		}
		out = append(out, a)
	}
	return out
}

// fakeSender records requests and answers with a scripted result.
type fakeSender struct {
	mu       sync.Mutex
	requests []egress.Request
	respond  func(req egress.Request, n int) (egress.Response, error)
	active   atomic.Int32
	maxSeen  atomic.Int32
}

func (s *fakeSender) Do(ctx context.Context, req egress.Request) (egress.Response, error) {
	cur := s.active.Add(1)
	defer s.active.Add(-1)
	for {
		m := s.maxSeen.Load()
		if cur <= m || s.maxSeen.CompareAndSwap(m, cur) {
			break
		}
	}
	s.mu.Lock()
	s.requests = append(s.requests, req)
	n := len(s.requests)
	s.mu.Unlock()
	if s.respond == nil {
		return egress.Response{StatusCode: 204}, nil
	}
	return s.respond(req, n)
}

func (s *fakeSender) count() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.requests) }

func status(code int) func(egress.Request, int) (egress.Response, error) {
	return func(egress.Request, int) (egress.Response, error) { return egress.Response{StatusCode: code}, nil }
}

func requestErr(class egress.ErrorClass) func(egress.Request, int) (egress.Response, error) {
	return func(egress.Request, int) (egress.Response, error) {
		return egress.Response{}, &egress.RequestError{Class: class}
	}
}

func (f *fixture) worker(t *testing.T, s delivery.Sender, mutate func(*delivery.WorkerDeps)) *delivery.Worker {
	t.Helper()
	cfg := delivery.DefaultWorkerConfig()
	cfg.WorkerID = "test-worker"
	cfg.Concurrency, cfg.BatchSize = 4, 4
	cfg.PollInterval, cfg.RecoveryInterval = 10*time.Millisecond, 20*time.Millisecond
	cfg.LeaseDuration, cfg.ShutdownGrace = 2*time.Minute, 200*time.Millisecond
	deps := delivery.WorkerDeps{
		Pool: f.env.Pool, Box: f.env.Box, Sender: s, Policy: delivery.DefaultRetryPolicy(), Config: cfg,
		Log: testLogger(), Metrics: delivery.NewMetrics(observability.NewRegistry()),
		Now: f.env.Clock.Now, Rand: func() float64 { return 0.5 }, // no jitter: delays are exact
	}
	if mutate != nil {
		mutate(&deps)
	}
	w, err := delivery.NewWorker(deps)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

// testLogger discards logs unless RELAYFORGE_TEST_LOG is set, which helps when debugging a failing test.
func testLogger() *slog.Logger {
	if os.Getenv("RELAYFORGE_TEST_LOG") != "" {
		return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func (f *fixture) runOnce(t *testing.T, w *delivery.Worker) int {
	t.Helper()
	n, err := w.RunOnce(bg)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	return n
}

// ---- claiming ----

func TestClaimDueClaimsOnlyEligibleDeliveries(t *testing.T) {
	f := newFixture(t)
	org, user := f.owner.Org.ID, f.owner.Principal.User.ID
	d1 := f.destination(t, org, user, "Active")
	paused := f.destination(t, org, user, "Paused")
	revoked := f.destination(t, org, user, "Revoked")
	f.env.Exec(t, "UPDATE destinations SET status = 'PAUSED' WHERE id = $1", paused.ID)
	f.env.Exec(t, "UPDATE destinations SET status = 'REVOKED', revoked_at = now() WHERE id = $1", revoked.ID)

	due := f.pending(t, org, f.event(t, `{"n":1}`), d1.ID, 8)
	future := f.pending(t, org, f.event(t, `{"n":2}`), d1.ID, 8)
	f.env.Exec(t, "UPDATE deliveries SET next_attempt_at = $2 WHERE id = $1", future, f.env.Clock.Now().Add(time.Hour))
	onPaused := f.pending(t, org, f.event(t, `{"n":3}`), paused.ID, 8)
	onRevoked := f.pending(t, org, f.event(t, `{"n":4}`), revoked.ID, 8)

	var claimed []delivery.Claimed
	err := f.env.Pool.InTx(bg, func(tx pgx.Tx) error {
		var err error
		claimed, err = delivery.ClaimDue(bg, tx, f.env.Clock.Now(), 10, 2*time.Minute, "w1")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 1 || claimed[0].DeliveryID != due {
		t.Fatalf("claimed %+v, want only the due delivery on an active destination", claimed)
	}
	c := claimed[0]
	if c.Attempt != 1 || c.OrganizationID != org || len(c.ClaimToken) != 36 {
		t.Fatalf("claim details: %+v", c)
	}
	if want := f.env.Clock.Now().Add(2 * time.Minute); c.LeaseExpiresAt.Sub(want).Abs() > time.Second {
		t.Fatalf("lease expires at %v, want about %v", c.LeaseExpiresAt, want)
	}
	for id, want := range map[string]string{future: "PENDING", onPaused: "PENDING", onRevoked: "PENDING", due: "IN_FLIGHT"} {
		if got, _, _ := f.state(t, id); got != want {
			t.Errorf("delivery %s is %s, want %s", id, got, want)
		}
	}
}

func TestClaimTokensAreUniqueRandomAndOrderIsOldestFirst(t *testing.T) {
	f := newFixture(t)
	org, user := f.owner.Org.ID, f.owner.Principal.User.ID
	d := f.destination(t, org, user, "D")
	var ids []string
	for i := 0; i < 6; i++ {
		id := f.pending(t, org, f.event(t, fmt.Sprintf(`{"n":%d}`, i)), d.ID, 8)
		f.env.Exec(t, "UPDATE deliveries SET next_attempt_at = $2 WHERE id = $1", id, f.env.Clock.Now().Add(-time.Duration(60-i)*time.Second))
		ids = append(ids, id)
	}
	var claimed []delivery.Claimed
	if err := f.env.Pool.InTx(bg, func(tx pgx.Tx) error {
		var err error
		claimed, err = delivery.ClaimDue(bg, tx, f.env.Clock.Now(), 4, time.Minute, "w1")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 4 {
		t.Fatalf("limit must be honored: %d", len(claimed))
	}
	tokens := map[string]bool{}
	for i, c := range claimed {
		if c.DeliveryID != ids[i] {
			t.Errorf("claim %d is %s, want the oldest first (%s)", i, c.DeliveryID, ids[i])
		}
		if tokens[c.ClaimToken] {
			t.Error("duplicate claim token")
		}
		tokens[c.ClaimToken] = true
	}
}

func TestConcurrentClaimsNeverShareADelivery(t *testing.T) {
	f := newFixture(t)
	org, user := f.owner.Org.ID, f.owner.Principal.User.ID
	d := f.destination(t, org, user, "D")
	const total = 40
	want := map[string]bool{}
	for i := 0; i < total; i++ {
		want[f.pending(t, org, f.event(t, fmt.Sprintf(`{"n":%d}`, i)), d.ID, 8)] = true
	}

	var mu sync.Mutex
	got := map[string]int{}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for {
				var claimed []delivery.Claimed
				err := f.env.Pool.InTx(bg, func(tx pgx.Tx) error {
					var err error
					claimed, err = delivery.ClaimDue(bg, tx, f.env.Clock.Now(), 5, time.Minute, fmt.Sprintf("w%d", w))
					return err
				})
				if err != nil {
					t.Error(err)
					return
				}
				if len(claimed) == 0 {
					return
				}
				mu.Lock()
				for _, c := range claimed {
					got[c.DeliveryID]++
				}
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()
	if len(got) != total {
		t.Fatalf("%d deliveries claimed, want %d", len(got), total)
	}
	for id, n := range got {
		if n != 1 || !want[id] {
			t.Errorf("delivery %s claimed %d times", id, n)
		}
	}
}

// ---- executing and finalizing ----

func TestSuccessfulDeliveryIsSignedAndRecorded(t *testing.T) {
	f := newFixture(t)
	org, user := f.owner.Org.ID, f.owner.Principal.User.ID
	d := f.destination(t, org, user, "D")
	payload := "{ \"order\" : 42,\n \"note\":\"exact bytes\" }"
	eventID := f.event(t, payload)
	id := f.pending(t, org, eventID, d.ID, 8)

	s := &fakeSender{respond: status(204)}
	w := f.worker(t, s, nil)
	if n := f.runOnce(t, w); n != 1 {
		t.Fatalf("processed %d", n)
	}

	if st, attempts, token := f.state(t, id); st != "SUCCEEDED" || attempts != 1 || token != nil {
		t.Fatalf("state = %s attempts=%d token=%v", st, attempts, token)
	}
	a := f.attempts(t, id)
	if len(a) != 1 || a[0].Number != 1 || a[0].Outcome != "SUCCESS" || a[0].HTTPStatus == nil || *a[0].HTTPStatus != 204 {
		t.Fatalf("attempts: %+v", a)
	}

	req := s.requests[0]
	if req.URL != destinationURL || string(req.Body) != payload {
		t.Fatalf("request: %s body %q", "(url)", req.Body)
	}
	h := req.Headers
	if h[delivery.HeaderDeliveryID] != id || h[delivery.HeaderEventID] != eventID || h[delivery.HeaderAttempt] != "1" || h[delivery.HeaderKeyVersion] != "1" {
		t.Fatalf("identifiers: %v", h)
	}
	if !delivery.VerifyOutbound(d.Secret, h[delivery.HeaderTimestamp], id, eventID, []byte(payload), h[delivery.HeaderSignature]) {
		t.Fatal("the signature must verify with the destination secret and the exact payload bytes")
	}
	if delivery.VerifyOutbound(f.secret, h[delivery.HeaderTimestamp], id, eventID, []byte(payload), h[delivery.HeaderSignature]) {
		t.Fatal("the ingestion secret must never sign deliveries")
	}
}

func TestRetryIsScheduledAndSurvivesARestart(t *testing.T) {
	f := newFixture(t)
	org, user := f.owner.Org.ID, f.owner.Principal.User.ID
	d := f.destination(t, org, user, "D")
	id := f.pending(t, org, f.event(t, `{"r":1}`), d.ID, 8)

	s := &fakeSender{respond: func(_ egress.Request, n int) (egress.Response, error) {
		if n == 1 {
			return egress.Response{StatusCode: 503, RetryAfter: "120"}, nil
		}
		return egress.Response{StatusCode: 200}, nil
	}}
	start := f.env.Clock.Now()
	f.runOnce(t, f.worker(t, s, nil))

	st, attempts, token := f.state(t, id)
	if st != "PENDING" || attempts != 1 || token != nil {
		t.Fatalf("after a 503: %s attempts=%d token=%v", st, attempts, token)
	}
	q, _ := f.env.Pool.Querier()
	var next time.Time
	var lastStatus *int16
	if err := q.QueryRow(bg, "SELECT next_attempt_at, last_http_status FROM deliveries WHERE id = $1", id).Scan(&next, &lastStatus); err != nil {
		t.Fatal(err)
	}
	if got := next.Sub(start); got < 119*time.Second || got > 121*time.Second {
		t.Fatalf("the Retry-After of 120 s must be honored, next attempt in %v", got)
	}
	if lastStatus == nil || *lastStatus != 503 {
		t.Fatalf("last status %v", lastStatus)
	}

	// Not due yet: a fresh worker (a restart) must not take it.
	if n := f.runOnce(t, f.worker(t, s, nil)); n != 0 {
		t.Fatalf("a delivery scheduled for later was processed early (%d)", n)
	}
	f.env.Clock.Advance(121 * time.Second)
	if n := f.runOnce(t, f.worker(t, s, nil)); n != 1 {
		t.Fatalf("a restarted worker must pick the retry up, processed %d", n)
	}
	if st, attempts, _ := f.state(t, id); st != "SUCCEEDED" || attempts != 2 {
		t.Fatalf("final state %s attempts=%d", st, attempts)
	}
	rows := f.attempts(t, id)
	if len(rows) != 2 || rows[0].Outcome != "HTTP_ERROR" || rows[1].Outcome != "SUCCESS" {
		t.Fatalf("attempt history: %+v", rows)
	}
}

func TestResponseClassificationDrivesTheLifecycle(t *testing.T) {
	cases := []struct {
		name   string
		code   int
		final  string
		reason string
	}{
		{"redirects are final and never followed", 302, "FAILED", "non_retryable"},
		{"permanent client error", 404, "FAILED", "non_retryable"},
		{"unauthorized", 401, "FAILED", "non_retryable"},
		{"request timeout is retried", 408, "PENDING", "retry"},
		{"too early is retried", 425, "PENDING", "retry"},
		{"too many requests is retried", 429, "PENDING", "retry"},
		{"server error is retried", 500, "PENDING", "retry"},
		{"bad gateway is retried", 502, "PENDING", "retry"},
		{"success", 200, "SUCCEEDED", "succeeded"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			org, user := f.owner.Org.ID, f.owner.Principal.User.ID
			d := f.destination(t, org, user, "D")
			id := f.pending(t, org, f.event(t, `{"c":1}`), d.ID, 8)
			s := &fakeSender{respond: status(c.code)}
			f.runOnce(t, f.worker(t, s, nil))
			if st, _, _ := f.state(t, id); st != c.final {
				t.Fatalf("status %d: delivery is %s, want %s", c.code, st, c.final)
			}
			if s.count() != 1 {
				t.Fatalf("exactly one request expected, got %d", s.count())
			}
		})
	}
}

func TestRetriesAreBoundedByMaxAttempts(t *testing.T) {
	f := newFixture(t)
	org, user := f.owner.Org.ID, f.owner.Principal.User.ID
	d := f.destination(t, org, user, "D")
	id := f.pending(t, org, f.event(t, `{"x":1}`), d.ID, 3)
	s := &fakeSender{respond: status(500)}
	w := f.worker(t, s, nil)
	for i := 0; i < 5; i++ {
		f.runOnce(t, w)
		f.env.Clock.Advance(7 * time.Hour) // beyond any backoff
	}
	if st, attempts, _ := f.state(t, id); st != "FAILED" || attempts != 3 {
		t.Fatalf("state %s after %d attempts, want FAILED after exactly 3", st, attempts)
	}
	if s.count() != 3 || len(f.attempts(t, id)) != 3 {
		t.Fatalf("%d requests and %d attempt rows, want 3 each", s.count(), len(f.attempts(t, id)))
	}
}

func TestTransportFailureClasses(t *testing.T) {
	cases := []struct {
		class egress.ErrorClass
		final string
		out   string
	}{
		{egress.ClassDNS, "PENDING", "NETWORK_ERROR"},
		{egress.ClassConnect, "PENDING", "NETWORK_ERROR"},
		{egress.ClassTLS, "PENDING", "NETWORK_ERROR"},
		{egress.ClassTimeout, "PENDING", "TIMEOUT"},
		{egress.ClassBlockedAddress, "FAILED", "BLOCKED_BY_POLICY"},
		{egress.ClassInvalidURL, "FAILED", "BLOCKED_BY_POLICY"},
	}
	for _, c := range cases {
		t.Run(string(c.class), func(t *testing.T) {
			f := newFixture(t)
			org, user := f.owner.Org.ID, f.owner.Principal.User.ID
			d := f.destination(t, org, user, "D")
			id := f.pending(t, org, f.event(t, `{}`), d.ID, 8)
			f.runOnce(t, f.worker(t, &fakeSender{respond: requestErr(c.class)}, nil))
			st, _, _ := f.state(t, id)
			a := f.attempts(t, id)
			if st != c.final || len(a) != 1 || a[0].Outcome != c.out || a[0].HTTPStatus != nil || a[0].ErrorClass == nil || *a[0].ErrorClass != string(c.class) {
				t.Fatalf("state %s attempts %+v", st, a)
			}
		})
	}
}

func TestShutdownInterruptionIsNotRecordedAsAFailure(t *testing.T) {
	f := newFixture(t)
	org, user := f.owner.Org.ID, f.owner.Principal.User.ID
	d := f.destination(t, org, user, "D")
	id := f.pending(t, org, f.event(t, `{}`), d.ID, 8)
	reg := observability.NewRegistry()
	w := f.worker(t, &fakeSender{respond: requestErr(egress.ClassCanceled)}, func(dd *delivery.WorkerDeps) { dd.Metrics = delivery.NewMetrics(reg) })
	f.runOnce(t, w)
	var metricsText strings.Builder
	reg.Render(&metricsText)
	if !strings.Contains(metricsText.String(), "relayforge_delivery_interrupted_total 1") ||
		!strings.Contains(metricsText.String(), "relayforge_delivery_finalize_errors_total 0") ||
		!strings.Contains(metricsText.String(), "relayforge_delivery_failed_total 0") {
		t.Fatalf("the interruption must be counted as such, not as a failure or a storage error:\n%s", metricsText.String())
	}

	// The delivery is still claimed and no failure was written: the lease will expire and recovery will record it.
	if st, attempts, token := f.state(t, id); st != "IN_FLIGHT" || attempts != 1 || token == nil {
		t.Fatalf("an interrupted delivery must stay claimed: %s %d %v", st, attempts, token)
	}
	if len(f.attempts(t, id)) != 0 {
		t.Fatal("no attempt may be recorded for an interrupted request")
	}
	f.env.Clock.Advance(3 * time.Minute)
	n, err := w.RecoverExpired(bg)
	if err != nil || n != 1 {
		t.Fatalf("recovery: %d %v", n, err)
	}
	a := f.attempts(t, id)
	if st, _, _ := f.state(t, id); st != "PENDING" || len(a) != 1 || a[0].Outcome != "ABANDONED" || a[0].Number != 1 {
		t.Fatalf("after recovery: %s %+v", st, a)
	}
}

func TestDestinationAndSecretProblemsAreHandledWithoutSending(t *testing.T) {
	t.Run("destination paused after the claim", func(t *testing.T) {
		f := newFixture(t)
		org, user := f.owner.Org.ID, f.owner.Principal.User.ID
		d := f.destination(t, org, user, "D")
		id := f.pending(t, org, f.event(t, `{}`), d.ID, 8)
		s := &fakeSender{}
		w := f.worker(t, s, func(dd *delivery.WorkerDeps) {
			dd.AfterClaim = func() { f.env.Exec(t, "UPDATE destinations SET status = 'PAUSED' WHERE id = $1", d.ID) }
		})
		f.runOnce(t, w)
		a := f.attempts(t, id)
		if s.count() != 0 || len(a) != 1 || a[0].Outcome != "ABANDONED" || a[0].ErrorClass == nil || *a[0].ErrorClass != "destination_inactive" {
			t.Fatalf("requests=%d attempts=%+v", s.count(), a)
		}
		if st, _, _ := f.state(t, id); st != "PENDING" {
			t.Fatalf("state %s", st)
		}
		// While paused it is not claimed again; once activated it is.
		f.env.Clock.Advance(time.Hour)
		if n := f.runOnce(t, w); n != 0 {
			t.Fatalf("a paused destination was claimed (%d)", n)
		}
		f.env.Exec(t, "UPDATE destinations SET status = 'ACTIVE' WHERE id = $1", d.ID)
		if n := f.runOnce(t, f.worker(t, s, nil)); n != 1 || s.count() != 1 {
			t.Fatalf("after activation: processed %d, requests %d", n, s.count())
		}
	})

	t.Run("revoked destination cancels pending deliveries", func(t *testing.T) {
		f := newFixture(t)
		org, user := f.owner.Org.ID, f.owner.Principal.User.ID
		d := f.destination(t, org, user, "D")
		id := f.pending(t, org, f.event(t, `{}`), d.ID, 8)
		f.env.Exec(t, "UPDATE destinations SET status = 'REVOKED', revoked_at = now() WHERE id = $1", d.ID)
		w := f.worker(t, &fakeSender{}, nil)
		n, err := w.CancelRevoked(bg)
		if err != nil || n != 1 {
			t.Fatalf("cancelled %d %v", n, err)
		}
		if st, _, _ := f.state(t, id); st != "CANCELLED" {
			t.Fatalf("state %s", st)
		}
	})

	t.Run("secret cannot be decrypted", func(t *testing.T) {
		f := newFixture(t)
		org, user := f.owner.Org.ID, f.owner.Principal.User.ID
		d := f.destination(t, org, user, "D")
		id := f.pending(t, org, f.event(t, `{}`), d.ID, 8)
		otherKey, _ := secretbox.New([]byte("another-32-byte-key-for-testing!"), 1)
		s := &fakeSender{}
		f.runOnce(t, f.worker(t, s, func(dd *delivery.WorkerDeps) { dd.Box = otherKey }))
		a := f.attempts(t, id)
		if s.count() != 0 || len(a) != 1 || a[0].Outcome != "ABANDONED" || a[0].ErrorClass == nil || *a[0].ErrorClass != "secret_unavailable" {
			t.Fatalf("requests=%d attempts=%+v", s.count(), a)
		}
		if st, _, _ := f.state(t, id); st != "PENDING" {
			t.Fatalf("a key problem must keep the delivery retryable, got %s", st)
		}
	})

	t.Run("a rotated secret signs with the new version", func(t *testing.T) {
		f := newFixture(t)
		org, user := f.owner.Org.ID, f.owner.Principal.User.ID
		d := f.destination(t, org, user, "D")
		id := f.pending(t, org, f.event(t, `{"k":1}`), d.ID, 8)
		newSecret, _ := delivery.GenerateDestinationSecret()
		sealed, _ := f.env.Box.Seal([]byte(newSecret), delivery.DestinationSecretAAD(d.ID))
		f.env.Exec(t, "UPDATE destinations SET signing_secret_ciphertext = $2, secret_version = secret_version + 1 WHERE id = $1", d.ID, sealed)

		s := &fakeSender{}
		f.runOnce(t, f.worker(t, s, nil))
		h := s.requests[0].Headers
		if h[delivery.HeaderKeyVersion] != "2" {
			t.Fatalf("key version header = %q", h[delivery.HeaderKeyVersion])
		}
		eventID := h[delivery.HeaderEventID]
		body := s.requests[0].Body
		if !delivery.VerifyOutbound(newSecret, h[delivery.HeaderTimestamp], id, eventID, body, h[delivery.HeaderSignature]) ||
			delivery.VerifyOutbound(d.Secret, h[delivery.HeaderTimestamp], id, eventID, body, h[delivery.HeaderSignature]) {
			t.Fatal("the delivery must be signed with the current secret, never the previous one")
		}
	})
}

// ---- fencing and recovery ----

func claimOne(t *testing.T, f *fixture, workerID string) delivery.Claimed {
	t.Helper()
	var claimed []delivery.Claimed
	if err := f.env.Pool.InTx(bg, func(tx pgx.Tx) error {
		var err error
		claimed, err = delivery.ClaimDue(bg, tx, f.env.Clock.Now(), 1, time.Minute, workerID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 1 {
		t.Fatalf("claimed %d", len(claimed))
	}
	return claimed[0]
}

func finishSuccess(f *fixture, c delivery.Claimed, token string) error {
	now := f.env.Clock.Now()
	rec := delivery.AttemptRecord{Number: c.Attempt, StartedAt: now, FinishedAt: now, Outcome: delivery.OutcomeSuccess, HTTPStatus: 204}
	return f.env.Pool.InTx(bg, func(tx pgx.Tx) error {
		return delivery.Finish(bg, tx, c.OrganizationID, c.DeliveryID, token, rec,
			delivery.Decision{Status: delivery.StatusSucceeded, Reason: delivery.ReasonSucceeded}, now)
	})
}

func TestStaleWorkerCannotFinalizeAfterRecoveryAndANewClaim(t *testing.T) {
	f := newFixture(t)
	org, user := f.owner.Org.ID, f.owner.Principal.User.ID
	d := f.destination(t, org, user, "D")
	id := f.pending(t, org, f.event(t, `{}`), d.ID, 8)
	w := f.worker(t, &fakeSender{}, nil)

	stale := claimOne(t, f, "stale-worker") // attempt 1, token T1; the worker then stalls
	f.env.Clock.Advance(3 * time.Minute)    // its lease expires
	if n, err := w.RecoverExpired(bg); err != nil || n != 1 {
		t.Fatalf("recovery: %d %v", n, err)
	}
	f.env.Clock.Advance(time.Hour)
	fresh := claimOne(t, f, "fresh-worker") // attempt 2, token T2
	if fresh.ClaimToken == stale.ClaimToken || fresh.Attempt != 2 {
		t.Fatalf("expected a new token and attempt 2: %+v", fresh)
	}

	err := finishSuccess(f, stale, stale.ClaimToken)
	if err == nil {
		t.Fatal("a stale worker must not be able to finalize")
	}
	if st, attempts, token := f.state(t, id); st != "IN_FLIGHT" || attempts != 2 || token == nil || *token != fresh.ClaimToken {
		t.Fatalf("the newer claim must be untouched: %s %d %v", st, attempts, token)
	}
	if a := f.attempts(t, id); len(a) != 1 || a[0].Outcome != "ABANDONED" {
		t.Fatalf("the stale worker's rolled-back attempt must not exist: %+v", a)
	}
	if err := finishSuccess(f, fresh, fresh.ClaimToken); err != nil {
		t.Fatalf("the current claim must finalize: %v", err)
	}
	if a := f.attempts(t, id); len(a) != 2 || a[1].Number != 2 || a[1].Outcome != "SUCCESS" {
		t.Fatalf("history: %+v", a)
	}
}

func TestFinalizeWithAWrongTokenRollsBackTheAttempt(t *testing.T) {
	f := newFixture(t)
	org, user := f.owner.Org.ID, f.owner.Principal.User.ID
	d := f.destination(t, org, user, "D")
	id := f.pending(t, org, f.event(t, `{}`), d.ID, 8)
	c := claimOne(t, f, "w")
	q, _ := f.env.Pool.Querier()
	var wrong string
	if err := q.QueryRow(bg, "SELECT gen_random_uuid()::text").Scan(&wrong); err != nil {
		t.Fatal(err)
	}
	if err := finishSuccess(f, c, wrong); !errors.Is(err, delivery.ErrStaleClaim) {
		t.Fatalf("got %v, want ErrStaleClaim", err)
	}
	if len(f.attempts(t, id)) != 0 {
		t.Fatal("the attempt must be rolled back together with the failed fencing")
	}
	if st, _, token := f.state(t, id); st != "IN_FLIGHT" || *token != c.ClaimToken {
		t.Fatal("the delivery must be untouched")
	}
}

func TestExpiredWorkerAndRecoveryRaceHasExactlyOneWinner(t *testing.T) {
	f := newFixture(t)
	org, user := f.owner.Org.ID, f.owner.Principal.User.ID
	d := f.destination(t, org, user, "D")
	w := f.worker(t, &fakeSender{}, nil)

	for i := 0; i < 30; i++ {
		id := f.pending(t, org, f.event(t, fmt.Sprintf(`{"i":%d}`, i)), d.ID, 8)
		c := claimOne(t, f, "slow-worker")
		if c.DeliveryID != id {
			t.Fatalf("iteration %d claimed %s, want %s", i, c.DeliveryID, id)
		}
		f.env.Clock.Advance(3 * time.Minute) // the lease is over for the reaper

		var wg sync.WaitGroup
		start := make(chan struct{})
		var workerErr error
		var recovered int
		var recErr error
		wg.Add(2)
		go func() { defer wg.Done(); <-start; workerErr = finishSuccess(f, c, c.ClaimToken) }()
		go func() { defer wg.Done(); <-start; recovered, recErr = w.RecoverExpired(bg) }()
		close(start)
		wg.Wait()

		if recErr != nil {
			t.Fatalf("iteration %d: recovery failed: %v", i, recErr)
		}
		a := f.attempts(t, id)
		if len(a) != 1 {
			t.Fatalf("iteration %d: %d attempt rows, want exactly 1 (worker err %v, recovered %d)", i, len(a), workerErr, recovered)
		}
		st, _, _ := f.state(t, id)
		switch {
		case workerErr == nil && recovered == 0 && a[0].Outcome == "SUCCESS" && st == "SUCCEEDED":
		case workerErr != nil && recovered == 1 && a[0].Outcome == "ABANDONED" && st == "PENDING":
		default:
			t.Fatalf("iteration %d: inconsistent outcome: worker err=%v recovered=%d attempt=%s state=%s", i, workerErr, recovered, a[0].Outcome, st)
		}
		// Park a recovered delivery so the next iteration claims a fresh one.
		if st == "PENDING" {
			f.env.Exec(t, "UPDATE deliveries SET next_attempt_at = $2 WHERE id = $1", id, f.env.Clock.Now().Add(100*time.Hour))
		}
	}
}

func TestConcurrentRecoveryRecoversEachDeliveryOnce(t *testing.T) {
	f := newFixture(t)
	org, user := f.owner.Org.ID, f.owner.Principal.User.ID
	d := f.destination(t, org, user, "D")
	const total = 24
	ids := make([]string, total)
	for i := range ids {
		ids[i] = f.pending(t, org, f.event(t, fmt.Sprintf(`{"i":%d}`, i)), d.ID, 8)
	}
	for range total {
		claimOne(t, f, "crashed")
	}
	f.env.Clock.Advance(3 * time.Minute)

	var wg sync.WaitGroup
	var recovered atomic.Int64
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := f.worker(t, &fakeSender{}, nil)
			for {
				n, err := w.RecoverExpired(bg)
				if err != nil {
					t.Error(err)
					return
				}
				if n == 0 {
					return
				}
				recovered.Add(int64(n))
			}
		}()
	}
	wg.Wait()
	if recovered.Load() != total {
		t.Fatalf("recovered %d, want %d", recovered.Load(), total)
	}
	for _, id := range ids {
		a := f.attempts(t, id)
		if st, _, _ := f.state(t, id); st != "PENDING" || len(a) != 1 || a[0].Outcome != "ABANDONED" || a[0].Number != 1 {
			t.Errorf("delivery %s: %s %+v", id, st, a)
		}
	}
}

func TestRecoveryNeverLeavesGapsAndExhaustsAfterMaxAttempts(t *testing.T) {
	f := newFixture(t)
	org, user := f.owner.Org.ID, f.owner.Principal.User.ID
	d := f.destination(t, org, user, "D")
	id := f.pending(t, org, f.event(t, `{}`), d.ID, 3)
	w := f.worker(t, &fakeSender{}, nil)
	for round := 1; round <= 3; round++ {
		c := claimOne(t, f, "crashing")
		if c.Attempt != round {
			t.Fatalf("round %d claimed attempt %d", round, c.Attempt)
		}
		f.env.Clock.Advance(3 * time.Minute)
		if n, err := w.RecoverExpired(bg); err != nil || n != 1 {
			t.Fatalf("round %d recovery: %d %v", round, n, err)
		}
		f.env.Clock.Advance(7 * time.Hour)
	}
	a := f.attempts(t, id)
	if st, attempts, _ := f.state(t, id); st != "FAILED" || attempts != 3 || len(a) != 3 {
		t.Fatalf("state %s attempts %d rows %d", st, attempts, len(a))
	}
	for i, row := range a {
		if row.Number != i+1 || row.Outcome != "ABANDONED" {
			t.Fatalf("history must be gapless and abandoned: %+v", a)
		}
	}
}

func TestRecoveryIgnoresUnexpiredLeases(t *testing.T) {
	f := newFixture(t)
	org, user := f.owner.Org.ID, f.owner.Principal.User.ID
	d := f.destination(t, org, user, "D")
	id := f.pending(t, org, f.event(t, `{}`), d.ID, 8)
	claimOne(t, f, "busy")
	w := f.worker(t, &fakeSender{}, nil)
	f.env.Clock.Advance(30 * time.Second) // lease is one minute
	if n, err := w.RecoverExpired(bg); err != nil || n != 0 {
		t.Fatalf("a live lease was recovered: %d %v", n, err)
	}
	if st, _, _ := f.state(t, id); st != "IN_FLIGHT" {
		t.Fatalf("state %s", st)
	}
}

// ---- tenant isolation ----

func TestTenantIsolationInTheDeliveryPath(t *testing.T) {
	f := newFixture(t)
	orgA, userA := f.owner.Org.ID, f.owner.Principal.User.ID
	other := f.env.Register(t, "other@example.com")
	orgB, userB := other.Org.ID, other.Principal.User.ID
	epB := f.env.CreateEndpoint(t, other)
	resB, err := f.env.Send(epB.Endpoint.PublicID, f.env.Signed(epB.SigningSecret, []byte(`{"tenant":"B"}`), "kb"))
	if err != nil {
		t.Fatal(err)
	}
	destA := f.destination(t, orgA, userA, "A")
	destB := f.destination(t, orgB, userB, "B")
	eventA := f.event(t, `{"tenant":"A"}`)

	// The schema refuses to join tenant A's destination to tenant B's event, in either direction.
	q, _ := f.env.Pool.Querier()
	for _, c := range []struct{ org, event, dest string }{
		{orgA, resB.EventID, destA.ID}, {orgB, eventA, destB.ID}, {orgA, resB.EventID, destB.ID}, {orgB, resB.EventID, destA.ID},
	} {
		if _, err := q.Exec(bg, "INSERT INTO deliveries (organization_id, event_id, destination_id) VALUES ($1,$2,$3)", c.org, c.event, c.dest); err == nil {
			t.Errorf("cross-tenant delivery %+v was accepted", c)
		}
	}

	idA := f.pending(t, orgA, eventA, destA.ID, 8)
	idB := f.pending(t, orgB, resB.EventID, destB.ID, 8)

	// Loading is always scoped by organization.
	if _, err := delivery.LoadSendData(bg, q, orgB, idA); !errors.Is(err, delivery.ErrNotFound) {
		t.Fatalf("tenant B loaded tenant A's delivery: %v", err)
	}
	dataA, err := delivery.LoadSendData(bg, q, orgA, idA)
	if err != nil || string(dataA.Payload) != `{"tenant":"A"}` {
		t.Fatalf("own delivery: %v", err)
	}

	// Each destination only ever receives its own tenant's payload.
	s := &fakeSender{}
	f.runOnce(t, f.worker(t, s, nil))
	if s.count() != 2 {
		t.Fatalf("requests: %d", s.count())
	}
	for _, r := range s.requests {
		tenant := strings.Contains(string(r.Body), `"A"`)
		deliveryID := r.Headers[delivery.HeaderDeliveryID]
		if tenant && deliveryID != idA || !tenant && deliveryID != idB {
			t.Errorf("payload %s delivered under %s", r.Body, deliveryID)
		}
	}

	// A claim token cannot be used across tenants or deliveries.
	c := delivery.Claimed{DeliveryID: idA, OrganizationID: orgB, Attempt: 1}
	err = f.env.Pool.InTx(bg, func(tx pgx.Tx) error {
		now := f.env.Clock.Now()
		return delivery.Finish(bg, tx, c.OrganizationID, c.DeliveryID, "00000000-0000-4000-8000-000000000000",
			delivery.AttemptRecord{Number: 1, StartedAt: now, FinishedAt: now, Outcome: delivery.OutcomeSuccess, HTTPStatus: 200},
			delivery.Decision{Status: delivery.StatusSucceeded}, now)
	})
	if err == nil {
		t.Fatal("finalizing another tenant's delivery must fail")
	}
	if st, _, _ := f.state(t, idA); st != "SUCCEEDED" || len(f.attempts(t, idA)) != 1 {
		t.Fatal("tenant A's delivery was altered")
	}
}

// ---- worker lifecycle ----

func TestConcurrencyIsBounded(t *testing.T) {
	f := newFixture(t)
	org, user := f.owner.Org.ID, f.owner.Principal.User.ID
	d := f.destination(t, org, user, "D")
	for i := 0; i < 16; i++ {
		f.pending(t, org, f.event(t, fmt.Sprintf(`{"i":%d}`, i)), d.ID, 8)
	}
	s := &fakeSender{respond: func(egress.Request, int) (egress.Response, error) {
		time.Sleep(30 * time.Millisecond)
		return egress.Response{StatusCode: 200}, nil
	}}
	w := f.worker(t, s, func(dd *delivery.WorkerDeps) { dd.Config.Concurrency, dd.Config.BatchSize = 3, 3 })
	processed := 0
	for processed < 16 {
		processed += f.runOnce(t, w)
	}
	if s.maxSeen.Load() > 3 || s.maxSeen.Load() < 2 {
		t.Fatalf("at most 3 requests may be in flight (saw %d)", s.maxSeen.Load())
	}
	if f.env.Count(t, "SELECT count(*) FROM deliveries WHERE status = 'SUCCEEDED'") != 16 {
		t.Fatal("not every delivery succeeded")
	}
}

func TestRunProcessesWorkAndStopsGracefully(t *testing.T) {
	f := newFixture(t)
	org, user := f.owner.Org.ID, f.owner.Principal.User.ID
	d := f.destination(t, org, user, "D")
	for i := 0; i < 6; i++ {
		f.pending(t, org, f.event(t, fmt.Sprintf(`{"i":%d}`, i)), d.ID, 8)
	}
	s := &fakeSender{}
	w := f.worker(t, s, nil)
	ctx, cancel := context.WithCancel(bg)
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	deadline := time.Now().Add(15 * time.Second)
	for f.env.Count(t, "SELECT count(*) FROM deliveries WHERE status = 'SUCCEEDED'") < 6 {
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("the worker did not process the deliveries")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop after cancellation")
	}
}

func TestShutdownWaitsForTheGracePeriodThenInterruptsRequests(t *testing.T) {
	f := newFixture(t)
	org, user := f.owner.Org.ID, f.owner.Principal.User.ID
	d := f.destination(t, org, user, "D")
	id := f.pending(t, org, f.event(t, `{}`), d.ID, 8)

	entered := make(chan struct{}, 1)
	w := f.worker(t, senderFunc(func(ctx context.Context, req egress.Request) (egress.Response, error) {
		entered <- struct{}{}
		<-ctx.Done() // returns as soon as the worker interrupts it
		return egress.Response{}, &egress.RequestError{Class: egress.ClassCanceled}
	}), nil)

	ctx, cancel := context.WithCancel(bg)
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("the request never started")
	}
	start := time.Now()
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after the grace period")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("shutdown took %v with a grace period of 200ms", time.Since(start))
	}
	// The interrupted request was not recorded as a failure: the claim is left for recovery.
	if st, _, _ := f.state(t, id); st != "IN_FLIGHT" || len(f.attempts(t, id)) != 0 {
		t.Fatalf("interrupted delivery state %s, attempts %d", st, len(f.attempts(t, id)))
	}
}

type senderFunc func(context.Context, egress.Request) (egress.Response, error)

func (s senderFunc) Do(ctx context.Context, r egress.Request) (egress.Response, error) {
	return s(ctx, r)
}

func TestMetricsCountLifecycleEventsWithoutLabels(t *testing.T) {
	f := newFixture(t)
	org, user := f.owner.Org.ID, f.owner.Principal.User.ID
	d := f.destination(t, org, user, "D")
	reg := observability.NewRegistry()
	f.pending(t, org, f.event(t, `{"a":1}`), d.ID, 8)
	f.pending(t, org, f.event(t, `{"a":2}`), d.ID, 8)
	f.pending(t, org, f.event(t, `{"a":3}`), d.ID, 8)
	codes := []int{200, 503, 404}
	s := &fakeSender{respond: func(_ egress.Request, n int) (egress.Response, error) {
		return egress.Response{StatusCode: codes[n-1]}, nil
	}}
	w := f.worker(t, s, func(dd *delivery.WorkerDeps) {
		dd.Metrics = delivery.NewMetrics(reg)
		dd.Config.Concurrency, dd.Config.BatchSize = 1, 1
	})
	for range 3 {
		f.runOnce(t, w)
	}
	var out strings.Builder
	reg.Render(&out)
	for _, want := range []string{
		"relayforge_delivery_claimed_total 3", "relayforge_delivery_succeeded_total 1", "relayforge_delivery_retries_scheduled_total 1",
		"relayforge_delivery_failed_total 1", "relayforge_delivery_attempt_seconds_count 3", "relayforge_delivery_end_to_end_seconds_count 1",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("metrics missing %q:\n%s", want, out.String())
		}
	}
	for _, banned := range []string{org, d.ID, "hooks.example.com", "SECRETTOKEN", d.Secret, "{"} {
		if strings.Contains(strings.ReplaceAll(out.String(), `{le=`, ""), banned) {
			t.Errorf("metrics expose %q", banned)
		}
	}
}

func TestLogsNeverContainSecretsPayloadsOrFullURLs(t *testing.T) {
	f := newFixture(t)
	org, user := f.owner.Org.ID, f.owner.Principal.User.ID
	d := f.destination(t, org, user, "D")
	f.pending(t, org, f.event(t, `{"card":"4111-PAYLOAD-SECRET"}`), d.ID, 8)
	f.pending(t, org, f.event(t, `{"card":"4111-PAYLOAD-SECRET-2"}`), d.ID, 8)
	var logs strings.Builder
	var mu sync.Mutex
	logger := slog.New(slog.NewTextHandler(writerFunc(func(p []byte) (int, error) { mu.Lock(); defer mu.Unlock(); return logs.Write(p) }), &slog.HandlerOptions{Level: slog.LevelDebug}))
	s := &fakeSender{respond: func(_ egress.Request, n int) (egress.Response, error) {
		if n == 1 {
			return egress.Response{}, &egress.RequestError{Class: egress.ClassTLS}
		}
		return egress.Response{StatusCode: 500}, nil
	}}
	w := f.worker(t, s, func(dd *delivery.WorkerDeps) { dd.Log = logger })
	f.runOnce(t, w)
	f.env.Clock.Advance(3 * time.Minute)
	if _, err := w.RecoverExpired(bg); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	out := logs.String()
	mu.Unlock()
	for _, banned := range []string{"PAYLOAD-SECRET", "SECRETTOKEN", d.Secret, "hooks.example.com/relayforge", "v1=", string(apptest.TestKey), "Authorization"} {
		if strings.Contains(out, banned) {
			t.Errorf("logs contain %q", banned)
		}
	}
}

type writerFunc func([]byte) (int, error)

func (w writerFunc) Write(p []byte) (int, error) { return w(p) }

var _ = database.ErrClosed
