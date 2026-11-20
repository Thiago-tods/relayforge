package database_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Thiago-tods/relayforge/internal/database"
	"github.com/Thiago-tods/relayforge/internal/delivery"
)

func (f *deliveryFixture) queueAge(t *testing.T, org string, now time.Time) delivery.QueueAge {
	t.Helper()
	q, _ := f.env.Pool.Querier()
	out, err := delivery.QueueAgeOf(context.Background(), q, org, now)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func bucketCounts(a delivery.QueueAge) []int {
	out := make([]int, len(a.Buckets))
	for i, b := range a.Buckets {
		out[i] = b.Count
	}
	return out
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestQueueAgeBucketsAtTheExactBoundaries(t *testing.T) {
	f := newDeliveryFixture(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	s := time.Second
	// Overdue ages with the bucket each belongs to (half-open: min <= age < max), including one microsecond either side of
	// a boundary.
	cases := []struct {
		age  time.Duration
		want int
	}{
		{0, 0}, {1 * s, 0}, {59 * s, 0}, {60*s - time.Microsecond, 0},
		{60 * s, 1}, {60*s + time.Microsecond, 1}, {299 * s, 1}, {300*s - time.Microsecond, 1},
		{300 * s, 2}, {899 * s, 2},
		{900 * s, 3}, {3599 * s, 3},
		{3600 * s, 4}, {21599 * s, 4},
		{21600 * s, 5}, {86399 * s, 5}, {86400*s - time.Microsecond, 5},
		{86400 * s, 6}, {90000 * s, 6}, {10000 * 24 * time.Hour, 6},
	}
	want := make([]int, 7)
	for _, c := range cases {
		f.opDelivery(t, opRow{status: "PENDING", next: now.Add(-c.age)})
		want[c.want]++
	}
	got := f.queueAge(t, f.orgID, now)
	if !equalInts(bucketCounts(got), want) {
		t.Fatalf("buckets = %v, want %v", bucketCounts(got), want)
	}
	// The bounds the response states are the ones the buckets use.
	labels := []string{"<1m", "1-5m", "5-15m", "15-60m", "1-6h", "6-24h", ">24h"}
	mins := []int64{0, 60, 300, 900, 3600, 21600, 86400}
	for i, b := range got.Buckets {
		if b.Label != labels[i] || b.MinSeconds != mins[i] {
			t.Errorf("bucket %d = %+v", i, b)
		}
		if i < 6 && (b.MaxSeconds == nil || *b.MaxSeconds != mins[i+1]) {
			t.Errorf("bucket %d must end where the next one starts: %+v", i, b)
		}
	}
	if got.Buckets[6].MaxSeconds != nil {
		t.Error("the last bucket has no upper bound")
	}
	if !got.GeneratedAt.Equal(now) || got.Capped {
		t.Fatalf("generatedAt = %v, capped = %v", got.GeneratedAt, got.Capped)
	}
	// One microsecond earlier on the clock, every row is one microsecond younger: the row that was exactly on a lower
	// boundary falls into the bucket below, and the row that was due exactly now is no longer due.
	younger := make([]int, 7)
	for _, c := range cases {
		age := c.age - time.Microsecond
		if age < 0 {
			continue
		}
		idx := 0
		for i := len(mins) - 1; i >= 0; i-- {
			if age >= time.Duration(mins[i])*s {
				idx = i
				break
			}
		}
		younger[idx]++
	}
	if gotY := bucketCounts(f.queueAge(t, f.orgID, now.Add(-time.Microsecond))); !equalInts(gotY, younger) {
		t.Fatalf("one microsecond earlier: %v, want %v", gotY, younger)
	}
}

func TestQueueAgeCountsOnlyDuePendingDeliveriesOfTheOrganization(t *testing.T) {
	f := newDeliveryFixture(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	due := now.Add(-2 * time.Minute) // bucket 1-5m

	f.opDelivery(t, opRow{status: "PENDING", next: due})
	// Not due: the future, including one microsecond ahead, is excluded.
	f.opDelivery(t, opRow{status: "PENDING", next: now.Add(time.Microsecond)})
	f.opDelivery(t, opRow{status: "PENDING", next: now.Add(time.Hour)})
	// Not PENDING, whatever their dates.
	f.opDelivery(t, opRow{status: "IN_FLIGHT", next: due, lease: now.Add(-time.Hour)})
	f.opDelivery(t, opRow{status: "SUCCEEDED", next: due, completed: due})
	f.opDelivery(t, opRow{status: "FAILED", reason: "RETRIES_EXHAUSTED", next: due, completed: due})
	f.opDelivery(t, opRow{status: "CANCELLED", next: due, completed: due})
	// Another organization's overdue work.
	for i := 0; i < 4; i++ {
		f.opDelivery(t, opRow{org: f.org2, status: "PENDING", next: due})
	}
	if got := bucketCounts(f.queueAge(t, f.orgID, now)); !equalInts(got, []int{0, 1, 0, 0, 0, 0, 0}) {
		t.Fatalf("only the one due PENDING delivery of the organization: %v", got)
	}
	if got := bucketCounts(f.queueAge(t, f.org2, now)); !equalInts(got, []int{0, 4, 0, 0, 0, 0, 0}) {
		t.Fatalf("the other organization sees only its own: %v", got)
	}

	// A paused destination's deliveries are still due (due does not mean claimable), like the queue snapshot says.
	f.env.Exec(t, "UPDATE destinations SET status = 'PAUSED' WHERE id = $1", f.dest1)
	f.opDelivery(t, opRow{status: "PENDING", next: now.Add(-3 * time.Hour)})
	got := f.queueAge(t, f.orgID, now)
	if !equalInts(bucketCounts(got), []int{0, 1, 0, 0, 1, 0, 0}) {
		t.Fatalf("a paused destination's pending delivery still counts: %v", bucketCounts(got))
	}
	// Consistency with the queue snapshot taken at the same instant: the buckets add up to the due count.
	snap := f.queue(t, f.orgID, now)
	sum := 0
	for _, c := range bucketCounts(got) {
		sum += c
	}
	if sum != snap.Due {
		t.Fatalf("buckets add up to %d, the queue snapshot says %d are due", sum, snap.Due)
	}
}

// TestQueueAgeBucketsSaturateIndependentlyAtTheCap uses one bucket at 99,999, 100,000 and 100,001 deliveries
// and checks that the other buckets stay exact and that only the response-level flag follows the cap.
func TestQueueAgeBucketsSaturateIndependentlyAtTheCap(t *testing.T) {
	if testing.Short() {
		t.Skip("seeds 100,001 rows")
	}
	f := newDeliveryFixture(t)
	const cap = delivery.CountCap
	now := time.Now().UTC().Truncate(time.Microsecond)
	stamp := func(d time.Duration) string {
		return "'" + now.Add(-d).Format("2006-01-02 15:04:05.999999Z07:00") + "'::timestamptz"
	}
	cols := "status, attempt_count, next_attempt_at"
	seed := func(n int, age time.Duration) {
		f.bulkTo(t, f.orgID, f.ep1, f.dest1, n, cols, "'PENDING', 0, "+stamp(age), "now()")
	}
	seed(3, 10*time.Second) // <1m
	seed(2, 2*time.Hour)    // 1-6h
	seed(cap-1, 2*time.Minute)

	a := f.queueAge(t, f.orgID, now)
	if a.Capped || a.Buckets[1].Count != cap-1 || !equalInts(bucketCounts(a), []int{3, cap - 1, 0, 0, 2, 0, 0}) {
		t.Fatalf("99,999: %v capped=%v", bucketCounts(a), a.Capped)
	}
	seed(1, 2*time.Minute)
	a = f.queueAge(t, f.orgID, now)
	if !a.Capped || a.Buckets[1].Count != cap || a.Buckets[0].Count != 3 || a.Buckets[4].Count != 2 {
		t.Fatalf("100,000 saturates that bucket only: %v capped=%v", bucketCounts(a), a.Capped)
	}
	seed(1, 2*time.Minute)
	a = f.queueAge(t, f.orgID, now)
	if !a.Capped || a.Buckets[1].Count != cap || a.Buckets[0].Count != 3 || a.Buckets[4].Count != 2 {
		t.Fatalf("100,001 still reports the cap, never more: %v capped=%v", bucketCounts(a), a.Capped)
	}
	// Another bucket reaching the cap does not disturb the first one.
	seed(cap, 30*time.Hour)
	a = f.queueAge(t, f.orgID, now)
	if a.Buckets[1].Count != cap || a.Buckets[6].Count != cap || a.Buckets[0].Count != 3 || !a.Capped {
		t.Fatalf("two saturated buckets: %v", bucketCounts(a))
	}

	// The plan at this size: the organization's partial pending index, never a scan of the table.
	f.env.Exec(t, "ANALYZE deliveries")
	plan := f.explainCaptured(t, explainAnalyze, func(db database.Querier) {
		if _, err := delivery.QueueAgeOf(context.Background(), db, f.orgID, now); err != nil {
			t.Fatal(err)
		}
	})
	t.Logf("queue age plan with two saturated buckets:\n%s", plan)
	if !strings.Contains(plan, "deliveries_org_pending_idx") {
		t.Error("the distribution must use deliveries_org_pending_idx")
	}
	if strings.Contains(plan, "Seq Scan on deliveries") {
		t.Error("the distribution must not scan the table")
	}
	if strings.Contains(plan, "deliveries_due_idx") {
		t.Error("the global claim index is not the distribution's index")
	}
}

// TestQueueAgeAndFeedLeaveTheClaimPathAlone: ClaimDue still plans on the global due index, with the indexes of
// 00020 and 00021 present, and still claims.
func TestQueueAgeAndFeedLeaveTheClaimPathAlone(t *testing.T) {
	f := newDeliveryFixture(t)
	ctx := context.Background()
	now := time.Now().UTC()
	for i := 0; i < 5; i++ {
		f.opDelivery(t, opRow{status: "PENDING", next: now.Add(-time.Minute)})
	}
	var captured string
	real, _ := f.env.Pool.Querier()
	cq := &captureQuerier{Querier: real}
	err := f.env.Pool.InTx(ctx, func(tx pgx.Tx) error {
		cq.Querier = tx
		claimed, err := delivery.ClaimDue(ctx, cq, now, 3, time.Minute, "plan-check")
		if err != nil {
			return err
		}
		if len(claimed) != 3 {
			t.Errorf("ClaimDue claimed %d deliveries, want 3", len(claimed))
		}
		captured = cq.sql
		return errRollback
	})
	if err != errRollback {
		t.Fatal(err)
	}
	rows, err := real.Query(ctx, "EXPLAIN "+captured, now, 3, time.Minute.Seconds(), "plan-check")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, line)
	}
	text := strings.Join(plan, "\n")
	if !strings.Contains(text, "deliveries_due_idx") || strings.Contains(text, "deliveries_org_") {
		t.Errorf("the claim keeps the global due index and ignores the organization indexes:\n%s", text)
	}
}
