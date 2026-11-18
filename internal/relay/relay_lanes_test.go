package relay_test

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/Thiago-tods/relayforge/internal/observability"
	"github.com/Thiago-tods/relayforge/internal/relay"
)

// subscribeLongAgo subscribes one destination 60 days ago, so events of any age in these
// tests are inside its interval (ADR 26).
func (f *fixture) subscribeLongAgo(t *testing.T) {
	t.Helper()
	id := newUUID(t)
	f.env.Exec(t, `INSERT INTO destinations (id, organization_id, name, url, signing_secret_ciphertext, encryption_key_version, created_by_user_id)
		VALUES ($1, $2, 'd', 'https://hooks.example.com/x', decode(repeat('ab', 40), 'hex'), 1, $3)`, id, f.user.Org.ID, f.user.Principal.User.ID)
	f.env.Exec(t, `INSERT INTO subscriptions (organization_id, ingestion_endpoint_id, destination_id, created_by_user_id, created_at)
		VALUES ($1, $2, $3, $4, now() - interval '60 days')`, f.user.Org.ID, f.ep, id, f.user.Principal.User.ID)
}

// lostEvent ingests an event and moves it to the given age; its fan-out message was "lost".
func (f *fixture) lostEvent(t *testing.T, age time.Duration) string {
	t.Helper()
	ev := f.event(t)
	at := time.Now().UTC().Add(-age)
	f.env.Exec(t, "UPDATE webhook_events SET received_at = $2 WHERE id = $1", ev, at)
	f.env.Exec(t, "UPDATE outbox SET created_at = $2 WHERE event_id = $1", ev, at)
	return ev
}

func (f *fixture) unmarkedIn(t *testing.T, lane string) int {
	t.Helper()
	cond := "created_at >= now() - interval '24 hours'"
	if lane == "history" {
		cond = "created_at < now() - interval '24 hours'"
	}
	return f.env.Count(t, "SELECT count(*) FROM outbox WHERE fanned_out_at IS NULL AND "+cond)
}

func (f *fixture) consumerWith(t *testing.T, cfg relay.ConsumerConfig, m *relay.Metrics, buf *bytes.Buffer) *relay.FanOutConsumer {
	t.Helper()
	log := quiet()
	if buf != nil {
		log = slog.New(slog.NewTextHandler(buf, nil))
	}
	c, err := relay.NewFanOutConsumer(f.env.Pool, f.cons, cfg, log, m)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// A busy live lane must not stop the history lane from draining, and the other way round:
// each lane has its own budget in every sweep.
func TestHistoryLaneKeepsDrainingWhileTheLiveLaneIsBusy(t *testing.T) {
	f := newFixture(t)
	f.subscribeLongAgo(t)
	const perLane = 30
	for i := 0; i < perLane; i++ {
		f.lostEvent(t, time.Duration(10+i)*time.Minute)   // live lane
		f.lostEvent(t, time.Duration(3+i%5)*24*time.Hour) // history lane
	}
	cfg := relay.DefaultConsumerConfig()
	cfg.ReconcileLimit, cfg.ReconcileLiveBatches, cfg.ReconcileHistoryBatches = 5, 2, 2 // at most 10 rows per lane per sweep
	c := f.consumerWith(t, cfg, relay.NewMetrics(observability.NewRegistry()), nil)

	for sweep, want := range []int{20, 10, 0} {
		if _, err := c.Reconcile(bg); err != nil {
			t.Fatal(err)
		}
		live, history := f.unmarkedIn(t, "live"), f.unmarkedIn(t, "history")
		if live != want || history != want {
			t.Fatalf("after sweep %d: live lane %d unmarked, history lane %d, want %d each: one lane starved the other", sweep+1, live, history, want)
		}
	}
	if got := f.env.Count(t, "SELECT count(*) FROM deliveries"); got != 2*perLane {
		t.Fatalf("%d deliveries, want %d", got, 2*perLane)
	}
}

// poisonRows makes the delivery insert fail with an integrity error for every event in poison_events.
func (f *fixture) poisonRows(t *testing.T, events []string) {
	t.Helper()
	f.env.Exec(t, "CREATE TABLE poison_events (id uuid PRIMARY KEY)")
	for _, ev := range events {
		f.env.Exec(t, "INSERT INTO poison_events VALUES ($1)", ev)
	}
	f.env.Exec(t, `CREATE FUNCTION inject_poison() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN IF EXISTS (SELECT 1 FROM poison_events WHERE id = NEW.event_id) THEN
			RAISE EXCEPTION 'injected failure, payload SECRET-PAYLOAD-7' USING ERRCODE = 'integrity_constraint_violation';
		END IF; RETURN NEW; END $$`)
	f.env.Exec(t, "CREATE TRIGGER inject_poison BEFORE INSERT ON deliveries FOR EACH ROW EXECUTE FUNCTION inject_poison()")
}

func TestRowFailuresAreCountedLoggedWithinABoundAndTheAgeGaugeShowsThem(t *testing.T) {
	f := newFixture(t)
	f.subscribeLongAgo(t)
	var poison []string
	for i := 0; i < 15; i++ {
		poison = append(poison, f.lostEvent(t, 30*time.Minute+time.Duration(i)*time.Second))
	}
	for i := 0; i < 5; i++ {
		f.lostEvent(t, 5*time.Minute+time.Duration(i)*time.Second)
	}
	f.poisonRows(t, poison)

	cfg := relay.DefaultConsumerConfig()
	cfg.ReconcileLimit = 50
	m := relay.NewMetrics(observability.NewRegistry())
	var logs bytes.Buffer
	c := f.consumerWith(t, cfg, m, &logs)

	created, err := c.Reconcile(bg)
	if err != nil || created != 5 {
		t.Fatalf("created %d (%v): the five healthy rows must go through", created, err)
	}
	if m.ReconcileRowFailures.Value() != 15 || m.ReconcileErrors.Value() != 0 || m.ReconciledEvents.Value() != 5 {
		t.Fatalf("metrics: row failures %d (want 15), sweep errors %d (want 0), events %d (want 5)",
			m.ReconcileRowFailures.Value(), m.ReconcileErrors.Value(), m.ReconciledEvents.Value())
	}
	out := logs.String()
	if got := strings.Count(out, "could not process an outbox row"); got != 10 {
		t.Fatalf("%d row-failure log lines, want the bound of 10:\n%s", got, out)
	}
	if !strings.Contains(out, "failures=15") || !strings.Contains(out, "logged=10") {
		t.Fatalf("the suppressed failures must be summarized:\n%s", out)
	}
	for _, banned := range []string{"SECRET-PAYLOAD-7", "hooks.example.com", "injected failure"} {
		if strings.Contains(out, banned) {
			t.Fatalf("the diagnostics leak %q:\n%s", banned, out)
		}
	}
	if !strings.Contains(out, "error_class=sqlstate:23000") || !strings.Contains(out, "outbox_id=") || !strings.Contains(out, "event_id=") {
		t.Fatalf("the diagnostics must name the row and the class:\n%s", out)
	}
	// 15 rows are 30+ minutes old and stuck: the live-lane age gauge says so.
	if age := m.OldestLiveUnmarkedAge.Value(); age < 30*60-10 || age > 31*60+60 {
		t.Fatalf("oldest live unmarked age = %ds, want about 1800s", age)
	}
	if m.OldestHistoryUnmarkedAge.Value() != 0 {
		t.Fatalf("the history lane is empty: %d", m.OldestHistoryUnmarkedAge.Value())
	}

	// The fault is fixed: the next sweep recovers every stuck row, counters stop moving, the gauge clears.
	f.env.Exec(t, "DROP TRIGGER inject_poison ON deliveries")
	if created, err := c.Reconcile(bg); err != nil || created != 15 {
		t.Fatalf("recovery sweep created %d (%v)", created, err)
	}
	if m.ReconcileRowFailures.Value() != 15 || m.OldestLiveUnmarkedAge.Value() != 0 {
		t.Fatalf("after recovery: failures %d, age %d", m.ReconcileRowFailures.Value(), m.OldestLiveUnmarkedAge.Value())
	}
	if f.env.Count(t, "SELECT count(*) FROM outbox WHERE fanned_out_at IS NULL") != 0 {
		t.Fatal("rows are still unmarked")
	}
}

func TestASweepStopsOnceTooManyRowsFailed(t *testing.T) {
	f := newFixture(t)
	f.subscribeLongAgo(t)
	var poison []string
	for i := 0; i < 30; i++ {
		poison = append(poison, f.lostEvent(t, 20*time.Minute+time.Duration(i)*time.Second))
	}
	f.poisonRows(t, poison)
	cfg := relay.DefaultConsumerConfig()
	cfg.ReconcileLimit, cfg.ReconcileMaxRowFailures = 3, 4
	m := relay.NewMetrics(observability.NewRegistry())
	c := f.consumerWith(t, cfg, m, nil)

	if _, err := c.Reconcile(bg); err != nil {
		t.Fatal(err)
	}
	// Batch 1 fails 3 rows (3 < 4: go on), batch 2 fails 3 more (6 >= 4: stop). The 10-batch
	// budget and the 30 stuck rows do not turn the sweep into an unbounded series of retries.
	if got := m.ReconcileRowFailures.Value(); got != 6 {
		t.Fatalf("%d row failures in one sweep, want 6 (bounded by ReconcileMaxRowFailures)", got)
	}
}

func TestATransientErrorFailsTheSweepAndIsNotARowFailure(t *testing.T) {
	f := newFixture(t)
	f.subscribeLongAgo(t)
	for i := 0; i < 4; i++ {
		f.lostEvent(t, 15*time.Minute)
	}
	f.env.Exec(t, `CREATE FUNCTION inject_outage() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'injected' USING ERRCODE = 'serialization_failure'; END $$`)
	f.env.Exec(t, "CREATE TRIGGER inject_outage BEFORE INSERT ON deliveries FOR EACH ROW EXECUTE FUNCTION inject_outage()")
	m := relay.NewMetrics(observability.NewRegistry())
	c := f.consumerWith(t, relay.DefaultConsumerConfig(), m, nil)

	if _, err := c.Reconcile(bg); err == nil {
		t.Fatal("a transient error must fail the sweep")
	}
	if m.ReconcileErrors.Value() != 1 || m.ReconcileRowFailures.Value() != 0 {
		t.Fatalf("sweep errors %d (want 1), row failures %d (want 0)", m.ReconcileErrors.Value(), m.ReconcileRowFailures.Value())
	}
	if f.env.Count(t, "SELECT count(*) FROM outbox WHERE fanned_out_at IS NULL") != 4 {
		t.Fatal("the failed sweep must leave every row unmarked")
	}
}
