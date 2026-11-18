package delivery_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/Thiago-tods/relayforge/internal/delivery"
	"github.com/Thiago-tods/relayforge/internal/observability"
)

// TestQueueCountsReportAgesNotJustCounts: counts cannot tell a healthy backlog from a
// stalled one, ages can.
func TestQueueCountsReportAgesNotJustCounts(t *testing.T) {
	f := newFixture(t)
	org, user := f.owner.Org.ID, f.owner.Principal.User.ID
	d := f.destination(t, org, user, "D")
	q, _ := f.env.Pool.Querier()

	c, err := delivery.QueueCounts(bg, q, f.env.Clock.Now())
	if err != nil || c.OldestDueAge != 0 || c.Pending != 0 {
		t.Fatalf("empty queue: %+v %v", c, err)
	}

	for i := 0; i < 3; i++ {
		f.pending(t, org, f.event(t, fmt.Sprintf(`{"i":%d}`, i)), d.ID, 8)
	}
	now := f.env.Clock.Now().Add(100 * time.Second)
	c, err = delivery.QueueCounts(bg, q, now)
	if err != nil {
		t.Fatal(err)
	}
	// pending() makes each delivery due one second before the fake clock's now.
	if c.Pending != 3 || c.DuePending != 3 || c.OutboxUnpublished != 3 {
		t.Fatalf("counts: %+v", c)
	}
	if got := c.OldestDueAge; got < 100*time.Second || got > 102*time.Second {
		t.Fatalf("the oldest due delivery has waited about 101s, got %v", got)
	}
	var oldest time.Time
	if err := q.QueryRow(bg, "SELECT min(created_at) FROM outbox WHERE published_at IS NULL").Scan(&oldest); err != nil {
		t.Fatal(err)
	}
	if want := now.Sub(oldest); c.OldestOutboxAge != max(want, 0) {
		t.Fatalf("the oldest unpublished outbox row is %v old, got %v", want, c.OldestOutboxAge)
	}

	// A delivery that is not due yet has no age.
	earlier := f.env.Clock.Now().Add(-time.Hour)
	if c, _ = delivery.QueueCounts(bg, q, earlier); c.DuePending != 0 || c.OldestDueAge != 0 {
		t.Fatalf("a delivery that is not due has no age: %+v", c)
	}
}

func TestSampleGaugesPublishesTheAges(t *testing.T) {
	f := newFixture(t)
	org, user := f.owner.Org.ID, f.owner.Principal.User.ID
	d := f.destination(t, org, user, "D")
	f.pending(t, org, f.event(t, `{}`), d.ID, 8)
	f.env.Clock.Advance(5 * time.Minute)
	m := delivery.NewMetrics(observability.NewRegistry())
	w := f.worker(t, &fakeSender{}, func(dd *delivery.WorkerDeps) { dd.Metrics = m })
	if err := w.SampleGauges(bg); err != nil {
		t.Fatal(err)
	}
	if got := m.OldestDueAgeSeconds.Value(); got < 300 || got > 302 {
		t.Fatalf("oldest due age gauge = %d, want about 301", got)
	}
	if m.Pending.Value() != 1 || m.DuePending.Value() != 1 {
		t.Fatalf("gauges: pending %d due %d", m.Pending.Value(), m.DuePending.Value())
	}
}
