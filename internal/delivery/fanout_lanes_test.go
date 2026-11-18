package delivery_test

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Thiago-tods/relayforge/internal/delivery"
)

// seedHistoricalBacklog inserts n events with their unmarked outbox rows, all older than
// the live window: what migration 00018 leaves behind. Rows are real (foreign keys hold).
func (f *fixture) seedHistoricalBacklog(t *testing.T, org string, n int) {
	t.Helper()
	f.env.Exec(t, `
		WITH ev AS (
			INSERT INTO webhook_events (id, organization_id, ingestion_endpoint_id, idempotency_key, payload, payload_sha256, content_type, received_at)
			SELECT id, $1::uuid, $2::uuid, id::text, '\x78'::bytea, decode(repeat('00', 32), 'hex'), 'application/json', at
			  FROM (SELECT gen_random_uuid() AS id, now() - interval '25 hours' - random() * interval '28 days' AS at FROM generate_series(1, $3::int)) g
			RETURNING id, received_at)
		INSERT INTO outbox (organization_id, event_id, message_type, payload, created_at, available_at, published_at)
		SELECT $1::uuid, id, 'webhook_event.accepted', '{}', received_at, received_at, received_at FROM ev`, org, f.ep, n)
}

func TestRecentLossRecoversPromptlyDespiteAHugeHistoricalBacklog(t *testing.T) {
	f := newFixture(t)
	org, user := f.owner.Org.ID, f.owner.Principal.User.ID
	d := f.destination(t, org, user, "D")
	f.subscribeLongAgo(t, org, d.ID)
	const history = 110_000
	f.seedHistoricalBacklog(t, org, history)

	var lost []string
	for i := 0; i < 5; i++ {
		ev := f.event(t, fmt.Sprintf(`{"lost":%d}`, i))
		f.backdate(t, ev, time.Now().UTC().Add(-time.Duration(10+i*5)*time.Minute))
		lost = append(lost, ev)
	}
	ahead := f.env.Count(t, "SELECT count(*) FROM outbox WHERE fanned_out_at IS NULL AND created_at < now() - interval '1 hour'")
	if ahead < history {
		t.Fatalf("the test needs the backlog to sit ahead of the new losses: %d", ahead)
	}

	// ONE live-lane batch repairs every recent loss: history never stands in front of it.
	res, err := reconcileLane(f, delivery.LaneLive, 500)
	if err != nil || res.Events != 5 || res.Deliveries != 5 || res.Examined != 5 {
		t.Fatalf("live lane: %+v (%v)", res, err)
	}
	for _, ev := range lost {
		if f.deliveryCount(t, ev) != 1 || f.marker(t, ev) == nil {
			t.Fatalf("recent loss %s was not repaired", ev)
		}
	}
	if left := f.env.Count(t, "SELECT count(*) FROM outbox WHERE fanned_out_at IS NULL"); left != history {
		t.Fatalf("the live lane touched the history lane: %d unmarked, want %d", left, history)
	}
}

func TestHistoryLaneDrainsAndRecoversRowsOfAnyAge(t *testing.T) {
	f := newFixture(t)
	org, user := f.owner.Org.ID, f.owner.Principal.User.ID
	d := f.destination(t, org, user, "D")
	f.subscribeLongAgo(t, org, d.ID)
	old := f.event(t, `{"age":"30d"}`)
	f.backdate(t, old, time.Now().UTC().Add(-30*24*time.Hour))
	older := f.event(t, `{"age":"50d"}`)
	f.backdate(t, older, time.Now().UTC().Add(-50*24*time.Hour))

	if res, err := reconcileLane(f, delivery.LaneLive, 100); err != nil || res.Examined != 0 {
		t.Fatalf("the live lane must not see rows older than the live window: %+v %v", res, err)
	}
	res, err := reconcileLane(f, delivery.LaneHistory, 100)
	if err != nil || res.Events != 2 || res.Deliveries != 2 {
		t.Fatalf("history lane: %+v %v", res, err)
	}
	if f.deliveryCount(t, old) != 1 || f.deliveryCount(t, older) != 1 {
		t.Fatal("old losses were not repaired")
	}
}

func TestLaneBoundaryIsExactAndAppliesToExactlyOneLane(t *testing.T) {
	f := newFixture(t)
	org, user := f.owner.Org.ID, f.owner.Principal.User.ID
	d := f.destination(t, org, user, "D")
	f.subscribeLongAgo(t, org, d.ID)
	now := time.Now().UTC().Truncate(time.Microsecond)
	onBoundary := f.event(t, `{"edge":"on"}`)
	justOlder := f.event(t, `{"edge":"older"}`)
	justYounger := f.event(t, `{"edge":"younger"}`)
	f.backdate(t, onBoundary, now.Add(-delivery.DefaultLiveWindow))
	f.backdate(t, justOlder, now.Add(-delivery.DefaultLiveWindow-time.Microsecond))
	f.backdate(t, justYounger, now.Add(-delivery.DefaultLiveWindow+time.Microsecond))

	live, err := reconcileLaneAt(f, delivery.LaneLive, now, 100)
	if err != nil || live.Examined != 2 {
		t.Fatalf("live lane (>= the boundary): %+v %v", live, err)
	}
	if f.marker(t, onBoundary) == nil || f.marker(t, justYounger) == nil || f.marker(t, justOlder) != nil {
		t.Fatal("the row exactly on the boundary belongs to the live lane, the one a microsecond older to history")
	}
	history, err := reconcileLaneAt(f, delivery.LaneHistory, now, 100)
	if err != nil || history.Examined != 1 || f.marker(t, justOlder) == nil {
		t.Fatalf("history lane (< the boundary): %+v %v", history, err)
	}
}

// ---- concurrency ----

func TestConcurrentReconcilersProcessDisjointRowsAcrossBothLanes(t *testing.T) {
	f := newFixture(t)
	org, user := f.owner.Org.ID, f.owner.Principal.User.ID
	d1, d2 := f.destination(t, org, user, "D1"), f.destination(t, org, user, "D2")
	f.subscribeLongAgo(t, org, d1.ID)
	f.subscribeLongAgo(t, org, d2.ID)
	const perLane = 40
	for i := 0; i < perLane; i++ {
		live := f.event(t, fmt.Sprintf(`{"live":%d}`, i))
		f.backdate(t, live, time.Now().UTC().Add(-time.Duration(1+i%20)*time.Hour))
		old := f.event(t, fmt.Sprintf(`{"old":%d}`, i))
		f.backdate(t, old, time.Now().UTC().Add(-time.Duration(2+i%20)*24*time.Hour))
	}

	var marked atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for w := 0; w < 6; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			<-start
			lanes := []delivery.Lane{delivery.LaneLive, delivery.LaneHistory}
			for idle := 0; idle < 2; {
				progressed := false
				for i := range lanes {
					res, err := reconcileLane(f, lanes[(i+w)%2], 3)
					if err != nil {
						t.Error(err)
						return
					}
					marked.Add(res.Events)
					progressed = progressed || res.Examined > 0
				}
				if progressed {
					idle = 0
				} else {
					idle++
				}
			}
		}(w)
	}
	close(start)
	wg.Wait()

	if marked.Load() != 2*perLane {
		t.Fatalf("the reconcilers marked %d rows, want exactly %d: a row was missed or processed twice", marked.Load(), 2*perLane)
	}
	if f.unmarked(t) != 0 {
		t.Fatalf("%d rows left", f.unmarked(t))
	}
	if got := f.env.Count(t, "SELECT count(*) FROM deliveries"); got != 2*perLane*2 {
		t.Fatalf("%d deliveries, want %d", got, 2*perLane*2)
	}
}

// TestBoundaryCrossingRowsAreNeitherLostNorProcessedTwice moves rows across the lane
// boundary while several reconcilers run, each with its own clock reading.
func TestBoundaryCrossingRowsAreNeitherLostNorProcessedTwice(t *testing.T) {
	f := newFixture(t)
	org, user := f.owner.Org.ID, f.owner.Principal.User.ID
	d := f.destination(t, org, user, "D")
	f.subscribeLongAgo(t, org, d.ID)
	const rows = 60
	base := time.Now().UTC()
	for i := 0; i < rows; i++ {
		ev := f.event(t, fmt.Sprintf(`{"b":%d}`, i))
		// Within +-30 s of the boundary, so different clock readings put a row in different lanes.
		f.backdate(t, ev, base.Add(-delivery.DefaultLiveWindow).Add(time.Duration(i-rows/2)*time.Second))
	}

	var marked atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for w := 0; w < 6; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			<-start
			for round := 0; round < 40; round++ {
				// Every call reads its own "now": rows drift between the lanes from call to call.
				now := base.Add(time.Duration((round*7+w*3)%90-45) * time.Second)
				for _, lane := range []delivery.Lane{delivery.LaneLive, delivery.LaneHistory} {
					res, err := reconcileLaneAt(f, lane, now.Add(time.Duration(w)*time.Second), 4)
					if err != nil {
						t.Error(err)
						return
					}
					marked.Add(res.Events)
				}
			}
		}(w)
	}
	close(start)
	wg.Wait()

	// Drain anything the shifting clocks left (rows still inside the grace period of some readings).
	for _, lane := range []delivery.Lane{delivery.LaneLive, delivery.LaneHistory} {
		res, err := reconcileLaneAt(f, lane, base.Add(time.Hour), 500)
		if err != nil {
			t.Fatal(err)
		}
		marked.Add(res.Events)
	}
	if marked.Load() != rows || f.unmarked(t) != 0 {
		t.Fatalf("marked %d of %d rows (%d unmarked): rows crossing the boundary were lost or processed twice", marked.Load(), rows, f.unmarked(t))
	}
	if got := f.env.Count(t, "SELECT count(*) FROM deliveries"); got != rows {
		t.Fatalf("%d deliveries for %d events", got, rows)
	}
}
