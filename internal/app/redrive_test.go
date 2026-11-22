package app_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Thiago-tods/relayforge/internal/app"
	"github.com/Thiago-tods/relayforge/internal/app/apptest"
	"github.com/Thiago-tods/relayforge/internal/database"
	"github.com/Thiago-tods/relayforge/internal/delivery"
	"github.com/Thiago-tods/relayforge/internal/identity"
	"github.com/Thiago-tods/relayforge/internal/observability"
	"github.com/Thiago-tods/relayforge/internal/tenancy"
)

// ---- helpers ----

func redriveInput(reason string, expected int) func() (app.RedriveInput, error) {
	return func() (app.RedriveInput, error) {
		return app.RedriveInput{Reason: reason, ExpectedAttemptCount: expected}, nil
	}
}

func redrive(svc *app.Service, p app.Principal, org, id string, expected int) (app.RedriveResult, error) {
	return svc.RedriveDelivery(bg, p, org, id, redriveInput("the receiver is back online", expected))
}

// redriveSvc is a second service over the same database with its own lock timeout and redrive counter.
func redriveSvc(t *testing.T, env *apptest.Env, lockTimeout time.Duration, counter *observability.Counter) *app.Service {
	t.Helper()
	cfg := app.DefaultConfig()
	cfg.Box = env.Box
	cfg.RedriveLockTimeout = lockTimeout
	cfg.Redriven = counter
	svc, err := app.New(env.Pool, identity.NewHasher(apptest.FastArgon2Params), cfg, env.Clock.Now, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func newDest(t *testing.T, tm team, name string) string {
	t.Helper()
	d, err := tm.env.Svc.CreateDestination(bg, tm.owner.Principal, tm.org.ID, name, goodURL)
	if err != nil {
		t.Fatal(err)
	}
	return d.Destination.ID
}

// failedRow inserts a FAILED delivery (one attempt used, no attempt rows) for a really ingested event whose payload
// carries a secret marker. A legacy row has no recorded failure reason.
func failedRow(t *testing.T, env *apptest.Env, u apptest.User, destID string, max int, legacy bool) string {
	t.Helper()
	ep := env.CreateEndpointIn(t, u, u.Org.ID)
	res, err := env.Send(ep.Endpoint.PublicID, env.Signed(ep.SigningSecret, []byte(`{"secret":"PAYLOADSECRET"}`), fmt.Sprintf("rd-%d", seedSeq.Add(1))))
	if err != nil {
		t.Fatal(err)
	}
	if legacy {
		env.Exec(t, "ALTER TABLE deliveries DROP CONSTRAINT deliveries_failed_has_reason")
		defer env.Exec(t, "ALTER TABLE deliveries ADD CONSTRAINT deliveries_failed_has_reason CHECK (status <> 'FAILED' OR failure_reason IS NOT NULL) NOT VALID")
	}
	q, _ := env.Pool.Querier()
	var id string
	if err := q.QueryRow(bg, `INSERT INTO deliveries (organization_id, event_id, destination_id, status, attempt_count, max_attempts, last_http_status, completed_at, failure_reason)
		VALUES ($1, $2, $3, 'FAILED', 1, $4, 410, now(), CASE WHEN $5 THEN NULL ELSE 'DESTINATION_REJECTED' END) RETURNING id::text`,
		u.Org.ID, res.EventID, destID, max, legacy).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

type deliveryRow struct {
	Status        string
	Attempts, Max int
	Reason        *string
	Completed     *time.Time
}

func rowOf(t *testing.T, env *apptest.Env, id string) deliveryRow {
	t.Helper()
	q, _ := env.Pool.Querier()
	var r deliveryRow
	if err := q.QueryRow(bg, "SELECT status, attempt_count, max_attempts, failure_reason, completed_at FROM deliveries WHERE id = $1", id).
		Scan(&r.Status, &r.Attempts, &r.Max, &r.Reason, &r.Completed); err != nil {
		t.Fatal(err)
	}
	return r
}

func redriveAudits(t *testing.T, env *apptest.Env, id string) int {
	t.Helper()
	return env.Count(t, "SELECT count(*) FROM audit_events WHERE resource_id = $1 AND action = 'delivery.redriven'", id)
}

// unchanged asserts that a rejected redrive left the delivery FAILED with its budget, and wrote no audit event.
func unchanged(t *testing.T, env *apptest.Env, id string, before deliveryRow) {
	t.Helper()
	if got := rowOf(t, env, id); got.Status != before.Status || got.Max != before.Max || got.Attempts != before.Attempts || got.Completed == nil != (before.Completed == nil) {
		t.Errorf("a rejected redrive changed the delivery: %+v -> %+v", before, got)
	}
	if n := redriveAudits(t, env, id); n != 0 {
		t.Errorf("a rejected redrive wrote %d audit events", n)
	}
}

func wantErr(t *testing.T, label string, got, want error) {
	t.Helper()
	if !errors.Is(got, want) {
		t.Errorf("%s: %v, want %v", label, got, want)
	}
}

// holdTx runs fn in a transaction that stays open until release is called.
func holdTx(t *testing.T, env *apptest.Env, fn func(tx pgx.Tx) error) (release func()) {
	t.Helper()
	started, done := make(chan struct{}), make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		finished <- env.Pool.InTx(bg, func(tx pgx.Tx) error {
			if err := fn(tx); err != nil {
				return err
			}
			close(started)
			<-done
			return nil
		})
	}()
	select {
	case <-started:
	case err := <-finished:
		t.Fatalf("hold transaction: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("hold transaction did not start")
	}
	return func() {
		close(done)
		if err := <-finished; err != nil {
			t.Errorf("release: %v", err)
		}
	}
}

func lockOrg(orgID, mode string) func(tx pgx.Tx) error {
	return func(tx pgx.Tx) error {
		_, err := tx.Exec(bg, "SELECT id FROM organizations WHERE id = $1 FOR "+mode, orgID)
		return err
	}
}

func notDone(t *testing.T, label string, ch <-chan error, d time.Duration) {
	t.Helper()
	select {
	case err := <-ch:
		t.Fatalf("%s finished early: %v", label, err)
	case <-time.After(d):
	}
}

func within(t *testing.T, label string, ch <-chan error, d time.Duration) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(d):
		t.Fatalf("%s did not finish within %v", label, d)
		return nil
	}
}

// ---- authorization ----

func TestRedriveAuthorizesBeforeItParsesOrValidates(t *testing.T) {
	tm := newTeam(t)
	dest := newDest(t, tm, "D")
	id := failedRow(t, tm.env, tm.owner, dest, 8, false)
	stranger := tm.env.Register(t, "stranger@example.com")
	svc := tm.env.Svc
	trap := func() (app.RedriveInput, error) {
		t.Error("the body must not be read for a caller who is not authorized")
		return app.RedriveInput{}, nil
	}
	_, err := svc.RedriveDelivery(bg, stranger.Principal, tm.org.ID, id, trap)
	wantErr(t, "non-member", err, app.ErrNotFound)
	_, err = svc.RedriveDelivery(bg, tm.owner.Principal, "not-a-uuid", id, trap)
	wantErr(t, "malformed organization", err, app.ErrNotFound)
	_, err = svc.RedriveDelivery(bg, tm.viewer.Principal, tm.org.ID, id, trap)
	wantErr(t, "viewer", err, app.ErrForbidden)
	_, err = svc.RedriveDelivery(bg, tm.owner.Principal, tm.org.ID, "not-a-uuid", trap)
	wantErr(t, "malformed delivery id", err, app.ErrNotFound)
	// Another organization's delivery is invisible, the same as a missing one.
	_, err = redrive(svc, stranger.Principal, stranger.Org.ID, id, 1)
	wantErr(t, "delivery of another organization", err, app.ErrNotFound)
	_, err = redrive(svc, tm.owner.Principal, tm.org.ID, "00000000-0000-4000-8000-000000000000", 1)
	wantErr(t, "unknown delivery", err, app.ErrNotFound)
	unchanged(t, tm.env, id, stillFailed(8))
}

func TestRedriveIsAllowedForOwnerAdminAndDeveloperOnly(t *testing.T) {
	tm := newTeam(t)
	dest := newDest(t, tm, "D")
	for name, u := range map[string]apptest.User{"owner": tm.owner, "admin": tm.admin, "developer": tm.developer} {
		id := failedRow(t, tm.env, tm.owner, dest, 8, false)
		res, err := redrive(tm.env.Svc, u.Principal, tm.org.ID, id, 1)
		if err != nil || res.Delivery.MaxAttempts != 13 {
			t.Errorf("%s: %+v %v", name, res, err)
		}
		var actor string
		q, _ := tm.env.Pool.Querier()
		if err := q.QueryRow(bg, "SELECT actor_user_id::text FROM audit_events WHERE resource_id = $1 AND action = 'delivery.redriven'", id).Scan(&actor); err != nil || actor != u.Principal.User.ID {
			t.Errorf("%s: audit actor %q %v", name, actor, err)
		}
	}
	id := failedRow(t, tm.env, tm.owner, dest, 8, false)
	_, err := redrive(tm.env.Svc, tm.viewer.Principal, tm.org.ID, id, 1)
	wantErr(t, "viewer", err, app.ErrForbidden)
	unchanged(t, tm.env, id, rowOf(t, tm.env, id))
}

func TestRedriveRespectsMembershipChangesCommittedBeforeIt(t *testing.T) {
	tm := newTeam(t)
	dest := newDest(t, tm, "D")
	if err := tm.env.Svc.ChangeRole(bg, tm.owner.Principal, tm.org.ID, tm.developer.Principal.User.ID, tenancy.RoleViewer); err != nil {
		t.Fatal(err)
	}
	id := failedRow(t, tm.env, tm.owner, dest, 8, false)
	_, err := redrive(tm.env.Svc, tm.developer.Principal, tm.org.ID, id, 1)
	wantErr(t, "demoted developer", err, app.ErrForbidden)
	if err := tm.env.Svc.RemoveMember(bg, tm.owner.Principal, tm.org.ID, tm.admin.Principal.User.ID); err != nil {
		t.Fatal(err)
	}
	_, err = redrive(tm.env.Svc, tm.admin.Principal, tm.org.ID, id, 1)
	wantErr(t, "removed admin", err, app.ErrNotFound)
	unchanged(t, tm.env, id, rowOf(t, tm.env, id))
}

// ---- eligibility, budget and audit ----

func TestRedriveSucceedsAndRecordsEverythingInOneTransaction(t *testing.T) {
	tm := newTeam(t)
	env := tm.env
	dest := newDest(t, tm, "D")
	id := failedRow(t, env, tm.owner, dest, 8, false)
	before := env.Count(t, "SELECT count(*) FROM delivery_attempts")
	env.Clock.Advance(time.Hour)

	res, err := redriveSvc(t, env, 0, nil).RedriveDelivery(bg, tm.owner.Principal, tm.org.ID, id, redriveInput("  receiver fixed the bug  ", 1))
	if err != nil {
		t.Fatal(err)
	}
	d := res.Delivery
	if d.Status != "PENDING" || d.AttemptCount != 1 || d.MaxAttempts != 13 || d.FailureReason != nil || d.CompletedAt != nil || d.NextAttemptAt == nil ||
		!d.NextAttemptAt.Equal(env.Clock.Now().UTC().Truncate(time.Microsecond)) {
		t.Fatalf("delivery after redrive: %+v (clock %v)", d, env.Clock.Now())
	}
	if res.PreviousMaxAttempts != 8 || res.PreviousFailureReason == nil || *res.PreviousFailureReason != "DESTINATION_REJECTED" {
		t.Fatalf("result: %+v", res)
	}
	row := rowOf(t, env, id)
	if row.Status != "PENDING" || row.Max != 13 || row.Attempts != 1 || row.Reason != nil || row.Completed != nil {
		t.Fatalf("row: %+v", row)
	}
	if env.Count(t, "SELECT count(*) FROM delivery_attempts") != before {
		t.Fatal("a redrive must not add or change attempt rows")
	}

	// Exactly one audit event, with the documented metadata and nothing sensitive.
	if n := redriveAudits(t, env, id); n != 1 {
		t.Fatalf("%d audit events", n)
	}
	q, _ := env.Pool.Querier()
	var raw []byte
	var actor, resource, orgID string
	if err := q.QueryRow(bg, "SELECT metadata, actor_user_id::text, resource_type, organization_id::text FROM audit_events WHERE resource_id = $1 AND action = 'delivery.redriven'", id).
		Scan(&raw, &actor, &resource, &orgID); err != nil {
		t.Fatal(err)
	}
	var meta map[string]any
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for k := range meta {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if strings.Join(keys, ",") != "attemptCount,destinationId,destinationStatus,maxAttempts,previousFailureReason,previousMaxAttempts,previousStatus,reason" {
		t.Errorf("audit metadata keys: %v", keys)
	}
	if meta["reason"] != "receiver fixed the bug" || meta["previousStatus"] != "FAILED" || meta["previousFailureReason"] != "DESTINATION_REJECTED" ||
		meta["previousMaxAttempts"] != float64(8) || meta["maxAttempts"] != float64(13) || meta["attemptCount"] != float64(1) ||
		meta["destinationId"] != dest || meta["destinationStatus"] != "ACTIVE" || actor != tm.owner.Principal.User.ID || resource != "delivery" || orgID != tm.org.ID {
		t.Errorf("audit event: %s actor %s", raw, actor)
	}
	out, _ := json.Marshal(res)
	for _, secret := range []string{"PAYLOADSECRET", "SECRETQUERYTOKEN", "https://"} {
		if strings.Contains(string(raw), secret) || strings.Contains(string(out), secret) {
			t.Errorf("%q leaked into the audit event or the result", secret)
		}
	}

	// A repeated request is a conflict, not a second redrive, and changes nothing.
	_, err = redrive(env.Svc, tm.owner.Principal, tm.org.ID, id, 1)
	wantErr(t, "repeated request", err, app.ErrDeliveryNotFailed)
	if rowOf(t, env, id).Max != 13 || redriveAudits(t, env, id) != 1 {
		t.Fatal("a repeated request changed the budget or the audit trail")
	}
}

func TestRedriveBudgetIsOldPlusFiveCappedAtFifty(t *testing.T) {
	tm := newTeam(t)
	dest := newDest(t, tm, "D")
	for _, c := range []struct{ from, to int }{{5, 10}, {8, 13}, {10, 15}, {45, 50}, {48, 50}} {
		id := failedRow(t, tm.env, tm.owner, dest, c.from, false)
		res, err := redrive(tm.env.Svc, tm.owner.Principal, tm.org.ID, id, 1)
		if err != nil || res.PreviousMaxAttempts != c.from || res.Delivery.MaxAttempts != c.to || rowOf(t, tm.env, id).Max != c.to {
			t.Errorf("%d -> %d: %+v %v", c.from, c.to, res, err)
		}
	}
	id := failedRow(t, tm.env, tm.owner, dest, 50, false)
	_, err := redrive(tm.env.Svc, tm.owner.Principal, tm.org.ID, id, 1)
	wantErr(t, "budget already 50", err, app.ErrRetryBudgetExhausted)
	unchanged(t, tm.env, id, stillFailed(50))
	// From the default of 8 the budget walks 13, 18, ..., 48, 50 and then stops: nine redrives in all.
	walk := failedRow(t, tm.env, tm.owner, dest, 8, false)
	want := []int{13, 18, 23, 28, 33, 38, 43, 48, 50}
	for i, w := range want {
		res, err := redrive(tm.env.Svc, tm.owner.Principal, tm.org.ID, walk, 1)
		if err != nil || res.Delivery.MaxAttempts != w {
			t.Fatalf("redrive %d: %+v %v, want budget %d", i+1, res, err, w)
		}
		// Return the row to FAILED the way a failed episode would (the test bypasses the worker).
		tm.env.Exec(t, "ALTER TABLE deliveries DISABLE TRIGGER deliveries_enforce_transition")
		tm.env.Exec(t, "UPDATE deliveries SET status = 'FAILED', completed_at = now(), failure_reason = 'RETRIES_EXHAUSTED' WHERE id = $1", walk)
		tm.env.Exec(t, "ALTER TABLE deliveries ENABLE TRIGGER deliveries_enforce_transition")
	}
	_, err = redrive(tm.env.Svc, tm.owner.Principal, tm.org.ID, walk, 1)
	wantErr(t, "the tenth redrive", err, app.ErrRetryBudgetExhausted)
}

func TestRedriveRefusesEveryOtherState(t *testing.T) {
	tm := newTeam(t)
	env := tm.env
	dest := newDest(t, tm, "D")

	pending := seedDelivery(t, env, tm.owner, dest, 1, "PENDING")
	succeeded := seedDelivery(t, env, tm.owner, dest, 1, "SUCCEEDED")
	cancelled := seedDelivery(t, env, tm.owner, dest, 1, "PENDING")
	env.Exec(t, "UPDATE deliveries SET status = 'CANCELLED', completed_at = now() WHERE id = $1", cancelled)
	for name, id := range map[string]string{"PENDING": pending, "SUCCEEDED": succeeded, "CANCELLED": cancelled} {
		before := rowOf(t, env, id)
		_, err := redrive(env.Svc, tm.owner.Principal, tm.org.ID, id, before.Attempts)
		wantErr(t, name, err, app.ErrDeliveryNotFailed)
		unchanged(t, env, id, before)
	}

	failed := failedRow(t, env, tm.owner, dest, 8, false)
	before := rowOf(t, env, failed)
	_, err := redrive(env.Svc, tm.owner.Principal, tm.org.ID, failed, 2)
	wantErr(t, "a stale attempt count", err, app.ErrDeliveryChanged)
	unchanged(t, env, failed, before)

	revoked := newDest(t, tm, "Gone")
	gone := failedRow(t, env, tm.owner, revoked, 8, false)
	if _, err := env.Svc.RevokeDestination(bg, tm.owner.Principal, tm.org.ID, revoked); err != nil {
		t.Fatal(err)
	}
	before = rowOf(t, env, gone)
	_, err = redrive(env.Svc, tm.owner.Principal, tm.org.ID, gone, 1)
	wantErr(t, "a revoked destination", err, app.ErrDestinationRevoked)
	unchanged(t, env, gone, before)

	// A PAUSED destination is allowed; its audit event says so.
	paused := newDest(t, tm, "Paused")
	waiting := failedRow(t, env, tm.owner, paused, 8, false)
	if _, err := env.Svc.PauseDestination(bg, tm.owner.Principal, tm.org.ID, paused); err != nil {
		t.Fatal(err)
	}
	if _, err := redrive(env.Svc, tm.owner.Principal, tm.org.ID, waiting, 1); err != nil {
		t.Fatalf("a paused destination: %v", err)
	}
	var status string
	q, _ := env.Pool.Querier()
	if err := q.QueryRow(bg, "SELECT metadata->>'destinationStatus' FROM audit_events WHERE resource_id = $1", waiting).Scan(&status); err != nil || status != "PAUSED" {
		t.Fatalf("destinationStatus %q %v", status, err)
	}
}

func TestRedriveAcceptsALegacyFailureWithoutAReason(t *testing.T) {
	tm := newTeam(t)
	dest := newDest(t, tm, "D")
	id := failedRow(t, tm.env, tm.owner, dest, 8, true)
	if rowOf(t, tm.env, id).Reason != nil {
		t.Fatal("setup: the legacy row must have no reason")
	}
	res, err := redrive(tm.env.Svc, tm.owner.Principal, tm.org.ID, id, 1)
	if err != nil || res.PreviousFailureReason != nil || res.Delivery.MaxAttempts != 13 {
		t.Fatalf("%+v %v", res, err)
	}
	q, _ := tm.env.Pool.Querier()
	var prev *string
	var raw []byte
	if err := q.QueryRow(bg, "SELECT metadata FROM audit_events WHERE resource_id = $1", id).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var meta map[string]any
	_ = json.Unmarshal(raw, &meta)
	if v, ok := meta["previousFailureReason"]; !ok || v != nil {
		t.Fatalf("the audit event must say the previous reason was unrecorded: %s %v", raw, prev)
	}
}

func TestRedriveNeverReconstructsOrSubstitutesAPayload(t *testing.T) {
	tm := newTeam(t)
	env := tm.env
	dest := newDest(t, tm, "D")

	tampered := failedRow(t, env, tm.owner, dest, 8, false)
	env.Exec(t, "UPDATE webhook_events SET payload = '\\x7b2273223a317d' WHERE id = (SELECT event_id FROM deliveries WHERE id = $1)", tampered)
	before := rowOf(t, env, tampered)
	_, err := redrive(env.Svc, tm.owner.Principal, tm.org.ID, tampered, 1)
	wantErr(t, "a payload that no longer matches its hash", err, app.ErrEventPayloadUnavailable)
	unchanged(t, env, tampered, before)

	badHash := failedRow(t, env, tm.owner, dest, 8, false)
	env.Exec(t, "UPDATE webhook_events SET payload_sha256 = decode(repeat('00', 32), 'hex') WHERE id = (SELECT event_id FROM deliveries WHERE id = $1)", badHash)
	before = rowOf(t, env, badHash)
	_, err = redrive(env.Svc, tm.owner.Principal, tm.org.ID, badHash, 1)
	wantErr(t, "a recorded hash that does not match", err, app.ErrEventPayloadUnavailable)
	unchanged(t, env, badHash, before)

	// An event that does not exist in the organization is unavailable too (the foreign key makes it unreachable through
	// the application; the guard is what the service relies on regardless).
	ok, err := delivery.EventPayloadIntact(bg, mq(t, env), tm.org.ID, "00000000-0000-4000-8000-000000000000")
	if err != nil || ok {
		t.Fatalf("a missing event: %v %v", ok, err)
	}
	intact := failedRow(t, env, tm.owner, dest, 8, false)
	var event string
	q, _ := env.Pool.Querier()
	if err := q.QueryRow(bg, "SELECT event_id::text FROM deliveries WHERE id = $1", intact).Scan(&event); err != nil {
		t.Fatal(err)
	}
	if ok, err := delivery.EventPayloadIntact(bg, q, tm.org.ID, event); err != nil || !ok {
		t.Fatalf("a real ingested payload must pass the integrity guard: %v %v", ok, err)
	}
	// The same event looked up through another organization is unavailable.
	other := env.Register(t, "other@example.com")
	if ok, _ := delivery.EventPayloadIntact(bg, q, other.Org.ID, event); ok {
		t.Fatal("the guard must be scoped to the organization")
	}
}

func TestRedriveValidatesTheReasonAndTheExpectedAttemptCount(t *testing.T) {
	tm := newTeam(t)
	dest := newDest(t, tm, "D")
	id := failedRow(t, tm.env, tm.owner, dest, 8, false)
	before := rowOf(t, tm.env, id)
	for name, c := range map[string]struct {
		reason   string
		expected int
	}{
		"empty reason":            {"", 1},
		"blank reason":            {" \t ", 1},
		"too long":                {strings.Repeat("x", 501), 1},
		"control character":       {"bad\x00reason", 1},
		"newline":                 {"two\nlines", 1},
		"invalid utf-8":           {"bad\xffreason", 1},
		"expected count zero":     {"ok", 0},
		"expected count negative": {"ok", -1},
		"expected count over 50":  {"ok", 51},
	} {
		_, err := tm.env.Svc.RedriveDelivery(bg, tm.owner.Principal, tm.org.ID, id, redriveInput(c.reason, c.expected))
		var ae interface{ Error() string }
		if err == nil || !errors.As(err, &ae) || !strings.Contains(err.Error(), "validation_failed") {
			t.Errorf("%s: %v, want a validation error", name, err)
		}
	}
	unchanged(t, tm.env, id, before)
	// 500 characters (not bytes) is the longest accepted reason; the stored reason is trimmed.
	long := strings.Repeat("é", 500)
	if _, err := tm.env.Svc.RedriveDelivery(bg, tm.owner.Principal, tm.org.ID, id, redriveInput(long, 1)); err != nil {
		t.Fatalf("500 characters: %v", err)
	}
	var stored string
	q, _ := tm.env.Pool.Querier()
	if err := q.QueryRow(bg, "SELECT metadata->>'reason' FROM audit_events WHERE resource_id = $1", id).Scan(&stored); err != nil || stored != long {
		t.Fatalf("stored reason: %v", err)
	}
}

func TestRedriveRollsEverythingBackWhenTheAuditEventCannotBeWritten(t *testing.T) {
	tm := newTeam(t)
	env := tm.env
	dest := newDest(t, tm, "D")
	id := failedRow(t, env, tm.owner, dest, 8, false)
	before := rowOf(t, env, id)
	// A test-only trigger that refuses this one audit action.
	env.Exec(t, `CREATE FUNCTION test_refuse_redrive_audit() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN IF NEW.action = 'delivery.redriven' THEN RAISE EXCEPTION 'audit refused'; END IF; RETURN NEW; END; $$`)
	env.Exec(t, "CREATE TRIGGER test_refuse_redrive_audit BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION test_refuse_redrive_audit()")
	_, err := redrive(env.Svc, tm.owner.Principal, tm.org.ID, id, 1)
	if err == nil || !strings.Contains(err.Error(), "audit") {
		t.Fatalf("the redrive must fail when its audit event cannot be written: %v", err)
	}
	unchanged(t, env, id, before)
	if r := rowOf(t, env, id); r.Reason == nil || *r.Reason != "DESTINATION_REJECTED" {
		t.Fatalf("the failure reason must survive the rollback: %+v", r)
	}
	env.Exec(t, "DROP TRIGGER test_refuse_redrive_audit ON audit_events")
	if _, err := redrive(env.Svc, tm.owner.Principal, tm.org.ID, id, 1); err != nil {
		t.Fatalf("after the audit trigger is gone: %v", err)
	}
}

func TestRedriveCounterCountsOnlyAcceptedRedrives(t *testing.T) {
	tm := newTeam(t)
	counter := observability.NewRegistry().Counter("relayforge_delivery_redriven_total", "test")
	svc := redriveSvc(t, tm.env, 0, counter)
	dest := newDest(t, tm, "D")
	id := failedRow(t, tm.env, tm.owner, dest, 8, false)
	if _, err := redrive(svc, tm.owner.Principal, tm.org.ID, id, 2); err == nil { // stale
		t.Fatal("expected a conflict")
	}
	if counter.Value() != 0 {
		t.Fatalf("a rejected redrive was counted: %d", counter.Value())
	}
	if _, err := redrive(svc, tm.owner.Principal, tm.org.ID, id, 1); err != nil {
		t.Fatal(err)
	}
	if counter.Value() != 1 {
		t.Fatalf("counter %d, want 1", counter.Value())
	}
}

// ---- feeds and read models ----

func TestRedriveIsVisibleInTheFeedsAndTheQueueAndKeepsBothEpisodes(t *testing.T) {
	tm := newTeam(t)
	env := tm.env
	dest := newDest(t, tm, "D")
	id := seedDelivery(t, env, tm.owner, dest, 1, "FAILED")
	env.Clock.Advance(time.Minute) // the seed stamps real time; the feeds use the fake clock

	feed, err := env.Svc.FailureFeed(bg, tm.owner.Principal, tm.org.ID, app.FailureFeedQuery{})
	if err != nil || len(feed.Items) != 1 {
		t.Fatalf("before: %+v %v", feed, err)
	}
	if _, err := redrive(env.Svc, tm.owner.Principal, tm.org.ID, id, 1); err != nil {
		t.Fatal(err)
	}
	feed, err = env.Svc.FailureFeed(bg, tm.owner.Principal, tm.org.ID, app.FailureFeedQuery{})
	if err != nil || len(feed.Items) != 0 {
		t.Fatalf("a redriven delivery is not a current failure: %+v %v", feed, err)
	}
	q, err := env.Svc.QueueHealth(bg, tm.owner.Principal, tm.org.ID)
	if err != nil || q.Pending != 1 || q.Due != 1 {
		t.Fatalf("queue after redrive: %+v %v", q, err)
	}
	detail, err := env.Svc.GetDelivery(bg, tm.owner.Principal, tm.org.ID, id)
	if err != nil || detail.Delivery.Status != "PENDING" || detail.Delivery.MaxAttempts != 13 || len(detail.Attempts) != 1 || detail.Attempts[0].TerminalReason == nil {
		t.Fatalf("detail: %+v %v", detail, err)
	}

	// A second failure episode: attempt 2 fails again. Both terminal attempts are in the history, and the
	// delivery is a current failure again.
	appendAttempt(t, env, tm.org.ID, id, 2, 2, "FAILED")
	env.Clock.Advance(time.Minute)
	feed, err = env.Svc.FailureFeed(bg, tm.owner.Principal, tm.org.ID, app.FailureFeedQuery{})
	if err != nil || len(feed.Items) != 1 || feed.Items[0].AttemptCount != 2 {
		t.Fatalf("after the second failure: %+v %v", feed, err)
	}
	attempts, err := env.Svc.AttemptFeed(bg, tm.owner.Principal, tm.org.ID, app.AttemptFeedQuery{DeliveryID: id})
	if err != nil || len(attempts.Items) != 2 {
		t.Fatalf("attempt feed: %+v %v", attempts, err)
	}
	for _, it := range attempts.Items {
		if it.TerminalReason == nil {
			t.Errorf("attempt %d should carry its terminal reason: %+v", it.AttemptNumber, it)
		}
	}
	// The first request is now stale; a fresh one with the new attempt count works, from the current budget.
	_, err = redrive(env.Svc, tm.owner.Principal, tm.org.ID, id, 1)
	wantErr(t, "stale request after a second failure", err, app.ErrDeliveryChanged)
	res, err := redrive(env.Svc, tm.owner.Principal, tm.org.ID, id, 2)
	if err != nil || res.PreviousMaxAttempts != 13 || res.Delivery.MaxAttempts != 18 {
		t.Fatalf("second redrive: %+v %v", res, err)
	}
	if n := redriveAudits(t, env, id); n != 2 {
		t.Fatalf("%d audit events, want 2", n)
	}
}

// ---- concurrency and locking ----

func TestConcurrentRedrivesOfOneDeliveryHaveExactlyOneWinner(t *testing.T) {
	tm := newTeam(t)
	dest := newDest(t, tm, "D")
	id := failedRow(t, tm.env, tm.owner, dest, 8, false)
	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, errs[i] = redrive(tm.env.Svc, tm.owner.Principal, tm.org.ID, id, 1)
		}()
	}
	close(start)
	wg.Wait()
	wins := 0
	for _, err := range errs {
		switch {
		case err == nil:
			wins++
		case !errors.Is(err, app.ErrDeliveryNotFailed):
			t.Errorf("a loser got %v, want delivery_not_failed", err)
		}
	}
	if wins != 1 || rowOf(t, tm.env, id).Max != 13 || redriveAudits(t, tm.env, id) != 1 {
		t.Fatalf("%d winners, max %d, %d audit events: want exactly one winner, +5 once, one audit event", wins, rowOf(t, tm.env, id).Max, redriveAudits(t, tm.env, id))
	}
}

func TestRedrivesOfDifferentDeliveriesDoNotQueueBehindEachOther(t *testing.T) {
	tm := newTeam(t)
	env := tm.env
	dest := newDest(t, tm, "D")
	id := failedRow(t, env, tm.owner, dest, 8, false)
	// While another transaction holds the SHARED organization lock (a redrive in progress), a redrive of another
	// delivery is not blocked.
	release := holdTx(t, env, lockOrg(tm.org.ID, "SHARE"))
	done := make(chan error, 1)
	go func() { _, err := redrive(env.Svc, tm.owner.Principal, tm.org.ID, id, 1); done <- err }()
	err := within(t, "redrive beside a shared lock", done, 5*time.Second)
	release()
	if err != nil {
		t.Fatal(err)
	}
}

func TestRedriveExcludesMembershipChangesAndIsExcludedByThem(t *testing.T) {
	tm := newTeam(t)
	env := tm.env
	dest := newDest(t, tm, "D")

	t.Run("a role change waits for a redrive in progress", func(t *testing.T) {
		release := holdTx(t, env, lockOrg(tm.org.ID, "SHARE")) // what a redrive holds
		done := make(chan error, 1)
		go func() {
			done <- env.Svc.ChangeRole(bg, tm.owner.Principal, tm.org.ID, tm.developer.Principal.User.ID, tenancy.RoleViewer)
		}()
		notDone(t, "the role change", done, 400*time.Millisecond)
		release()
		if err := within(t, "the role change after the redrive ended", done, 5*time.Second); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("a redrive that waits for a demotion sees it", func(t *testing.T) {
		id := failedRow(t, env, tm.owner, dest, 8, false)
		release := holdTx(t, env, func(tx pgx.Tx) error {
			if err := lockOrg(tm.org.ID, "UPDATE")(tx); err != nil {
				return err
			}
			// The developer was demoted to VIEWER above; promote and demote again inside this transaction so the
			// committed state changes only when the lock is released.
			_, err := tx.Exec(bg, "UPDATE memberships SET role = 'VIEWER' WHERE organization_id = $1 AND user_id = $2", tm.org.ID, tm.admin.Principal.User.ID)
			return err
		})
		done := make(chan error, 1)
		go func() { _, err := redrive(env.Svc, tm.admin.Principal, tm.org.ID, id, 1); done <- err }()
		notDone(t, "the redrive", done, 400*time.Millisecond)
		release()
		if err := within(t, "the redrive after the demotion", done, 5*time.Second); !errors.Is(err, app.ErrForbidden) {
			t.Fatalf("the redrive must see the committed demotion: %v", err)
		}
		unchanged(t, env, id, rowOf(t, env, id))
	})
}

func TestRedriveAndDestinationRevocationNeverLeaveARedrivenDeliveryOnARevokedDestination(t *testing.T) {
	tm := newTeam(t)
	env := tm.env
	for i := 0; i < 12; i++ {
		dest := newDest(t, tm, fmt.Sprintf("Race %d", i))
		id := failedRow(t, env, tm.owner, dest, 8, false)
		var wg sync.WaitGroup
		var redriveErr, revokeErr error
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, redriveErr = redrive(env.Svc, tm.owner.Principal, tm.org.ID, id, 1)
		}()
		go func() {
			defer wg.Done()
			<-start
			_, revokeErr = env.Svc.RevokeDestination(bg, tm.owner.Principal, tm.org.ID, dest)
		}()
		close(start)
		wg.Wait()
		if revokeErr != nil {
			t.Fatalf("race %d: revoke: %v", i, revokeErr)
		}
		got := rowOf(t, env, id)
		switch {
		case redriveErr == nil:
			// Redrive first: revoke then cancelled the PENDING delivery.
			if got.Status != "CANCELLED" || redriveAudits(t, env, id) != 1 {
				t.Fatalf("race %d: redrive won, delivery %+v, %d audits", i, got, redriveAudits(t, env, id))
			}
		case errors.Is(redriveErr, app.ErrDestinationRevoked):
			if got.Status != "FAILED" || got.Max != 8 || redriveAudits(t, env, id) != 0 {
				t.Fatalf("race %d: revoke won, delivery %+v, %d audits", i, got, redriveAudits(t, env, id))
			}
		default:
			t.Fatalf("race %d: redrive: %v", i, redriveErr)
		}
		if got.Status == "PENDING" || got.Status == "IN_FLIGHT" {
			t.Fatalf("race %d: a delivery of a revoked destination is %s", i, got.Status)
		}
	}
}

func TestRevocationCompletesWhileRedrivesAreRunning(t *testing.T) {
	tm := newTeam(t)
	env := tm.env
	dest := newDest(t, tm, "Busy")
	ids := make([]string, 30)
	for i := range ids {
		ids[i] = failedRow(t, env, tm.owner, dest, 8, false)
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for w := 0; w < 3; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := w; i < len(ids); i += 3 {
				select {
				case <-stop:
					return
				default:
				}
				// Either outcome is fine; the point is that nothing deadlocks and revoke is not starved.
				_, _ = redrive(env.Svc, tm.owner.Principal, tm.org.ID, ids[i], 1)
			}
		}()
	}
	done := make(chan error, 1)
	go func() { _, err := env.Svc.RevokeDestination(bg, tm.owner.Principal, tm.org.ID, dest); done <- err }()
	if err := within(t, "revoke beside running redrives", done, 20*time.Second); err != nil {
		t.Fatal(err)
	}
	close(stop)
	wg.Wait()
	if n := env.Count(t, "SELECT count(*) FROM deliveries WHERE destination_id = $1 AND status IN ('PENDING', 'IN_FLIGHT')", dest); n != 0 {
		t.Fatalf("%d live deliveries on a revoked destination", n)
	}
}

func TestRedriveLockTimeoutIsTransactionLocalAndMapsToServiceUnavailable(t *testing.T) {
	tm := newTeam(t)
	env := tm.env
	dest := newDest(t, tm, "D")
	id := failedRow(t, env, tm.owner, dest, 8, false)
	svc := redriveSvc(t, env, 300*time.Millisecond, nil)
	q, _ := env.Pool.Querier()
	setting := func() string {
		var s string
		if err := q.QueryRow(bg, "SELECT setting FROM pg_settings WHERE name = 'lock_timeout'").Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	global := setting()

	check := func(label string, err error) {
		t.Helper()
		var ae interface{ Error() string }
		if !errors.Is(err, app.ErrLockTimeout) || !errors.As(err, &ae) {
			t.Fatalf("%s: %v, want lock_timeout", label, err)
		}
		if app.ErrLockTimeout.Status != 503 || app.ErrLockTimeout.Code != "lock_timeout" || app.ErrLockTimeout.RetryAfter <= 0 {
			t.Fatalf("lock timeout error: %+v", app.ErrLockTimeout)
		}
		unchanged(t, env, id, stillFailed(8))
	}

	t.Run("waiting for the organization lock", func(t *testing.T) {
		release := holdTx(t, env, lockOrg(tm.org.ID, "UPDATE"))
		defer release()
		start := time.Now()
		_, err := redrive(svc, tm.owner.Principal, tm.org.ID, id, 1)
		check("organization lock", err)
		if took := time.Since(start); took < 250*time.Millisecond || took > 4*time.Second {
			t.Fatalf("gave up after %v, want about 300ms", took)
		}
	})
	t.Run("waiting for the delivery row", func(t *testing.T) {
		release := holdTx(t, env, func(tx pgx.Tx) error {
			_, err := tx.Exec(bg, "SELECT 1 FROM deliveries WHERE id = $1 FOR NO KEY UPDATE", id)
			return err
		})
		defer release()
		_, err := redrive(svc, tm.owner.Principal, tm.org.ID, id, 1)
		check("delivery row", err)
	})
	t.Run("waiting for the destination row", func(t *testing.T) {
		release := holdTx(t, env, func(tx pgx.Tx) error {
			_, err := tx.Exec(bg, "SELECT 1 FROM destinations WHERE id = $1 FOR UPDATE", dest)
			return err
		})
		defer release()
		_, err := redrive(svc, tm.owner.Principal, tm.org.ID, id, 1)
		check("destination row", err)
	})

	// Nothing global or lingering: the server setting is untouched and a later transaction on any pooled
	// connection has the default timeout.
	if got := setting(); got != global {
		t.Fatalf("lock_timeout setting changed from %q to %q", global, got)
	}
	for i := 0; i < 10; i++ {
		var v string
		if err := q.QueryRow(bg, "SHOW lock_timeout").Scan(&v); err != nil {
			t.Fatal(err)
		}
		if v != "0" {
			t.Fatalf("a later session has lock_timeout %q: SET LOCAL leaked", v)
		}
	}
	if _, err := redrive(svc, tm.owner.Principal, tm.org.ID, id, 1); err != nil {
		t.Fatalf("once the locks are gone the redrive works: %v", err)
	}
}

// ---- the shared lock helper stays exclusive to redrive ----

func TestTheSharedOrganizationLockIsUsedOnlyByRedrive(t *testing.T) {
	allowed := map[string]bool{
		filepath.Join("tenancy", "store.go"):    true, // LockOrganizationShared is defined here
		filepath.Join("app", "org.go"):          true, // lockSharedAndAuthorize is defined here
		filepath.Join("app", "redrive.go"):      true, // and used here
		filepath.Join("app", "redrive_test.go"): true,
	}
	root := ".." // internal/
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if allowed[rel] || strings.HasSuffix(rel, "_test.go") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if s := string(raw); strings.Contains(s, "LockOrganizationShared(") || strings.Contains(s, "lockSharedAndAuthorize(") {
			t.Errorf("%s uses the shared organization lock, which is reserved for the redrive flow (ADR 34)", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// And redrive itself uses no exclusive organization lock.
	raw, err := os.ReadFile("redrive.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "lockAndAuthorize(") || strings.Contains(string(raw), "LockOrganization(") {
		t.Error("redrive.go must take the shared organization lock only")
	}
}

func mq(t *testing.T, env *apptest.Env) database.Querier {
	t.Helper()
	q, err := env.Pool.Querier()
	if err != nil {
		t.Fatal(err)
	}
	return q
}

// stillFailed is the state a rejected redrive must leave behind: FAILED with one attempt used, the given budget and a
// completion time (only its presence is compared).
func stillFailed(max int) deliveryRow {
	done := time.Now()
	return deliveryRow{Status: "FAILED", Attempts: 1, Max: max, Completed: &done}
}
