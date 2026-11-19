package httpapi_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Thiago-tods/relayforge/internal/app"
	"github.com/Thiago-tods/relayforge/internal/app/apptest"
	"github.com/Thiago-tods/relayforge/internal/httpapi"
	"github.com/Thiago-tods/relayforge/internal/identity"
)

func keysOf(m map[string]any) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

func TestOperationsAPIRolesAuthenticationAndReadOnlyRoutes(t *testing.T) {
	s := newServer(t, false)
	is := newIngestSetup(t, s)
	org := "/api/v1/organizations/" + is.org
	paths := []string{org + "/operations/queue", org + "/operations/deliveries"}

	// Every role that holds deliveries:read may read both models.
	clients := map[string]*client{"OWNER": is.c}
	for _, role := range []string{"ADMIN", "DEVELOPER", "VIEWER"} {
		clients[role] = join(t, is.c, s, is.org, strings.ToLower(role)+"@example.com", role)
	}
	for role, c := range clients {
		for _, p := range paths {
			if r := c.do("GET", p, nil); r.Status != 200 {
				t.Errorf("%s GET %s: %d %s", role, p, r.Status, r.Body)
			}
		}
	}

	// No session: unauthenticated, and the session is not created by the read.
	for _, p := range paths {
		if r := s.client(t).do("GET", p, nil); r.Status != 401 || r.errorCode() != "unauthenticated" {
			t.Errorf("anonymous GET %s: %d %s", p, r.Status, r.Body)
		}
	}

	// A user who is not a member, and a malformed organization id, get the same 404.
	stranger := s.client(t)
	stranger.register("stranger@example.com")
	for _, p := range paths {
		if r := stranger.do("GET", p, nil); r.Status != 404 || r.errorCode() != "not_found" {
			t.Errorf("non-member GET %s: %d %s", p, r.Status, r.Body)
		}
	}
	for _, tail := range []string{"/operations/queue", "/operations/deliveries"} {
		for _, id := range []string{"not-a-uuid", "00000000-0000-4000-8000-000000000000"} {
			if r := is.c.do("GET", "/api/v1/organizations/"+id+tail, nil); r.Status != 404 || r.errorCode() != "not_found" {
				t.Errorf("GET %s%s: %d %s", id, tail, r.Status, r.Body)
			}
		}
	}

	// The models are read-only: no other verb is routed.
	before := s.env.Count(t, "SELECT count(*) FROM deliveries")
	for _, m := range []string{"POST", "PUT", "PATCH", "DELETE"} {
		for _, p := range paths {
			if r := is.c.mutate(m, p, map[string]string{"status": "FAILED"}); r.Status != 404 && r.Status != 405 {
				t.Errorf("%s %s: %d, want 404 or 405", m, p, r.Status)
			}
		}
	}
	if s.env.Count(t, "SELECT count(*) FROM deliveries") != before {
		t.Fatal("deliveries changed through the operations API")
	}
}

func TestOperationsAPIReportsRealDeliveriesWithoutSensitiveFields(t *testing.T) {
	logs := &lockedBuffer{}
	s := newServerWith(t, false, logs, nil)
	is := newIngestSetup(t, s)
	org := "/api/v1/organizations/" + is.org
	destID, destSecret := createDestination(t, is.c, is.org, "ConfidentialTargetName")
	var ownerID string
	if err := mustQuery(s, "SELECT id::text FROM users LIMIT 1", &ownerID); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	// Two deliveries that exhausted their attempts, created one hour ago.
	seedHTTPDelivery(t, s.env, is.org, ownerID, is.pub, is.secret, destID, 2, now.Add(-time.Hour))
	seedHTTPDelivery(t, s.env, is.org, ownerID, is.pub, is.secret, destID, 1, now.Add(-time.Hour))
	// One delivery still waiting (a pending retry an hour from now is delayed, not due).
	seedHTTPDelivery(t, s.env, is.org, ownerID, is.pub, is.secret, destID, 0, now.Add(-time.Hour))

	window := "?from=" + now.Add(-2*time.Hour).Format(time.RFC3339) + "&to=" + now.Add(time.Hour).Format(time.RFC3339)
	sum := is.c.do("GET", org+"/operations/deliveries"+window, nil)
	if sum.Status != 200 {
		t.Fatalf("summary: %d %s", sum.Status, sum.Body)
	}
	body := sum.json()
	if keysOf(body) != "capped,completed,created,from,generatedAt,to" {
		t.Fatalf("summary fields: %s", sum.Body)
	}
	created := body["created"].(map[string]any)
	if created["total"] != float64(3) || keysOf(created["byStatus"].(map[string]any)) != "CANCELLED,FAILED,IN_FLIGHT,PENDING,SUCCEEDED" {
		t.Fatalf("created: %v", created)
	}
	if by := created["byStatus"].(map[string]any); by["FAILED"] != float64(2) || by["PENDING"] != float64(1) || by["SUCCEEDED"] != float64(0) {
		t.Fatalf("created by status: %v", by)
	}
	completed := body["completed"].(map[string]any)
	if keysOf(completed) != "byFailureReason,cancelled,failed,failureRate,succeeded" || completed["failed"] != float64(2) || completed["failureRate"] != float64(1) {
		t.Fatalf("completed: %v", completed)
	}
	reasons := completed["byFailureReason"].(map[string]any)
	if keysOf(reasons) != "DESTINATION_REJECTED,EGRESS_BLOCKED,INTERNAL_ERROR,REDIRECT_REFUSED,RETRIES_EXHAUSTED,UNRECORDED" || reasons["RETRIES_EXHAUSTED"] != float64(2) {
		t.Fatalf("failure reasons: %v", reasons)
	}
	if body["capped"] != false {
		t.Fatalf("capped: %v", body["capped"])
	}

	queue := is.c.do("GET", org+"/operations/queue", nil)
	if queue.Status != 200 || keysOf(queue.json()) != "capped,delayed,due,expiredLeases,generatedAt,inFlight,oldestDueAgeSeconds,pending" {
		t.Fatalf("queue: %d %s", queue.Status, queue.Body)
	}
	if q := queue.json(); q["pending"] != float64(1) || q["capped"] != false || q["inFlight"] != float64(0) || q["expiredLeases"] != float64(0) {
		t.Fatalf("queue numbers: %v", q)
	}

	// Aggregates only: nothing a delivery, destination or event carries may appear.
	for _, r := range []response{sum, queue} {
		for _, banned := range []string{destSecret, destID, "ConfidentialTargetName", "PAYLOADSECRET", "hooks.example.com", "QUERYSECRET", "token=",
			"claimToken", "claim_token", "claimedBy", "leaseExpiresAt", "payload", "signingSecret", "ciphertext", "sql", "pq:", "pgx", "organization_id"} {
			if strings.Contains(r.Body, banned) {
				t.Errorf("operations response exposes %q: %s", banned, r.Body)
			}
		}
		if cc := r.Header.Get("Cache-Control"); cc != "no-store" {
			t.Errorf("Cache-Control = %q, want no-store", cc)
		}
	}
	if out := logs.String(); strings.Contains(out, destSecret) || strings.Contains(out, "QUERYSECRET") {
		t.Fatal("server logs contain a secret")
	}
}

func TestOperationsAPIWindowValidationDefaultsAndOffsets(t *testing.T) {
	s := newServer(t, false)
	is := newIngestSetup(t, s)
	path := "/api/v1/organizations/" + is.org + "/operations/deliveries"
	const base = "2026-11-18T13:00:00Z"
	at := func(d time.Duration) string {
		ts, _ := time.Parse(time.RFC3339, base)
		return ts.Add(d).Format(time.RFC3339)
	}

	for name, q := range map[string]string{
		"from is not a timestamp":      "?from=yesterday",
		"to is not a timestamp":        "?to=2026-13-45",
		"a date without a time":        "?from=2026-11-18&to=2026-11-19",
		"from after to":                "?from=" + at(time.Hour) + "&to=" + at(0),
		"an empty window":              "?from=" + at(0) + "&to=" + at(0),
		"one second over 31 days":      "?from=" + at(0) + "&to=" + at(31*24*time.Hour+time.Second),
		"far over 31 days":             "?from=2020-01-01T00:00:00Z&to=2026-01-01T00:00:00Z",
		"an empty from":                "?from=%20",
		"equal after offset":           "?from=2026-11-18T10:00:00-03:00&to=2026-11-18T13:00:00Z",
		"inverted across offsets":      "?from=2026-11-18T14:00:00Z&to=2026-11-18T10:00:00-03:00",
		"a default window before from": "?from=2999-01-01T00:00:00Z",
	} {
		if r := is.c.do("GET", path+q, nil); r.Status != 400 || r.errorCode() != "validation_failed" {
			t.Errorf("%s: %d %s, want 400 validation_failed", name, r.Status, r.Body)
		}
	}

	// The longest allowed window is exactly 31 days.
	if r := is.c.do("GET", path+"?from="+at(0)+"&to="+at(31*24*time.Hour), nil); r.Status != 200 {
		t.Errorf("exactly 31 days: %d %s", r.Status, r.Body)
	}
	// RFC 3339 offsets are honored and the response is in UTC.
	r := is.c.do("GET", path+"?from=2026-11-18T10:00:00-03:00&to=2026-11-18T16:00:00%2B02:00", nil)
	if r.Status != 200 || r.json()["from"] != "2026-11-18T13:00:00Z" || r.json()["to"] != "2026-11-18T14:00:00Z" {
		t.Errorf("offsets: %d %s", r.Status, r.Body)
	}
	// With no parameters the window is the last 24 hours up to the service clock.
	d := is.c.do("GET", path, nil)
	from, _ := time.Parse(time.RFC3339Nano, d.json()["from"].(string))
	to, _ := time.Parse(time.RFC3339Nano, d.json()["to"].(string))
	generated, _ := time.Parse(time.RFC3339Nano, d.json()["generatedAt"].(string))
	if d.Status != 200 || to.Sub(from) != 24*time.Hour || !to.Equal(generated) {
		t.Errorf("default window: %d %s", d.Status, d.Body)
	}
	// Only `to` given: the window is the 24 hours before it. Only `from`: it ends at now.
	if r := is.c.do("GET", path+"?to="+base, nil); r.Status != 200 || r.json()["from"] != "2026-11-17T13:00:00Z" {
		t.Errorf("only to: %d %s", r.Status, r.Body)
	}
	// Parameters this route does not know are ignored, like everywhere else in the API.
	if r := is.c.do("GET", path+"?limit=9999&offset=-1&status=NOPE&unknown=1", nil); r.Status != 200 {
		t.Errorf("unknown parameters: %d %s", r.Status, r.Body)
	}
	// The queue takes no parameters at all.
	if r := is.c.do("GET", "/api/v1/organizations/"+is.org+"/operations/queue?from=garbage", nil); r.Status != 200 {
		t.Errorf("queue ignores parameters: %d %s", r.Status, r.Body)
	}
}

func TestOperationsAPIIsolatesOrganizations(t *testing.T) {
	s := newServer(t, false)
	is := newIngestSetup(t, s)
	org := "/api/v1/organizations/" + is.org
	destID, _ := createDestination(t, is.c, is.org, "Mine")
	var ownerID string
	if err := mustQuery(s, "SELECT id::text FROM users LIMIT 1", &ownerID); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	seedHTTPDelivery(t, s.env, is.org, ownerID, is.pub, is.secret, destID, 1, now.Add(-time.Hour))
	window := "?from=" + now.Add(-2*time.Hour).Format(time.RFC3339) + "&to=" + now.Add(time.Hour).Format(time.RFC3339)

	other := s.client(t)
	otherOrg := other.orgID(other.register("other@example.com"))
	otherBase := "/api/v1/organizations/" + otherOrg

	// The other organization has no deliveries: its own numbers are zero, not ours.
	r := other.do("GET", otherBase+"/operations/deliveries"+window, nil)
	if r.Status != 200 || r.json()["created"].(map[string]any)["total"] != float64(0) || r.json()["completed"].(map[string]any)["failed"] != float64(0) {
		t.Fatalf("the other organization's summary: %d %s", r.Status, r.Body)
	}
	if q := other.do("GET", otherBase+"/operations/queue", nil); q.Status != 200 || q.json()["pending"] != float64(0) {
		t.Fatalf("the other organization's queue: %d %s", q.Status, q.Body)
	}
	// Our organization's id with the other session is a plain 404, for both models.
	for _, p := range []string{org + "/operations/queue", org + "/operations/deliveries" + window} {
		if r := other.do("GET", p, nil); r.Status != 404 || r.errorCode() != "not_found" {
			t.Errorf("foreign GET %s: %d %s", p, r.Status, r.Body)
		}
	}
	// And ours still sees its own delivery.
	if mine := is.c.do("GET", org+"/operations/deliveries"+window, nil); mine.json()["created"].(map[string]any)["total"] != float64(1) {
		t.Fatalf("our summary: %s", mine.Body)
	}
}

// shortDeadlineServer serves the same database as s through a service whose operational reads give
// up after d. Sessions live in the database, so the clients of s work against it.
func shortDeadlineServer(t *testing.T, s *server, d time.Duration, logs io.Writer) *server {
	t.Helper()
	cfg := app.DefaultConfig()
	cfg.Box = s.env.Box
	cfg.OperationsDeadline = d
	svc, err := app.New(s.env.Pool, identity.NewHasher(apptest.FastArgon2Params), cfg, s.env.Clock.Now, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	api := httpapi.New(svc, s.env.Ingest, slog.New(slog.NewTextHandler(logs, nil)), false)
	ts := httptest.NewServer(api.Handler())
	t.Cleanup(ts.Close)
	return &server{env: s.env, url: ts.URL}
}

func lockDeliveries(t *testing.T, s *server, fn func()) {
	t.Helper()
	err := s.env.Pool.InTx(context.Background(), func(tx pgx.Tx) error {
		if _, err := tx.Exec(context.Background(), "LOCK TABLE deliveries IN ACCESS EXCLUSIVE MODE"); err != nil {
			return err
		}
		fn()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestOperationsAPIAnOverlongReadIs503WithoutDatabaseDetail makes the read genuinely wait (the
// deliveries table is locked) until the deadline cancels it.
func TestOperationsAPIAnOverlongReadIs503WithoutDatabaseDetail(t *testing.T) {
	logs := &lockedBuffer{}
	s := newServer(t, false)
	is := newIngestSetup(t, s)
	fast := shortDeadlineServer(t, s, 300*time.Millisecond, logs)
	// The same session, against the server with the short deadline.
	c := &client{t: t, s: fast, cookie: is.c.cookie, csrf: is.c.csrf}
	org := "/api/v1/organizations/" + is.org

	lockDeliveries(t, s, func() {
		for _, p := range []string{org + "/operations/queue", org + "/operations/deliveries", org + "/operations/deliveries?from=2026-11-01T00:00:00Z&to=2026-11-30T00:00:00Z"} {
			start := time.Now()
			r := c.do("GET", p, nil)
			if r.Status != 503 || r.errorCode() != "read_model_timeout" {
				t.Errorf("GET %s: %d %s, want 503 read_model_timeout", p, r.Status, r.Body)
			}
			if took := time.Since(start); took < 250*time.Millisecond || took > 5*time.Second {
				t.Errorf("GET %s answered after %v, want about the 300ms deadline", p, took)
			}
			if cc := r.Header.Get("Cache-Control"); cc != "no-store" {
				t.Errorf("Cache-Control = %q", cc)
			}
			e := r.json()["error"].(map[string]any)
			if e["message"] != "the query took too long; try a shorter time window" || len(e) != 2 {
				t.Errorf("the error carries only the code and the safe message: %s", r.Body)
			}
			for _, banned := range []string{"SQLSTATE", "57014", "pgx", "deadline", "context", "lock", "deliveries", "SELECT", "organization_id", "canceling", is.org} {
				if strings.Contains(r.Body, banned) {
					t.Errorf("the 503 exposes %q: %s", banned, r.Body)
				}
			}
		}
	})
	if out := logs.String(); strings.Contains(out, "SELECT") {
		t.Errorf("a timeout must not log SQL text: %s", out)
	}
	// Once the lock is gone the same server and session answer normally.
	if r := c.do("GET", org+"/operations/queue", nil); r.Status != 200 {
		t.Fatalf("after the lock is released: %d %s", r.Status, r.Body)
	}
}

// TestOperationsAPIAClientThatGoesAwayIsNotAnOperationsTimeout cancels the HTTP request while the
// read waits. The established API convention applies (a cancelled request is not a 503
// read_model_timeout), and the server stays healthy.
func TestOperationsAPIAClientThatGoesAwayIsNotAnOperationsTimeout(t *testing.T) {
	logs := &lockedBuffer{}
	s := newServer(t, false)
	is := newIngestSetup(t, s)
	slow := shortDeadlineServer(t, s, time.Minute, logs)
	c := &client{t: t, s: slow, cookie: is.c.cookie, csrf: is.c.csrf}
	url := slow.url + "/api/v1/organizations/" + is.org + "/operations/deliveries"

	lockDeliveries(t, s, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
		req.AddCookie(is.c.cookie)
		start := time.Now()
		resp, err := http.DefaultClient.Do(req)
		if resp != nil {
			resp.Body.Close()
		}
		if err == nil || !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 10*time.Second {
			t.Fatalf("the client gives up first: err = %v after %v", err, time.Since(start))
		}
	})
	// The abandoned read released its connection and the server still answers.
	if r := c.do("GET", "/api/v1/organizations/"+is.org+"/operations/queue", nil); r.Status != 200 {
		t.Fatalf("after the client went away: %d %s", r.Status, r.Body)
	}
	if strings.Contains(logs.String(), "read_model_timeout") {
		t.Errorf("a client that goes away must not be reported as a timeout: %s", logs.String())
	}
}

func TestOperationsAPITimestampLimitsReturn400BeforeAnyRead(t *testing.T) {
	s := newServer(t, false)
	is := newIngestSetup(t, s)
	path := "/api/v1/organizations/" + is.org + "/operations/deliveries"
	for name, q := range map[string]string{
		"year 0000":                       "?from=0000-01-01T00:00:00Z&to=0000-01-02T00:00:00Z",
		"year 0000 only in from":          "?from=0000-12-31T00:00:00Z&to=0001-01-01T00:00:00Z",
		"an offset leaves year 0001":      "?from=0001-01-01T00:00:00%2B14:00&to=0001-01-02T00:00:00Z",
		"an offset enters year 10000":     "?from=9999-12-31T00:00:00Z&to=9999-12-31T23:59:59-05:00",
		"a default from before year 0001": "?to=0001-01-01T12:00:00Z",
		"five digit year":                 "?from=10000-01-01T00:00:00Z",
		"the widest possible span":        "?from=0001-01-01T00:00:00Z&to=9999-12-31T23:59:59Z",
		"a leap second":                   "?from=2026-06-30T23:59:60Z",
		"a unix epoch":                    "?from=0",
		"an invalid offset":               "?from=2026-11-18T10:00:00%2B25:00",
	} {
		if r := is.c.do("GET", path+q, nil); r.Status != 400 || r.errorCode() != "validation_failed" {
			t.Errorf("%s: %d %s, want 400 validation_failed", name, r.Status, r.Body)
		}
	}
	// The limits of the supported range are accepted and PostgreSQL handles them.
	for name, q := range map[string]string{
		"the first supported day":            "?from=0001-01-01T00:00:00Z&to=0001-01-02T00:00:00Z",
		"the last supported day":             "?from=9999-12-30T00:00:00Z&to=9999-12-31T23:59:59.999999Z",
		"31 days at the end of year 9999":    "?from=9999-11-30T23:59:59.999999Z&to=9999-12-31T23:59:59.999999Z",
		"extreme offsets that stay in range": "?from=2026-11-18T00:00:00%2B14:00&to=2026-11-18T00:00:00-12:00",
		"microsecond precision":              "?from=2026-11-18T10:00:00.000001Z&to=2026-11-18T10:00:00.000002Z",
	} {
		r := is.c.do("GET", path+q, nil)
		if r.Status != 200 {
			t.Errorf("%s: %d %s, want 200", name, r.Status, r.Body)
		}
	}
	// Sub-microsecond input is normalized, and the response echoes the window that was read.
	r := is.c.do("GET", path+"?from=2026-11-18T10:00:00.1234567Z&to=2026-11-18T11:00:00%2B00:00", nil)
	if r.Status != 200 || r.json()["from"] != "2026-11-18T10:00:00.123456Z" || r.json()["to"] != "2026-11-18T11:00:00Z" {
		t.Errorf("normalization: %d %s", r.Status, r.Body)
	}
	// A non-member sees 404 whatever the window is.
	stranger := s.client(t)
	stranger.register("stranger@example.com")
	if r := stranger.do("GET", path+"?from=0000-01-01T00:00:00Z", nil); r.Status != 404 || r.errorCode() != "not_found" {
		t.Errorf("a non-member with an invalid window: %d %s", r.Status, r.Body)
	}
}
