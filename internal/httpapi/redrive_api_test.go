package httpapi_test

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Thiago-tods/relayforge/internal/app"
	"github.com/Thiago-tods/relayforge/internal/app/apptest"
	"github.com/Thiago-tods/relayforge/internal/delivery"
	"github.com/Thiago-tods/relayforge/internal/httpapi"
	"github.com/Thiago-tods/relayforge/internal/identity"
)

func redrivePath(org, id string) string {
	return "/api/v1/organizations/" + org + "/deliveries/" + id + "/redrive"
}

func redriveBody(expected int) map[string]any {
	return map[string]any{"reason": "the receiver is back online", "expectedAttemptCount": expected}
}

func keySet(m map[string]any) string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return strings.Join(ks, ",")
}

func TestRedriveAPIRolesAuthenticationAndCSRF(t *testing.T) {
	s := newServer(t, false)
	fs := newFailSeed(t, s, "Target")
	now := s.env.Clock.Now()
	org := fs.is.org

	clients := map[string]*client{"OWNER": fs.is.c}
	for _, role := range []string{"ADMIN", "DEVELOPER", "VIEWER"} {
		clients[role] = join(t, fs.is.c, s, org, strings.ToLower(role)+"@example.com", role)
	}
	for _, role := range []string{"OWNER", "ADMIN", "DEVELOPER"} {
		id := fs.failed(now.Add(-time.Hour), "RETRIES_EXHAUSTED")
		if r := clients[role].mutate("POST", redrivePath(org, id), redriveBody(3)); r.Status != 200 {
			t.Errorf("%s: %d %s", role, r.Status, r.Body)
		}
	}
	id := fs.failed(now.Add(-time.Hour), "RETRIES_EXHAUSTED")
	if r := clients["VIEWER"].mutate("POST", redrivePath(org, id), redriveBody(3)); r.Status != 403 || r.errorCode() != "forbidden" {
		t.Errorf("VIEWER: %d %s", r.Status, r.Body)
	}
	if r := s.client(t).do("POST", redrivePath(org, id), redriveBody(3)); r.Status != 401 || r.errorCode() != "unauthenticated" {
		t.Errorf("anonymous: %d %s", r.Status, r.Body)
	}
	// A session without the CSRF token is refused before anything else.
	if r := fs.is.c.do("POST", redrivePath(org, id), redriveBody(3)); r.Status != 403 || r.errorCode() != "csrf_invalid" {
		t.Errorf("missing CSRF token: %d %s", r.Status, r.Body)
	}
	if r := fs.is.c.do("POST", redrivePath(org, id), redriveBody(3), httpapi.CSRFHeader, "wrong"); r.Status != 403 || r.errorCode() != "csrf_invalid" {
		t.Errorf("wrong CSRF token: %d %s", r.Status, r.Body)
	}
	// A non-member always gets the same 404, whatever the body says, including garbage and a malformed organization.
	stranger := s.client(t)
	stranger.register("stranger@example.com")
	for name, body := range map[string]any{"valid": redriveBody(3), "garbage": `{"reason": 5, "x":`, "empty": map[string]any{}, "unknown field": map[string]any{"nope": 1}} {
		if r := stranger.mutate("POST", redrivePath(org, id), body); r.Status != 404 || r.errorCode() != "not_found" {
			t.Errorf("non-member with %s body: %d %s", name, r.Status, r.Body)
		}
	}
	for _, o := range []string{"not-a-uuid", "00000000-0000-4000-8000-000000000000"} {
		if r := fs.is.c.mutate("POST", redrivePath(o, id), `{garbage`); r.Status != 404 || r.errorCode() != "not_found" {
			t.Errorf("organization %s: %d %s", o, r.Status, r.Body)
		}
	}
	// A member without the permission gets 403 for garbage too (authorization comes before validation).
	if r := clients["VIEWER"].mutate("POST", redrivePath(org, id), `{garbage`); r.Status != 403 {
		t.Errorf("viewer with garbage: %d %s", r.Status, r.Body)
	}
	// Another organization's delivery is not found, and the route is POST only.
	other := newFailSeedAs(t, s, "other@example.com", "Other")
	foreign := other.failed(now.Add(-time.Hour), "RETRIES_EXHAUSTED")
	if r := fs.is.c.mutate("POST", redrivePath(org, foreign), redriveBody(3)); r.Status != 404 || r.errorCode() != "not_found" {
		t.Errorf("foreign delivery: %d %s", r.Status, r.Body)
	}
	for _, m := range []string{"GET", "PUT", "PATCH", "DELETE"} {
		if r := fs.is.c.mutate(m, redrivePath(org, id), nil); r.Status != 404 && r.Status != 405 {
			t.Errorf("%s: %d, want 404 or 405", m, r.Status)
		}
	}
	if s.env.Count(t, "SELECT count(*) FROM deliveries WHERE id = $1 AND status = 'FAILED'", id) != 1 || s.env.Count(t, "SELECT count(*) FROM deliveries WHERE id = $1", foreign) != 1 {
		t.Fatal("a refused request changed a delivery")
	}
}

func TestRedriveAPIContractAndNoSensitiveData(t *testing.T) {
	logs := &lockedBuffer{}
	s := newServerWith(t, false, logs, nil)
	fs := newFailSeed(t, s, "ConfidentialTargetName")
	now := s.env.Clock.Now()
	org := fs.is.org
	id := fs.failed(now.Add(-time.Hour), "DESTINATION_REJECTED")
	legacy := fs.failed(now.Add(-time.Hour), "")

	r := fs.is.c.mutate("POST", redrivePath(org, id), map[string]any{"reason": "SECRETREASONTEXT fixed upstream", "expectedAttemptCount": 3})
	if r.Status != 200 || r.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("%d %v %s", r.Status, r.Header, r.Body)
	}
	body := r.json()
	if keySet(body) != "delivery,redrive" {
		t.Fatalf("response keys: %s", keySet(body))
	}
	d := body["delivery"].(map[string]any)
	if d["status"] != "PENDING" || d["attemptCount"] != float64(3) || d["maxAttempts"] != float64(13) || d["id"] != id {
		t.Errorf("delivery: %v", d)
	}
	for _, absent := range []string{"failureReason", "completedAt"} {
		if _, ok := d[absent]; ok {
			t.Errorf("a PENDING delivery must not have %s", absent)
		}
	}
	if d["nextAttemptAt"] == nil {
		t.Error("nextAttemptAt must be present for a PENDING delivery")
	}
	rd := body["redrive"].(map[string]any)
	if keySet(rd) != "maxAttempts,previousFailureReason,previousMaxAttempts" || rd["previousMaxAttempts"] != float64(8) || rd["maxAttempts"] != float64(13) || rd["previousFailureReason"] != "DESTINATION_REJECTED" {
		t.Errorf("redrive: %v", rd)
	}
	// A legacy delivery reports a null previous reason.
	lr := fs.is.c.mutate("POST", redrivePath(org, legacy), redriveBody(3))
	if lr.Status != 200 || lr.json()["redrive"].(map[string]any)["previousFailureReason"] != nil {
		t.Errorf("legacy: %d %s", lr.Status, lr.Body)
	}
	// The detail now shows the redriven delivery.
	g := fs.is.c.do("GET", "/api/v1/organizations/"+org+"/deliveries/"+id, nil)
	if g.Status != 200 || g.json()["delivery"].(map[string]any)["maxAttempts"] != float64(13) {
		t.Errorf("detail: %d %s", g.Status, g.Body)
	}
	// Nothing sensitive in responses or logs: payload, destination secret and URL query, operator reason.
	for _, text := range []string{r.Body, lr.Body, g.Body, logs.String()} {
		for _, secret := range []string{"PAYLOADSECRET", fs.secret, "QUERYSECRET", "SECRETREASONTEXT"} {
			if strings.Contains(text, secret) {
				t.Errorf("%q leaked: %.200s", secret, text)
			}
		}
	}
	// The reason is in the audit record, which is where it belongs.
	if s.env.Count(t, "SELECT count(*) FROM audit_events WHERE resource_id = $1 AND metadata->>'reason' LIKE 'SECRETREASONTEXT%'", id) != 1 {
		t.Error("the operator reason must be in the audit event")
	}
}

func TestRedriveAPIConflictsAndValidation(t *testing.T) {
	s := newServer(t, false)
	fs := newFailSeed(t, s, "Target")
	now := s.env.Clock.Now()
	org := fs.is.org
	c := fs.is.c

	t.Run("repeated and stale requests", func(t *testing.T) {
		id := fs.failed(now.Add(-time.Hour), "RETRIES_EXHAUSTED")
		if r := c.mutate("POST", redrivePath(org, id), redriveBody(2)); r.Status != 409 || r.errorCode() != "delivery_changed" {
			t.Errorf("stale count: %d %s", r.Status, r.Body)
		}
		if r := c.mutate("POST", redrivePath(org, id), redriveBody(3)); r.Status != 200 {
			t.Fatalf("first: %d %s", r.Status, r.Body)
		}
		if r := c.mutate("POST", redrivePath(org, id), redriveBody(3)); r.Status != 409 || r.errorCode() != "delivery_not_failed" {
			t.Errorf("repeat: %d %s", r.Status, r.Body)
		}
		if s.env.Count(t, "SELECT max_attempts FROM deliveries WHERE id = $1", id) != 13 {
			t.Error("a conflict changed the budget")
		}
	})
	t.Run("delivery in another state", func(t *testing.T) {
		id := fs.failed(now.Add(-time.Hour), "RETRIES_EXHAUSTED")
		s.env.Exec(t, "ALTER TABLE deliveries DISABLE TRIGGER deliveries_enforce_transition")
		s.env.Exec(t, "UPDATE deliveries SET status = 'CANCELLED', failure_reason = NULL WHERE id = $1", id)
		s.env.Exec(t, "ALTER TABLE deliveries ENABLE TRIGGER deliveries_enforce_transition")
		if r := c.mutate("POST", redrivePath(org, id), redriveBody(3)); r.Status != 409 || r.errorCode() != "delivery_not_failed" {
			t.Errorf("cancelled: %d %s", r.Status, r.Body)
		}
	})
	t.Run("budget already at the ceiling", func(t *testing.T) {
		id := fs.failed(now.Add(-time.Hour), "RETRIES_EXHAUSTED")
		s.env.Exec(t, "ALTER TABLE deliveries DISABLE TRIGGER deliveries_enforce_transition")
		s.env.Exec(t, "UPDATE deliveries SET max_attempts = 50 WHERE id = $1", id)
		s.env.Exec(t, "ALTER TABLE deliveries ENABLE TRIGGER deliveries_enforce_transition")
		if r := c.mutate("POST", redrivePath(org, id), redriveBody(3)); r.Status != 409 || r.errorCode() != "retry_budget_exhausted" {
			t.Errorf("budget 50: %d %s", r.Status, r.Body)
		}
	})
	t.Run("revoked destination", func(t *testing.T) {
		other := newFailSeedAs(t, s, "second@example.com", "Gone")
		id := other.failed(now.Add(-time.Hour), "RETRIES_EXHAUSTED")
		if r := other.is.c.mutate("DELETE", "/api/v1/organizations/"+other.is.org+"/destinations/"+other.dest, nil); r.Status != 200 {
			t.Fatalf("revoke: %d %s", r.Status, r.Body)
		}
		if r := other.is.c.mutate("POST", redrivePath(other.is.org, id), redriveBody(3)); r.Status != 409 || r.errorCode() != "destination_revoked" {
			t.Errorf("revoked: %d %s", r.Status, r.Body)
		}
	})
	t.Run("payload unavailable", func(t *testing.T) {
		id := fs.failed(now.Add(-time.Hour), "RETRIES_EXHAUSTED")
		s.env.Exec(t, "UPDATE webhook_events SET payload = '\\x7b7d' WHERE id = (SELECT event_id FROM deliveries WHERE id = $1)", id)
		if r := c.mutate("POST", redrivePath(org, id), redriveBody(3)); r.Status != 409 || r.errorCode() != "event_payload_unavailable" {
			t.Errorf("tampered payload: %d %s", r.Status, r.Body)
		}
		if s.env.Count(t, "SELECT count(*) FROM deliveries WHERE id = $1 AND status = 'FAILED' AND max_attempts = 8", id) != 1 {
			t.Error("an unavailable payload changed the delivery")
		}
	})
	t.Run("validation", func(t *testing.T) {
		id := fs.failed(now.Add(-time.Hour), "RETRIES_EXHAUSTED")
		p := redrivePath(org, id)
		for name, body := range map[string]any{
			"missing expectedAttemptCount": map[string]any{"reason": "ok"},
			"missing reason":               map[string]any{"expectedAttemptCount": 3},
			"blank reason":                 map[string]any{"reason": "   ", "expectedAttemptCount": 3},
			"reason too long":              map[string]any{"reason": strings.Repeat("x", 501), "expectedAttemptCount": 3},
			"control character":            map[string]any{"reason": "a\u0000b", "expectedAttemptCount": 3},
			"count zero":                   map[string]any{"reason": "ok", "expectedAttemptCount": 0},
			"count above 50":               map[string]any{"reason": "ok", "expectedAttemptCount": 51},
			"count negative":               map[string]any{"reason": "ok", "expectedAttemptCount": -3},
		} {
			if r := c.mutate("POST", p, body); r.Status != 400 || r.errorCode() != "validation_failed" {
				t.Errorf("%s: %d %s", name, r.Status, r.Body)
			}
		}
		for name, body := range map[string]any{
			"unknown field":     map[string]any{"reason": "ok", "expectedAttemptCount": 3, "maxAttempts": 50},
			"count as string":   `{"reason":"ok","expectedAttemptCount":"3"}`,
			"not json":          `{"reason":`,
			"two objects":       `{"reason":"ok","expectedAttemptCount":3}{}`,
			"count as fraction": `{"reason":"ok","expectedAttemptCount":3.5}`,
		} {
			if r := c.mutate("POST", p, body); r.Status != 400 || r.errorCode() != "invalid_json" {
				t.Errorf("%s: %d %s", name, r.Status, r.Body)
			}
		}
		if r := c.mutate("POST", p, `{"reason":"`+strings.Repeat("x", 70<<10)+`","expectedAttemptCount":3}`); r.Status != 413 {
			t.Errorf("oversized body: %d", r.Status)
		}
		if r := c.do("POST", p, `{"reason":"ok","expectedAttemptCount":3}`, httpapi.CSRFHeader, c.csrf, "Content-Type", "text/plain"); r.Status != 415 {
			t.Errorf("wrong content type: %d %s", r.Status, r.Body)
		}
		if r := c.mutate("POST", redrivePath(org, "not-a-uuid"), redriveBody(3)); r.Status != 404 || r.errorCode() != "not_found" {
			t.Errorf("bad delivery id: %d %s", r.Status, r.Body)
		}
		if s.env.Count(t, "SELECT count(*) FROM deliveries WHERE id = $1 AND status = 'FAILED' AND max_attempts = 8", id) != 1 {
			t.Error("a rejected request changed the delivery")
		}
	})
}

func TestRedriveAPIReportsALockTimeoutAsServiceUnavailable(t *testing.T) {
	s := newServer(t, false)
	fs := newFailSeed(t, s, "Target")
	id := fs.failed(s.env.Clock.Now().Add(-time.Hour), "RETRIES_EXHAUSTED")

	cfg := app.DefaultConfig()
	cfg.Box = s.env.Box
	cfg.RedriveLockTimeout = 300 * time.Millisecond
	svc, err := app.New(s.env.Pool, identity.NewHasher(apptest.FastArgon2Params), cfg, s.env.Clock.Now, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(httpapi.New(svc, s.env.Ingest, slog.New(slog.NewTextHandler(io.Discard, nil)), false).Handler())
	t.Cleanup(ts.Close)
	c := &client{t: t, s: &server{env: s.env, url: ts.URL}, cookie: fs.is.c.cookie, csrf: fs.is.c.csrf}

	started, done := make(chan struct{}), make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		finished <- s.env.Pool.InTx(context.Background(), func(tx pgx.Tx) error {
			if _, err := tx.Exec(context.Background(), "SELECT id FROM organizations WHERE id = $1 FOR UPDATE", fs.is.org); err != nil {
				return err
			}
			close(started)
			<-done
			return nil
		})
	}()
	<-started
	r := c.mutate("POST", redrivePath(fs.is.org, id), redriveBody(3))
	close(done)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if r.Status != 503 || r.errorCode() != "lock_timeout" || r.Header.Get("Retry-After") == "" {
		t.Fatalf("%d %v %s", r.Status, r.Header, r.Body)
	}
	if s.env.Count(t, "SELECT count(*) FROM deliveries WHERE id = $1 AND status = 'FAILED' AND max_attempts = 8", id) != 1 {
		t.Fatal("a lock timeout changed the delivery")
	}
	if r := c.mutate("POST", redrivePath(fs.is.org, id), redriveBody(3)); r.Status != 200 {
		t.Fatalf("after the lock is gone: %d %s", r.Status, r.Body)
	}
}

// TestAttemptsAPIShowsEveryFailureEpisodeOfARedrivenDelivery redrives a delivery through the API, lets it fail a second
// time through the real store functions, and reads both feeds over HTTP: the attempt feed lists the attempts of both
// episodes (each terminal attempt with its own terminalReason), and the failure feed lists the delivery only while its
// latest episode is a current failure.
func TestAttemptsAPIShowsEveryFailureEpisodeOfARedrivenDelivery(t *testing.T) {
	s := newServer(t, false)
	fs := newFailSeed(t, s, "Target")
	now := s.env.Clock.Now().Truncate(time.Microsecond)
	org := fs.is.org
	id := fs.failed(now.Add(-time.Hour), "RETRIES_EXHAUSTED") // attempt_count 3
	fs.addAttempt(id, 1, now.Add(-3*time.Hour), "HTTP_ERROR", 503, "http_5xx", nil)
	fs.addAttempt(id, 2, now.Add(-2*time.Hour), "HTTP_ERROR", 503, "http_5xx", nil)
	fs.addAttempt(id, 3, now.Add(-time.Hour), "HTTP_ERROR", 503, "http_5xx", "RETRIES_EXHAUSTED")
	attempts := "/api/v1/organizations/" + org + "/operations/attempts?deliveryId=" + id
	failures := "/api/v1/organizations/" + org + "/operations/failures"

	numbers := func(r response) (nums []float64, reasons []any) {
		for _, it := range r.json()["items"].([]any) {
			m := it.(map[string]any)
			nums = append(nums, m["attemptNumber"].(float64))
			reasons = append(reasons, m["terminalReason"])
		}
		return
	}
	if r := fs.is.c.do("GET", failures, nil); r.Status != 200 || len(itemIDs(r)) != 1 {
		t.Fatalf("before the redrive: %d %s", r.Status, r.Body)
	}

	if r := fs.is.c.mutate("POST", redrivePath(org, id), redriveBody(3)); r.Status != 200 {
		t.Fatalf("redrive: %d %s", r.Status, r.Body)
	}
	if r := fs.is.c.do("GET", failures, nil); r.Status != 200 || len(itemIDs(r)) != 0 {
		t.Fatalf("a redriven delivery is not a current failure: %d %s", r.Status, r.Body)
	}
	if r := fs.is.c.do("GET", attempts, nil); r.Status != 200 {
		t.Fatalf("%d %s", r.Status, r.Body)
	} else if nums, _ := numbers(r); len(nums) != 3 {
		t.Fatalf("a redrive adds no attempt rows: %v", nums)
	}

	// The second failure episode: attempt 4 is claimed and recorded by the store, as the worker does.
	q, _ := s.env.Pool.Querier()
	var token string
	if err := q.QueryRow(bg, `UPDATE deliveries SET status = 'IN_FLIGHT', attempt_count = attempt_count + 1, lease_expires_at = now() + interval '1 minute',
		claim_token = gen_random_uuid(), claimed_by = 'test' WHERE id = $1 RETURNING claim_token::text`, id).Scan(&token); err != nil {
		t.Fatal(err)
	}
	rec := delivery.AttemptRecord{Number: 4, StartedAt: now.Add(-10 * time.Minute), FinishedAt: now.Add(-9 * time.Minute), Outcome: delivery.OutcomeHTTPError, HTTPStatus: 410}
	dec := delivery.Decision{Status: delivery.StatusFailed, FailureReason: delivery.FailureDestinationRejected}
	if err := s.env.Pool.InTx(bg, func(tx pgx.Tx) error {
		return delivery.Finish(bg, tx, org, id, token, rec, dec, now.Add(-9*time.Minute))
	}); err != nil {
		t.Fatal(err)
	}

	r := fs.is.c.do("GET", attempts, nil)
	if r.Status != 200 {
		t.Fatalf("%d %s", r.Status, r.Body)
	}
	nums, reasons := numbers(r)
	if len(nums) != 4 || nums[0] != 4 || nums[1] != 3 || nums[2] != 2 || nums[3] != 1 {
		t.Fatalf("both episodes, newest first, no gaps: %v", nums)
	}
	if reasons[0] != "DESTINATION_REJECTED" || reasons[1] != "RETRIES_EXHAUSTED" || reasons[2] != nil || reasons[3] != nil {
		t.Fatalf("each failure episode ends with its own terminalReason: %v", reasons)
	}
	first := r.json()["items"].([]any)[0].(map[string]any)
	if keysOf(first) != "attemptId,attemptNumber,deliveryId,destinationId,destinationName,durationMs,eventId,finishedAt,httpStatus,outcome,startedAt,terminalReason" || first["deliveryId"] != id {
		t.Errorf("item shape: %v", first)
	}
	if r := fs.is.c.do("GET", failures, nil); r.Status != 200 || len(itemIDs(r)) != 1 || r.json()["items"].([]any)[0].(map[string]any)["attemptCount"] != float64(4) ||
		r.json()["items"].([]any)[0].(map[string]any)["failureReason"] != "DESTINATION_REJECTED" {
		t.Fatalf("after the second failure the delivery is a current failure again, with the latest reason: %d %s", r.Status, r.Body)
	}
	// Paging through the episodes with a small limit sees every attempt exactly once.
	var seen []float64
	cursor := ""
	for page := 0; page < 5; page++ {
		path := attempts + "&limit=2"
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		pr := fs.is.c.do("GET", path, nil)
		n, _ := numbers(pr)
		seen = append(seen, n...)
		next, _ := pr.json()["nextCursor"].(string)
		if next == "" {
			break
		}
		cursor = next
	}
	if len(seen) != 4 || seen[0] != 4 || seen[3] != 1 {
		t.Fatalf("paging across episodes: %v", seen)
	}
}
