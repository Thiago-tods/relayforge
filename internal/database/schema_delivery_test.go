package database_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pressly/goose/v3"

	"github.com/Thiago-tods/relayforge/internal/app/apptest"
	"github.com/Thiago-tods/relayforge/internal/database"
	"github.com/Thiago-tods/relayforge/internal/delivery"
)

// deliveryFixture extends the ingestion fixture with one destination per organization.
type deliveryFixture struct {
	ingestFixture
	dest1, dest2 string
	keySeq       atomic.Int64
}

const validURL = "https://hooks.example.com/relayforge/abc?token=t"

func newDeliveryFixture(t *testing.T) *deliveryFixture {
	t.Helper()
	f := &deliveryFixture{ingestFixture: newIngestFixture(t)}
	f.dest1 = f.insertDestination(t, f.orgID, f.userID, "Primary", validURL)
	f.dest2 = f.insertDestination(t, f.org2, f.user2, "Primary", validURL)
	return f
}

const destinationInsert = "INSERT INTO destinations (id, organization_id, name, url, status, signing_secret_ciphertext, " +
	"encryption_key_version, secret_version, created_by_user_id, revoked_at) " +
	"VALUES (gen_random_uuid(), $1, $2, $3, $4, %s, $5, $6, $7, $8)"

func (f *deliveryFixture) destinationSQL(cipherLiteral string) string {
	return strings.Replace(destinationInsert, "%s", cipherLiteral, 1)
}

func (f *deliveryFixture) insertDestination(t *testing.T, org, user, name, url string) string {
	t.Helper()
	q, _ := f.env.Pool.Querier()
	var id string
	err := q.QueryRow(context.Background(), f.destinationSQL(cipher)+" RETURNING id", org, name, url, "ACTIVE", 1, 1, user, nil).Scan(&id)
	if err != nil {
		t.Fatalf("insert destination: %v", err)
	}
	return id
}

// newEvent inserts a webhook event for an endpoint and returns its id.
func (f *deliveryFixture) newEvent(t *testing.T, org, endpoint string) string {
	t.Helper()
	q, _ := f.env.Pool.Querier()
	var id string
	key := fmt.Sprintf("evt-%d", f.keySeq.Add(1))
	if err := q.QueryRow(context.Background(), f.insertEvent()+" RETURNING id", org, endpoint, key, []byte(`{}`)).Scan(&id); err != nil {
		t.Fatalf("insert event: %v", err)
	}
	return id
}

// dv describes a delivery row. Zero values mean "database default" or NULL.
type dv struct {
	org, event, dest, status string
	attempts, max            int
	lease, token, by         string // SQL fragments, e.g. "now() + interval '1 minute'"
	httpStatus               int
	completed                string
	// reason is a failure_reason SQL fragment. A FAILED row without one gets a valid default,
	// unless legacy is set: a legacy row models a delivery that failed before migration 00019.
	reason string
	legacy bool
}

func (d dv) sql() string {
	nz := func(s string) string {
		if s == "" {
			return "NULL"
		}
		return s
	}
	status := d.status
	if status == "" {
		status = "PENDING"
	}
	max := d.max
	if max == 0 {
		max = 8
	}
	reason := d.reason
	if reason == "" && status == "FAILED" && !d.legacy {
		reason = "'DESTINATION_REJECTED'"
	}
	http := "NULL"
	if d.httpStatus != 0 {
		http = fmt.Sprint(d.httpStatus)
	}
	return fmt.Sprintf("INSERT INTO deliveries (organization_id, event_id, destination_id, status, attempt_count, max_attempts, "+
		"lease_expires_at, claim_token, claimed_by, last_http_status, completed_at, failure_reason) "+
		"VALUES ($1, $2, $3, '%s', %d, %d, %s, %s, %s, %s, %s, %s)",
		status, d.attempts, max, nz(d.lease), nz(d.token), nz(d.by), http, nz(d.completed), nz(reason))
}

func (f *deliveryFixture) insertDelivery(t *testing.T, d dv) (string, error) {
	t.Helper()
	q, _ := f.env.Pool.Querier()
	var id string
	err := q.QueryRow(context.Background(), d.sql()+" RETURNING id", d.org, d.event, d.dest).Scan(&id)
	return id, err
}

func (f *deliveryFixture) mustDelivery(t *testing.T, d dv) string {
	t.Helper()
	id, err := f.insertDelivery(t, d)
	if err != nil {
		t.Fatalf("insert delivery: %v", err)
	}
	return id
}

// pending returns a minimal valid PENDING delivery for the first organization.
func (f *deliveryFixture) pending(t *testing.T) dv {
	return dv{org: f.orgID, event: f.newEvent(t, f.orgID, f.ep1), dest: f.dest1}
}

func (f *deliveryFixture) rejectDelivery(t *testing.T, name string, d dv, wantInError string) {
	t.Helper()
	_, err := f.insertDelivery(t, d)
	if err == nil {
		t.Errorf("%s: the delivery was accepted but must be rejected", name)
		return
	}
	if wantInError != "" && !strings.Contains(err.Error(), wantInError) {
		t.Errorf("%s: rejected by the wrong rule: %v (want %q)", name, err, wantInError)
	}
}

const (
	inFlightLease = "now() + interval '1 minute'"
	tokenExpr     = "gen_random_uuid()"
)

func TestDestinationConstraints(t *testing.T) {
	f := newDeliveryFixture(t)
	ins := f.destinationSQL(cipher)
	row := func(org, name, url, status string, keyVersion, secretVersion int, user string, revokedAt any) []any {
		return []any{org, name, url, status, keyVersion, secretVersion, user, revokedAt}
	}
	ok := func(name, url string) []any { return row(f.orgID, name, url, "ACTIVE", 1, 1, f.userID, nil) }

	f.accept(t, "valid destination", ins, ok("Second", "https://example.org:8443/path?x=1")...)
	f.accept(t, "host-only URL", ins, ok("Third", "https://example.org")...)

	for name, url := range map[string]string{
		"http scheme":         "http://example.com/hook",
		"no scheme":           "example.com/hook",
		"userinfo":            "https://user:pass@example.com/hook",
		"userinfo without pw": "https://user@example.com/hook",
		"uppercase host":      "https://Example.com/hook",
		"fragment":            "https://example.com/hook#frag",
		"whitespace":          "https://example.com/a b",
		"control character":   "https://example.com/a\tb",
		"backslash":           "https://example.com\\@evil.com/",
		"non-ascii":           "https://exämple.com/",
		"ipv6 literal":        "https://[::1]/",
		"empty host":          "https:///path",
		"leading hyphen":      "https://-bad.example.com/",
		"trailing dot host":   "https://example.com./x",
		"port too long":       "https://example.com:123456/",
		"empty":               "",
		"too long":            "https://example.com/" + strings.Repeat("a", 2100),
	} {
		f.reject(t, "url: "+name, ins, ok("Name "+name, url)...)
	}

	f.reject(t, "empty name", ins, ok("", validURL)...)
	f.reject(t, "name over 100 characters", ins, ok(strings.Repeat("n", 101), validURL)...)
	f.reject(t, "invalid status", ins, row(f.orgID, "S1", validURL, "DELETED", 1, 1, f.userID, nil)...)
	f.reject(t, "lowercase status", ins, row(f.orgID, "S2", validURL, "active", 1, 1, f.userID, nil)...)
	f.reject(t, "revoked without revoked_at", ins, row(f.orgID, "S3", validURL, "REVOKED", 1, 1, f.userID, nil)...)
	f.reject(t, "active with revoked_at", ins, row(f.orgID, "S4", validURL, "ACTIVE", 1, 1, f.userID, "2026-01-01T00:00:00Z")...)
	f.accept(t, "revoked with revoked_at", ins, row(f.orgID, "S5", validURL, "REVOKED", 1, 1, f.userID, "2026-01-01T00:00:00Z")...)
	f.accept(t, "paused", ins, row(f.orgID, "S6", validURL, "PAUSED", 1, 1, f.userID, nil)...)

	// Secret material: ciphertext and key version constraints.
	f.reject(t, "ciphertext shorter than nonce plus tag", f.destinationSQL("'\\x0011'"), ok("C1", validURL)...)
	f.reject(t, "empty ciphertext", f.destinationSQL("''::bytea"), ok("C2", validURL)...)
	f.reject(t, "null ciphertext", f.destinationSQL("NULL"), ok("C3", validURL)...)
	f.reject(t, "key version zero", ins, row(f.orgID, "C4", validURL, "ACTIVE", 0, 1, f.userID, nil)...)
	f.reject(t, "negative key version", ins, row(f.orgID, "C5", validURL, "ACTIVE", -1, 1, f.userID, nil)...)
	f.reject(t, "secret version zero", ins, row(f.orgID, "C6", validURL, "ACTIVE", 1, 0, f.userID, nil)...)

	// Tenancy and references.
	f.reject(t, "unknown organization", ins, row("00000000-0000-4000-8000-000000000000", "T1", validURL, "ACTIVE", 1, 1, f.userID, nil)...)
	f.reject(t, "unknown creator", ins, row(f.orgID, "T2", validURL, "ACTIVE", 1, 1, "00000000-0000-4000-8000-000000000000", nil)...)
}

func TestDestinationNamesAreUniquePerOrganizationWhileNotRevoked(t *testing.T) {
	f := newDeliveryFixture(t)
	ins := f.destinationSQL(cipher)
	args := func(org, name, status string, revokedAt any) []any {
		user := f.userID
		if org == f.org2 {
			user = f.user2
		}
		return []any{org, name, validURL, status, 1, 1, user, revokedAt}
	}
	f.reject(t, "duplicate name", ins, args(f.orgID, "Primary", "ACTIVE", nil)...)
	f.reject(t, "duplicate name differing in case", ins, args(f.orgID, "PRIMARY", "ACTIVE", nil)...)
	f.reject(t, "duplicate name while paused", ins, args(f.orgID, "primary", "PAUSED", nil)...)
	f.accept(t, "same name in another organization", ins, args(f.org2, "Fresh", "ACTIVE", nil)...)
	f.accept(t, "same name in another organization 2", ins, args(f.orgID, "Fresh", "ACTIVE", nil)...)

	f.accept(t, "revoke the original", "UPDATE destinations SET status = 'REVOKED', revoked_at = now() WHERE id = $1", f.dest1)
	f.accept(t, "the name is free again after revocation", ins, args(f.orgID, "Primary", "ACTIVE", nil)...)
	f.accept(t, "revoked duplicates may coexist", ins, args(f.orgID, "Primary", "REVOKED", "2026-01-01T00:00:00Z")...)
}

func TestDeliveryConstraints(t *testing.T) {
	f := newDeliveryFixture(t)

	id := f.mustDelivery(t, f.pending(t))
	q, _ := f.env.Pool.Querier()
	var status string
	var attempts, max int
	var due, leaseNull, completedNull bool
	if err := q.QueryRow(context.Background(),
		`SELECT status, attempt_count, max_attempts, next_attempt_at <= now(), lease_expires_at IS NULL, completed_at IS NULL
		   FROM deliveries WHERE id = $1`, id).Scan(&status, &attempts, &max, &due, &leaseNull, &completedNull); err != nil {
		t.Fatal(err)
	}
	if status != "PENDING" || attempts != 0 || max != 8 || !due || !leaseNull || !completedNull {
		t.Fatalf("defaults: %s attempts=%d max=%d due=%v", status, attempts, max, due)
	}

	t.Run("statuses", func(t *testing.T) {
		for _, bad := range []string{"RETRY_WAIT", "pending", "DONE", "", "PENDING "} {
			d := f.pending(t)
			d.status = bad
			q, _ := f.env.Pool.Querier()
			_, err := q.Exec(context.Background(),
				"INSERT INTO deliveries (organization_id, event_id, destination_id, status) VALUES ($1, $2, $3, $4)", d.org, d.event, d.dest, bad)
			if err == nil {
				t.Errorf("status %q accepted", bad)
			}
		}
	})

	t.Run("counters", func(t *testing.T) {
		f.rejectDelivery(t, "negative attempt_count", func() dv { d := f.pending(t); d.attempts = -1; return d }(), "attempt_count")
		f.rejectDelivery(t, "attempt_count above max_attempts", func() dv { d := f.pending(t); d.attempts, d.max = 9, 8; return d }(), "")
		// A cancelled delivery has no other attempt rule, so only max_attempts can reject these.
		cancelled := func(max int) dv {
			d := f.pending(t)
			d.status, d.completed, d.max = "CANCELLED", "now()", max
			return d
		}
		// A negative maximum also breaks attempts_within_max, so either rule may report it.
		f.rejectDelivery(t, "negative max_attempts", cancelled(-1), "")
		f.rejectDelivery(t, "max_attempts above 50", cancelled(51), "deliveries_max_attempts_range")
		f.rejectDelivery(t, "pending with no attempts left", func() dv { d := f.pending(t); d.attempts, d.max = 3, 3; return d }(), "")
		zero := cancelled(0)
		zero.max = 0
		q, _ := f.env.Pool.Querier()
		_, err := q.Exec(context.Background(), "INSERT INTO deliveries (organization_id, event_id, destination_id, status, max_attempts, completed_at) VALUES ($1,$2,$3,'CANCELLED',0,now())",
			zero.org, zero.event, zero.dest)
		if err == nil || !strings.Contains(err.Error(), "deliveries_max_attempts_range") {
			t.Errorf("max_attempts = 0 must be rejected by deliveries_max_attempts_range, got %v", err)
		}
	})

	t.Run("status and field consistency", func(t *testing.T) {
		inFlight := func() dv {
			d := f.pending(t)
			d.status, d.attempts, d.lease, d.token, d.by = "IN_FLIGHT", 1, inFlightLease, tokenExpr, "'worker-1'"
			return d
		}
		terminal := func(status string, attempts, http int) dv {
			d := f.pending(t)
			d.status, d.attempts, d.httpStatus, d.completed = status, attempts, http, "now()"
			return d
		}
		f.mustDelivery(t, inFlight())
		f.mustDelivery(t, terminal("SUCCEEDED", 1, 204))
		f.mustDelivery(t, terminal("FAILED", 3, 500))
		f.mustDelivery(t, terminal("FAILED", 1, 0))
		f.mustDelivery(t, terminal("CANCELLED", 0, 0))

		f.rejectDelivery(t, "in flight without lease", func() dv { d := inFlight(); d.lease = ""; return d }(), "")
		f.rejectDelivery(t, "in flight without claim token", func() dv { d := inFlight(); d.token = ""; return d }(), "")
		f.rejectDelivery(t, "in flight without claimant", func() dv { d := inFlight(); d.by = ""; return d }(), "")
		f.rejectDelivery(t, "in flight with no attempt counted", func() dv { d := inFlight(); d.attempts = 0; return d }(), "")
		f.rejectDelivery(t, "in flight and completed", func() dv { d := inFlight(); d.completed = "now()"; return d }(), "")
		f.rejectDelivery(t, "pending holding a lease", func() dv { d := f.pending(t); d.lease = inFlightLease; return d }(), "")
		f.rejectDelivery(t, "pending holding a claim token", func() dv { d := f.pending(t); d.token = tokenExpr; return d }(), "")
		f.rejectDelivery(t, "pending already completed", func() dv { d := f.pending(t); d.completed = "now()"; return d }(), "")
		f.rejectDelivery(t, "terminal without completed_at", func() dv { d := terminal("FAILED", 1, 500); d.completed = ""; return d }(), "")
		f.rejectDelivery(t, "terminal holding a lease", func() dv { d := terminal("FAILED", 1, 500); d.lease = inFlightLease; return d }(), "")
		f.rejectDelivery(t, "succeeded without any attempt", terminal("SUCCEEDED", 0, 204), "")
		f.rejectDelivery(t, "succeeded with a non-2xx status", terminal("SUCCEEDED", 1, 500), "")
		f.rejectDelivery(t, "succeeded without a recorded status", terminal("SUCCEEDED", 1, 0), "")
		f.rejectDelivery(t, "failed without any attempt", terminal("FAILED", 0, 0), "")
		f.rejectDelivery(t, "status code below 100", func() dv { d := terminal("FAILED", 1, 0); d.httpStatus = 99; return d }(), "")
		f.rejectDelivery(t, "status code above 599", func() dv { d := terminal("FAILED", 1, 0); d.httpStatus = 600; return d }(), "")
	})

	t.Run("tenancy", func(t *testing.T) {
		// Organization 1's event with organization 2's destination, and the reverse.
		f.rejectDelivery(t, "cross-tenant destination", dv{org: f.orgID, event: f.newEvent(t, f.orgID, f.ep1), dest: f.dest2}, "deliveries_destination_fk")
		f.rejectDelivery(t, "cross-tenant event", dv{org: f.orgID, event: f.newEvent(t, f.org2, f.ep2), dest: f.dest1}, "deliveries_event_fk")
		f.rejectDelivery(t, "both references from another tenant", dv{org: f.orgID, event: f.newEvent(t, f.org2, f.ep2), dest: f.dest2}, "")
		f.rejectDelivery(t, "organization differs from both references", dv{org: f.org2, event: f.newEvent(t, f.orgID, f.ep1), dest: f.dest1}, "")
		f.rejectDelivery(t, "unknown organization", dv{org: "00000000-0000-4000-8000-000000000000", event: f.newEvent(t, f.orgID, f.ep1), dest: f.dest1}, "")
		f.rejectDelivery(t, "unknown event", dv{org: f.orgID, event: "00000000-0000-4000-8000-000000000000", dest: f.dest1}, "")
		f.rejectDelivery(t, "unknown destination", dv{org: f.orgID, event: f.newEvent(t, f.orgID, f.ep1), dest: "00000000-0000-4000-8000-000000000000"}, "")
		// The legitimate same-tenant references still work.
		f.mustDelivery(t, dv{org: f.org2, event: f.newEvent(t, f.org2, f.ep2), dest: f.dest2})
	})

	t.Run("one delivery per event and destination", func(t *testing.T) {
		d := f.pending(t)
		f.mustDelivery(t, d)
		f.rejectDelivery(t, "duplicate event and destination", d, "deliveries_event_destination_key")
		// The same event may go to a different destination of the same organization.
		other := f.insertDestination(t, f.orgID, f.userID, "Another", validURL)
		f.mustDelivery(t, dv{org: f.orgID, event: d.event, dest: other})
		// And a different event may go to the same destination.
		f.mustDelivery(t, f.pending(t))
	})
}

func TestDestinationsAndDeliveriesAreNeverDeletedUnderHistory(t *testing.T) {
	f := newDeliveryFixture(t)
	d := f.pending(t)
	deliveryID := f.mustDelivery(t, d)
	f.accept(t, "claim", `UPDATE deliveries SET status = 'IN_FLIGHT', attempt_count = 1, lease_expires_at = now() + interval '1 minute',
		claim_token = gen_random_uuid(), claimed_by = 'w' WHERE id = $1`, deliveryID)
	f.accept(t, "attempt", `INSERT INTO delivery_attempts (organization_id, delivery_id, attempt_number, started_at, finished_at, outcome, http_status, duration_ms)
		VALUES ($1, $2, 1, now(), now(), 'SUCCESS', 204, 12)`, f.orgID, deliveryID)

	f.reject(t, "delete a destination that has deliveries", "DELETE FROM destinations WHERE id = $1", f.dest1)
	f.reject(t, "delete an event that has deliveries", "DELETE FROM webhook_events WHERE id = $1", d.event)
	f.reject(t, "delete a delivery that has attempts", "DELETE FROM deliveries WHERE id = $1", deliveryID)
	f.reject(t, "delete an organization that has destinations", "DELETE FROM organizations WHERE id = $1", f.orgID)

	// Disabling or revoking a destination keeps every historical row.
	f.accept(t, "pause", "UPDATE destinations SET status = 'PAUSED' WHERE id = $1", f.dest1)
	f.accept(t, "revoke", "UPDATE destinations SET status = 'REVOKED', revoked_at = now() WHERE id = $1", f.dest1)
	if f.env.Count(t, "SELECT count(*) FROM deliveries WHERE destination_id = $1", f.dest1) != 1 ||
		f.env.Count(t, "SELECT count(*) FROM delivery_attempts WHERE delivery_id = $1", deliveryID) != 1 {
		t.Fatal("history must survive pausing and revoking the destination")
	}
}

// transitionAssignments returns the UPDATE assignments that move a delivery to
// `to` while satisfying every CHECK constraint, so a failure can only come from
// the transition trigger. inc is the attempt_count increment.
func transitionAssignments(to delivery.Status, inc int) string {
	clear := "lease_expires_at = NULL, claim_token = NULL, claimed_by = NULL"
	switch to {
	case delivery.StatusPending:
		return "status = 'PENDING', " + clear + ", completed_at = NULL, next_attempt_at = now() + interval '1 minute'"
	case delivery.StatusInFlight:
		return fmt.Sprintf("status = 'IN_FLIGHT', attempt_count = attempt_count + %d, lease_expires_at = now() + interval '1 minute', "+
			"claim_token = gen_random_uuid(), claimed_by = 'w2', completed_at = NULL", inc)
	case delivery.StatusSucceeded:
		return "status = 'SUCCEEDED', " + clear + ", last_http_status = 200, completed_at = now()"
	case delivery.StatusFailed:
		return "status = 'FAILED', " + clear + ", last_http_status = 500, completed_at = now(), failure_reason = 'RETRIES_EXHAUSTED'"
	default: // CANCELLED
		return "status = 'CANCELLED', " + clear + ", completed_at = now()"
	}
}

// rowIn returns a delivery row in the given status with attempt_count = 1.
func (f *deliveryFixture) rowIn(t *testing.T, status delivery.Status) string {
	d := f.pending(t)
	d.attempts = 1
	switch status {
	case delivery.StatusInFlight:
		d.status, d.lease, d.token, d.by = "IN_FLIGHT", inFlightLease, tokenExpr, "'w1'"
	case delivery.StatusSucceeded:
		d.status, d.httpStatus, d.completed = "SUCCEEDED", 204, "now()"
	case delivery.StatusFailed:
		d.status, d.httpStatus, d.completed = "FAILED", 500, "now()"
	case delivery.StatusCancelled:
		d.status, d.completed = "CANCELLED", "now()"
	}
	return f.mustDelivery(t, d)
}

var allDeliveryStatuses = []delivery.Status{delivery.StatusPending, delivery.StatusInFlight, delivery.StatusSucceeded, delivery.StatusFailed, delivery.StatusCancelled}

// TestDatabaseTransitionsMatchTheGoStateMachine runs all 25 (from, to) pairs
// against the trigger and requires the database to agree with delivery.CheckTransition.
func TestDatabaseTransitionsMatchTheGoStateMachine(t *testing.T) {
	f := newDeliveryFixture(t)
	for _, from := range allDeliveryStatuses {
		for _, to := range allDeliveryStatuses {
			if from == to {
				continue // same-status updates are covered separately
			}
			id := f.rowIn(t, from)
			inc := 0
			if from == delivery.StatusPending && to == delivery.StatusInFlight {
				inc = 1 // a claim increments the counter by exactly one
			}
			q, _ := f.env.Pool.Querier()
			_, err := q.Exec(context.Background(), "UPDATE deliveries SET "+transitionAssignments(to, inc)+" WHERE id = $1", id)
			want := delivery.CheckTransition(from, to) == nil
			switch {
			case want && err != nil:
				t.Errorf("%s -> %s is legal in Go but the database rejected it: %v", from, to, err)
			case !want && err == nil:
				t.Errorf("%s -> %s is illegal in Go but the database accepted it", from, to)
			case !want && !strings.Contains(err.Error(), "delivery"):
				t.Errorf("%s -> %s was rejected by something other than the transition trigger: %v", from, to, err)
			}
		}
	}
}

func TestTerminalDeliveriesAreImmutable(t *testing.T) {
	f := newDeliveryFixture(t)
	for _, status := range []delivery.Status{delivery.StatusSucceeded, delivery.StatusFailed, delivery.StatusCancelled} {
		id := f.rowIn(t, status)
		for name, assign := range map[string]string{
			"same status":  "status = '" + string(status) + "'",
			"error class":  "last_error_class = 'late'",
			"claimed_by":   "claimed_by = 'late'",
			"next attempt": "next_attempt_at = now()",
			"completed_at": "completed_at = now() + interval '1 day'",
			"http status":  "last_http_status = 200",
			"reopen":       "status = 'PENDING', completed_at = NULL",
		} {
			f.reject(t, string(status)+" "+name, "UPDATE deliveries SET "+assign+" WHERE id = $1", id)
		}
	}
}

func TestPendingAndInFlightMayBeUpdatedWithoutChangingStatus(t *testing.T) {
	f := newDeliveryFixture(t)
	pending := f.rowIn(t, delivery.StatusPending)
	f.accept(t, "reschedule a pending delivery", "UPDATE deliveries SET next_attempt_at = now() + interval '5 minutes', last_error_class = 'timeout' WHERE id = $1", pending)

	inFlight := f.rowIn(t, delivery.StatusInFlight)
	f.accept(t, "renew the lease", "UPDATE deliveries SET lease_expires_at = now() + interval '2 minutes' WHERE id = $1", inFlight)
	f.reject(t, "replace the claim token while in flight", "UPDATE deliveries SET claim_token = gen_random_uuid() WHERE id = $1", inFlight)
}

func TestDeliveryIdentityAndCountersAreProtected(t *testing.T) {
	f := newDeliveryFixture(t)
	other := f.insertDestination(t, f.orgID, f.userID, "Other", validURL)
	id := f.rowIn(t, delivery.StatusPending)

	f.reject(t, "move to another destination", "UPDATE deliveries SET destination_id = $2 WHERE id = $1", id, other)
	f.reject(t, "move to another event", "UPDATE deliveries SET event_id = $2 WHERE id = $1", id, f.newEvent(t, f.orgID, f.ep1))
	f.reject(t, "move to another organization", "UPDATE deliveries SET organization_id = $2 WHERE id = $1", id, f.org2)
	f.reject(t, "change the id", "UPDATE deliveries SET id = gen_random_uuid() WHERE id = $1", id)
	f.reject(t, "change max_attempts", "UPDATE deliveries SET max_attempts = 50 WHERE id = $1", id)
	f.reject(t, "change created_at", "UPDATE deliveries SET created_at = now() - interval '1 day' WHERE id = $1", id)
	f.reject(t, "decrease attempt_count", "UPDATE deliveries SET attempt_count = 0 WHERE id = $1", id)
	f.reject(t, "increase attempt_count without a claim", "UPDATE deliveries SET attempt_count = 2 WHERE id = $1", id)

	claim := func(inc int) string {
		return "UPDATE deliveries SET " + transitionAssignments(delivery.StatusInFlight, inc) + " WHERE id = $1"
	}
	f.reject(t, "claim without counting the attempt", claim(0), id)
	f.reject(t, "claim counting two attempts", claim(2), id)
	f.accept(t, "claim counting one attempt", claim(1), id)
	f.reject(t, "complete while changing attempt_count",
		"UPDATE deliveries SET status = 'SUCCEEDED', attempt_count = 3, lease_expires_at = NULL, claim_token = NULL, claimed_by = NULL, last_http_status = 200, completed_at = now() WHERE id = $1", id)
}

const attemptInsert = "INSERT INTO delivery_attempts (organization_id, delivery_id, attempt_number, started_at, finished_at, outcome, http_status, duration_ms) " +
	"VALUES ($1, $2, $3, now() - interval '1 second', now(), $4, $5, $6)"

func TestDeliveryAttemptConstraints(t *testing.T) {
	f := newDeliveryFixture(t)
	// A real history: attempt 1 was abandoned and the delivery was claimed again.
	deliveryID := f.inFlightAt(t, f.orgID, f.dest1, f.ep1, 2)
	otherOrgDelivery := f.inFlightAt(t, f.org2, f.dest2, f.ep2, 1)
	var none any

	f.accept(t, "the current attempt (2)", attemptInsert, f.orgID, deliveryID, 2, "HTTP_ERROR", 503, 90)
	f.reject(t, "duplicate attempt number", attemptInsert, f.orgID, deliveryID, 2, "HTTP_ERROR", 500, 5)
	f.reject(t, "attempt number zero", attemptInsert, f.orgID, deliveryID, 0, "NETWORK_ERROR", none, 5)
	f.reject(t, "negative attempt number", attemptInsert, f.orgID, deliveryID, -1, "NETWORK_ERROR", none, 5)
	f.reject(t, "attempt beyond the claimed count", attemptInsert, f.orgID, deliveryID, 3, "NETWORK_ERROR", none, 5)

	// The history must reference the delivery in its own organization.
	f.reject(t, "organization does not own the delivery", attemptInsert, f.org2, deliveryID, 2, "NETWORK_ERROR", none, 5)
	f.reject(t, "delivery of another organization", attemptInsert, f.orgID, otherOrgDelivery, 1, "NETWORK_ERROR", none, 5)
	f.reject(t, "unknown delivery", attemptInsert, f.orgID, "00000000-0000-4000-8000-000000000000", 1, "NETWORK_ERROR", none, 5)
	f.reject(t, "unknown organization", attemptInsert, "00000000-0000-4000-8000-000000000000", deliveryID, 2, "NETWORK_ERROR", none, 5)
	f.accept(t, "the owning organization", attemptInsert, f.org2, otherOrgDelivery, 1, "NETWORK_ERROR", none, 5)

	// An attempt can only be recorded for a delivery that was claimed.
	unclaimed := f.mustDelivery(t, f.pending(t))
	f.reject(t, "attempt for a delivery that was never claimed", attemptInsert, f.orgID, unclaimed, 1, "NETWORK_ERROR", none, 5)

	third := f.inFlightAt(t, f.orgID, f.dest1, f.ep1, 1)
	f.reject(t, "negative duration", attemptInsert, f.orgID, third, 1, "TIMEOUT", none, -1)
	f.reject(t, "finished before started",
		"INSERT INTO delivery_attempts (organization_id, delivery_id, attempt_number, started_at, finished_at, outcome, duration_ms) VALUES ($1,$2,1, now(), now() - interval '1 hour', 'TIMEOUT', 5)",
		f.orgID, third)
}

func TestAttemptOutcomesMatchTheirHTTPStatus(t *testing.T) {
	f := newDeliveryFixture(t)
	id := f.inFlightAt(t, f.orgID, f.dest1, f.ep1, 1)
	var none any
	n := 1 // the delivery's current attempt number
	try := func(outcome string, status any, ok bool) {
		t.Helper()
		name := fmt.Sprintf("%s with status %v", outcome, status)
		if !ok {
			f.reject(t, name, attemptInsert, f.orgID, id, n, outcome, status, 1)
			return
		}
		f.accept(t, name, attemptInsert, f.orgID, id, n, outcome, status, 1)
		f.retryAndClaim(t, id) // a new attempt number needs a retry and a new claim
		n++
	}
	// These rules mirror delivery.OutcomeFromHTTPStatus.
	try("SUCCESS", 500, false)
	try("SUCCESS", none, false)
	try("SUCCESS", 200, true)
	try("SUCCESS", 299, true)
	try("REDIRECT", 200, false)
	try("REDIRECT", none, false)
	try("REDIRECT", 302, true)
	try("HTTP_ERROR", 200, false)
	try("HTTP_ERROR", 302, false)
	try("HTTP_ERROR", none, false)
	try("HTTP_ERROR", 404, true)
	try("HTTP_ERROR", 599, true)
	try("NETWORK_ERROR", 500, false)
	try("NETWORK_ERROR", none, true)
}

func TestEveryDeliveryOutcomeIsRepresentable(t *testing.T) {
	f := newDeliveryFixture(t)
	id := f.inFlightAt(t, f.orgID, f.dest1, f.ep1, 1)
	var none any
	outcomes := []struct {
		o delivery.Outcome
		s any
	}{
		{delivery.OutcomeSuccess, 204}, {delivery.OutcomeRedirect, 301}, {delivery.OutcomeHTTPError, 500},
		{delivery.OutcomeNetworkError, none}, {delivery.OutcomeTimeout, none}, {delivery.OutcomeBlockedByPolicy, none},
		{delivery.OutcomeAbandoned, none},
	}
	n := 1
	for _, c := range outcomes {
		f.accept(t, string(c.o), attemptInsert, f.orgID, id, n, string(c.o), c.s, 3)
		f.retryAndClaim(t, id)
		n++
	}
	f.reject(t, "an outcome Go does not define", attemptInsert, f.orgID, id, n, "EXPLODED", none, 3)
	f.reject(t, "lowercase outcome", attemptInsert, f.orgID, id, n, "success", 200, 3)
	// Every status code the Go classifier maps to an outcome is accepted with that outcome.
	for _, code := range []int{200, 204, 299, 301, 399, 400, 404, 429, 500, 599} {
		o := delivery.OutcomeFromHTTPStatus(code)
		f.accept(t, fmt.Sprintf("Go maps %d to %s", code, o), attemptInsert, f.orgID, id, n, string(o), code, 1)
		f.retryAndClaim(t, id)
		n++
	}
}

func TestAttemptsAreAppendOnly(t *testing.T) {
	f := newDeliveryFixture(t)
	d := f.pending(t)
	d.attempts, d.status, d.lease, d.token, d.by = 1, "IN_FLIGHT", inFlightLease, tokenExpr, "'w'"
	id := f.mustDelivery(t, d)
	f.accept(t, "record an attempt", attemptInsert, f.orgID, id, 1, "SUCCESS", 200, 7)

	f.reject(t, "update", "UPDATE delivery_attempts SET outcome = 'TIMEOUT', http_status = NULL")
	f.reject(t, "update duration", "UPDATE delivery_attempts SET duration_ms = 0")
	f.reject(t, "delete", "DELETE FROM delivery_attempts")
	f.reject(t, "truncate-like delete by delivery", "DELETE FROM delivery_attempts WHERE delivery_id = $1", id)
	if f.env.Count(t, "SELECT count(*) FROM delivery_attempts WHERE outcome = 'SUCCESS' AND duration_ms = 7") != 1 {
		t.Fatal("attempt history was altered")
	}
}

func TestIndexesForSchedulingAndHistoryExist(t *testing.T) {
	env := apptest.New(t)
	want := map[string]string{
		"deliveries_due_idx":            "status = 'PENDING'",
		"deliveries_lease_idx":          "status = 'IN_FLIGHT'",
		"deliveries_destination_idx":    "destination_id",
		"deliveries_organization_idx":   "organization_id",
		"delivery_attempts_history_idx": "organization_id",
		"destinations_organization_idx": "organization_id",
		"destinations_active_name_key":  "lower(name)",
	}
	for name, fragment := range want {
		var def string
		q, _ := env.Pool.Querier()
		if err := q.QueryRow(context.Background(), "SELECT indexdef FROM pg_indexes WHERE indexname = $1", name).Scan(&def); err != nil {
			t.Errorf("index %s missing: %v", name, err)
			continue
		}
		if !strings.Contains(def, fragment) {
			t.Errorf("index %s = %s, want it to contain %q", name, def, fragment)
		}
	}
}

func TestTenantReferencesAreCompositeForeignKeys(t *testing.T) {
	env := apptest.New(t)
	q, _ := env.Pool.Querier()
	// Every tenant-owned reference must include organization_id.
	want := map[string]string{
		"deliveries_event_fk":           "FOREIGN KEY (event_id, organization_id)",
		"deliveries_destination_fk":     "FOREIGN KEY (destination_id, organization_id)",
		"delivery_attempts_delivery_fk": "FOREIGN KEY (delivery_id, organization_id)",
	}
	for name, fragment := range want {
		var def string
		if err := q.QueryRow(context.Background(), "SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = $1", name).Scan(&def); err != nil {
			t.Errorf("constraint %s missing: %v", name, err)
			continue
		}
		if !strings.HasPrefix(def, fragment) {
			t.Errorf("%s = %s, want %s", name, def, fragment)
		}
		if strings.Contains(def, "CASCADE") || strings.Contains(def, "SET NULL") {
			t.Errorf("%s must not cascade deletes: %s", name, def)
		}
	}
}

func TestConcurrentInsertionCreatesOneLogicalDelivery(t *testing.T) {
	f := newDeliveryFixture(t)
	d := f.pending(t)
	q, _ := f.env.Pool.Querier()

	const workers = 24
	t.Run("ON CONFLICT DO NOTHING", func(t *testing.T) {
		var inserted atomic.Int64
		var wg sync.WaitGroup
		start := make(chan struct{})
		for range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				tag, err := q.Exec(context.Background(),
					`INSERT INTO deliveries (organization_id, event_id, destination_id) VALUES ($1, $2, $3)
					 ON CONFLICT (event_id, destination_id) DO NOTHING`, d.org, d.event, d.dest)
				if err != nil {
					t.Error(err)
					return
				}
				inserted.Add(tag.RowsAffected())
			}()
		}
		close(start)
		wg.Wait()
		if inserted.Load() != 1 || f.env.Count(t, "SELECT count(*) FROM deliveries WHERE event_id = $1 AND destination_id = $2", d.event, d.dest) != 1 {
			t.Fatalf("%d rows inserted, want exactly 1", inserted.Load())
		}
	})

	t.Run("plain INSERT", func(t *testing.T) {
		d2 := f.pending(t)
		var ok, duplicates atomic.Int64
		var wg sync.WaitGroup
		start := make(chan struct{})
		for range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, err := q.Exec(context.Background(),
					"INSERT INTO deliveries (organization_id, event_id, destination_id) VALUES ($1, $2, $3)", d2.org, d2.event, d2.dest)
				var pgErr *pgconn.PgError
				switch {
				case err == nil:
					ok.Add(1)
				case errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "deliveries_event_destination_key":
					duplicates.Add(1)
				default:
					t.Errorf("unexpected error: %v", err)
				}
			}()
		}
		close(start)
		wg.Wait()
		if ok.Load() != 1 || duplicates.Load() != workers-1 {
			t.Fatalf("%d succeeded and %d were rejected as duplicates, want 1 and %d", ok.Load(), duplicates.Load(), workers-1)
		}
	})
}

func TestUpdatedAtTriggersExistForDeliveryTables(t *testing.T) {
	f := newDeliveryFixture(t)
	if n := f.env.Count(t, "SELECT count(*) FROM pg_trigger WHERE tgname IN ('destinations_set_updated_at', 'deliveries_set_updated_at')"); n != 2 {
		t.Fatalf("updated_at triggers = %d, want 2", n)
	}
	f.accept(t, "backdate", "UPDATE destinations SET updated_at = now() - interval '1 day' WHERE id = $1", f.dest1)
	f.accept(t, "touch", "UPDATE destinations SET name = 'Renamed' WHERE id = $1", f.dest1)
	if f.env.Count(t, "SELECT count(*) FROM destinations WHERE id = $1 AND updated_at > now() - interval '1 minute'", f.dest1) != 1 {
		t.Fatal("updated_at must be refreshed")
	}
}

// TestRollbackOfDeliveryMigrationsAndReapply rolls migrations 00013, 00012 and
// 00011 back with goose, checks that earlier schema is untouched, and applies
// them again.
func TestRollbackOfDeliveryMigrationsAndReapply(t *testing.T) {
	env := apptest.New(t)
	ctx := context.Background()
	db, err := env.Pool.SQLDB()
	if err != nil {
		t.Fatal(err)
	}
	provider := newGooseProvider(t, db)
	has := func(table string) bool {
		return env.Count(t, "SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public' AND table_name = $1", table) == 1
	}
	for _, tbl := range []string{"destinations", "deliveries", "delivery_attempts"} {
		if !has(tbl) {
			t.Fatalf("table %s must exist after migrating up", tbl)
		}
	}

	if _, err := provider.DownTo(ctx, 10); err != nil {
		t.Fatalf("rolling back to version 10: %v", err)
	}
	for _, tbl := range []string{"destinations", "deliveries", "delivery_attempts"} {
		if has(tbl) {
			t.Errorf("table %s must be gone after the rollback", tbl)
		}
	}
	for _, fn := range []string{"relayforge_enforce_delivery_transition", "relayforge_check_delivery_attempt", "relayforge_reject_attempt_change", "relayforge_reject_row_removal"} {
		if env.Count(t, "SELECT count(*) FROM pg_proc WHERE proname = $1", fn) != 0 {
			t.Errorf("function %s must be dropped by the rollback", fn)
		}
	}
	// Earlier migrations are untouched.
	for _, tbl := range []string{"webhook_events", "outbox", "ingestion_endpoints", "rate_limit_counters", "audit_events"} {
		if !has(tbl) {
			t.Errorf("rolling back must not remove %s", tbl)
		}
	}
	if env.Count(t, "SELECT count(*) FROM pg_proc WHERE proname = 'relayforge_set_updated_at'") != 1 {
		t.Error("the shared updated_at function must survive")
	}

	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("re-applying: %v", err)
	}
	for _, tbl := range []string{"destinations", "deliveries", "delivery_attempts"} {
		if !has(tbl) {
			t.Errorf("table %s must exist again after re-applying", tbl)
		}
	}
}

func newGooseProvider(t *testing.T, db *sql.DB) *goose.Provider {
	t.Helper()
	p, err := goose.NewProvider(goose.DialectPostgres, db, database.EmbeddedMigrations())
	if err != nil {
		t.Fatal(err)
	}
	return p
}
