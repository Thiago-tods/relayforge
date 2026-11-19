package database_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Thiago-tods/relayforge/internal/delivery"
)

// The tests of this file seed hundreds of thousands of rows. They are skipped with -short. They check
// exact results and record plans and timings with t.Logf (run with -v to read them); none of them
// asserts how fast anything is, because that would be flaky on a shared machine.

const (
	opCap = delivery.CountCap
	// bulkCols is the column list bulk() and the helpers below use for terminal and pending rows.
	bulkCols = "status, attempt_count, last_http_status, completed_at, next_attempt_at, failure_reason"
)

func sqlTime(t time.Time) string {
	return "'" + t.UTC().Format("2006-01-02 15:04:05.999999+00") + "'::timestamptz"
}

// terminalValues is the value list (for bulkCols) of a terminal row completed at the given time.
func terminalValues(status string, completed time.Time) string {
	switch status {
	case "SUCCEEDED":
		return "'SUCCEEDED', 1, 200, " + sqlTime(completed) + ", now(), NULL"
	case "FAILED":
		return "'FAILED', 1, 500, " + sqlTime(completed) + ", now(), 'EGRESS_BLOCKED'"
	case "CANCELLED":
		return "'CANCELLED', 0, NULL, " + sqlTime(completed) + ", now(), NULL"
	}
	panic("unsupported status " + status)
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// TestCountersAtExactlyTheCap checks the behavior at the threshold with real rows: a counter with
// 99,999 matching rows is exact and does not mark the response capped; with exactly 100,000 and with
// 100,001 it reports 100,000 and marks it capped. Each scenario has its own one-hour window, so
// the scenarios do not see each other's rows.
func TestCountersAtExactlyTheCap(t *testing.T) {
	if testing.Short() {
		t.Skip("seeds about 500,000 rows")
	}
	f := newDeliveryFixture(t)
	base := time.Now().UTC().Truncate(time.Hour).Add(-10 * 24 * time.Hour)
	win := func(i int) (time.Time, time.Time) {
		from := base.Add(time.Duration(i) * 3 * time.Hour)
		return from, from.Add(time.Hour)
	}
	add := func(i, n int, status string) {
		from, _ := win(i)
		f.bulkTo(t, f.orgID, f.ep1, f.dest1, n, bulkCols, terminalValues(status, from.Add(2*time.Minute)), sqlTime(from.Add(time.Minute)))
	}
	by := func(s delivery.StatusCounts, status string) int {
		return map[string]int{"PENDING": s.Pending, "IN_FLIGHT": s.InFlight, "SUCCEEDED": s.Succeeded, "FAILED": s.Failed, "CANCELLED": s.Cancelled}[status]
	}
	done := func(c delivery.CompletedCohort, status string) int {
		return map[string]int{"SUCCEEDED": c.Succeeded, "FAILED": c.Failed, "CANCELLED": c.Cancelled}[status]
	}

	// n rows of one status exist in the window: the counters of that status are min(n, cap).
	check := func(t *testing.T, i int, status string, n int) {
		t.Helper()
		from, to := win(i)
		got := f.summary(t, f.orgID, from, to)
		want, saturated := minInt(n, opCap), n >= opCap
		if got.Capped != saturated {
			t.Errorf("%s n=%d: capped = %v, want %v", status, n, got.Capped, saturated)
		}
		if got.Created.Total != want || by(got.Created.ByStatus, status) != want || done(got.Completed, status) != want {
			t.Errorf("%s n=%d: created total %d, created %s %d, completed %s %d, want %d each (%+v)",
				status, n, got.Created.Total, status, by(got.Created.ByStatus, status), status, done(got.Completed, status), want, got)
		}
		for _, other := range []string{"SUCCEEDED", "FAILED", "CANCELLED"} {
			if other != status && (by(got.Created.ByStatus, other) != 0 || done(got.Completed, other) != 0) {
				t.Errorf("%s n=%d: %s must be 0: %+v", status, n, other, got)
			}
		}
		if status == "FAILED" {
			if got.Completed.ByFailureReason.EgressBlocked != want || got.Completed.ByFailureReason.RetriesExhausted != 0 || got.Completed.ByFailureReason.Unrecorded != 0 {
				t.Errorf("FAILED n=%d: failure reasons %+v, want EGRESS_BLOCKED %d", n, got.Completed.ByFailureReason, want)
			}
		}
		// The rate: failed over succeeded+failed, and null as soon as the succeeded or the failed count is saturated.
		switch {
		case status == "CANCELLED":
			if got.Completed.FailureRate != nil {
				t.Errorf("only cancelled deliveries: no denominator, the rate must be null: %v", *got.Completed.FailureRate)
			}
		case saturated:
			if got.Completed.FailureRate != nil {
				t.Errorf("%s n=%d: a saturated count makes the rate null, got %v", status, n, *got.Completed.FailureRate)
			}
		default:
			wantRate := map[string]float64{"SUCCEEDED": 0, "FAILED": 1}[status]
			if got.Completed.FailureRate == nil || *got.Completed.FailureRate != wantRate {
				t.Errorf("%s n=%d: below the cap the rate is exact (%v), got %v", status, n, wantRate, got.Completed.FailureRate)
			}
		}
	}

	for i, status := range []string{"SUCCEEDED", "FAILED", "CANCELLED"} {
		t.Run(status, func(t *testing.T) {
			add(i, opCap-1, status)
			check(t, i, status, opCap-1) // 99,999: exact, not capped
			add(i, 1, status)
			check(t, i, status, opCap) // exactly 100,000: reported as 100,000 and capped
			add(i, 1, status)
			check(t, i, status, opCap+1) // 100,001: still reported as 100,000
		})
	}

	t.Run("counters saturate independently of each other", func(t *testing.T) {
		// Window 1 holds 100,001 failed deliveries. Add counters that stay below the cap.
		add(1, 3, "SUCCEEDED")
		add(1, 2, "CANCELLED")
		from, to := win(1)
		got := f.summary(t, f.orgID, from, to)
		if !got.Capped || got.Completed.Failed != opCap || got.Created.ByStatus.Failed != opCap || got.Completed.ByFailureReason.EgressBlocked != opCap {
			t.Fatalf("failed must stay saturated: %+v", got)
		}
		if got.Completed.Succeeded != 3 || got.Created.ByStatus.Succeeded != 3 || got.Completed.Cancelled != 2 || got.Created.ByStatus.Cancelled != 2 {
			t.Fatalf("the unsaturated counters must stay exact next to a saturated one: %+v", got)
		}
		if got.Completed.FailureRate != nil {
			t.Fatalf("the failed count is saturated: the rate must be null, got %v", *got.Completed.FailureRate)
		}

		// Window 0 holds 100,001 succeeded deliveries. Saturate the failed counter too, and keep a small one.
		add(0, opCap, "FAILED")
		add(0, 3, "CANCELLED")
		from, to = win(0)
		got = f.summary(t, f.orgID, from, to)
		if !got.Capped || got.Completed.Succeeded != opCap || got.Completed.Failed != opCap || got.Created.Total != opCap ||
			got.Completed.ByFailureReason.EgressBlocked != opCap {
			t.Fatalf("both counters must be saturated: %+v", got)
		}
		if got.Completed.Cancelled != 3 || got.Created.ByStatus.Cancelled != 3 {
			t.Fatalf("cancelled must stay exact: %+v", got)
		}
		if got.Completed.FailureRate != nil {
			t.Fatalf("both sides saturated: the rate must be null, got %v", *got.Completed.FailureRate)
		}
	})

	t.Run("the queue at 99,999, 100,000 and 100,001 pending deliveries", func(t *testing.T) {
		pendingDue := "'PENDING', 0, NULL, NULL, now() - interval '1 hour', NULL"
		queue := func() delivery.QueueSnapshot { return f.queue(t, f.org2, time.Now().UTC()) }

		f.bulk(t, f.org2, opCap-1, pendingDue)
		if q := queue(); q.Pending != opCap-1 || q.Due != opCap-1 || q.Delayed != 0 || q.Capped {
			t.Fatalf("99,999 pending and due: exact and not capped: %+v", q)
		}
		f.bulk(t, f.org2, 1, pendingDue)
		if q := queue(); q.Pending != opCap || q.Due != opCap || !q.Capped {
			t.Fatalf("exactly 100,000: reported as 100,000 and capped: %+v", q)
		}
		f.bulk(t, f.org2, 1, pendingDue)
		if q := queue(); q.Pending != opCap || q.Due != opCap || !q.Capped {
			t.Fatalf("100,001: still reported as 100,000: %+v", q)
		}
		// Delayed is counted by its own predicate, so it stays exact next to two saturated counts, and so does the age.
		f.bulk(t, f.org2, 3, "'PENDING', 0, NULL, NULL, now() + interval '1 hour', NULL")
		q := queue()
		if q.Delayed != 3 || q.Pending != opCap || q.Due != opCap || !q.Capped || q.InFlight != 0 || q.ExpiredLeases != 0 {
			t.Fatalf("delayed must stay exact next to saturated pending and due: %+v", q)
		}
		if q.OldestDueAgeSeconds < 3599 || q.OldestDueAgeSeconds > 3700 {
			t.Fatalf("the oldest due age is a minimum, exact whatever the counts: %d", q.OldestDueAgeSeconds)
		}
	})
}

// TestRareBucketsOfALargeTenantOverAWideWindow records what the count cap does and does not bound.
// LIMIT 100,000 bounds the matching rows each counter returns; it does not bound the index entries or
// heap rows PostgreSQL examines to find them. A rare bucket (CANCELLED, FAILED, one failure reason) in
// a big tenant never reaches 100,000 matches, so it may examine most of the tenant's window. This test
// builds that case, checks the results are exact, and logs EXPLAIN (ANALYZE, BUFFERS) and wall-clock
// timings as evidence. It does not assert any duration.
func TestRareBucketsOfALargeTenantOverAWideWindow(t *testing.T) {
	if testing.Short() {
		t.Skip("seeds about 280,000 rows")
	}
	f := newDeliveryFixture(t)
	now := time.Now().UTC().Truncate(time.Second)
	from, to := now.Add(-30*24*time.Hour), now.Add(time.Hour) // 30 days and one hour: wide, inside the 31-day limit
	spread := "now() - random() * interval '29 days'"

	// The big tenant: 250,000 successes spread over 29 days. A second tenant adds noise.
	f.bulkTo(t, f.orgID, f.ep1, f.dest1, 250_000, bulkCols, "'SUCCEEDED', 1, 200, "+spread+", now(), NULL", spread)
	f.bulkTo(t, f.org2, f.ep2, f.dest2, 30_000, bulkCols, "'SUCCEEDED', 1, 200, "+spread+", now(), NULL", spread)

	// Rare buckets of the big tenant, scattered across the window.
	at := func(i int) time.Time { return now.Add(-time.Duration(1+(i%40)*16) * time.Hour) }
	for i := 0; i < 10; i++ {
		f.opDelivery(t, opRow{status: "CANCELLED", created: at(i), completed: at(i).Add(time.Minute)})
	}
	for reason, n := range map[string]int{"RETRIES_EXHAUSTED": 25, "INTERNAL_ERROR": 3, "DESTINATION_REJECTED": 2, "EGRESS_BLOCKED": 1} {
		for i := 0; i < n; i++ {
			f.opDelivery(t, opRow{status: "FAILED", reason: reason, created: at(i + 7), completed: at(i + 7).Add(time.Minute)})
		}
	}
	for i := 0; i < 2; i++ {
		f.legacyFailed(t, opRow{created: at(i + 3), completed: at(i + 3).Add(time.Minute)})
	}
	f.env.Exec(t, "ANALYZE deliveries")

	got := f.summary(t, f.orgID, from, to)
	r := got.Completed.ByFailureReason
	if !got.Capped || got.Created.Total != opCap || got.Completed.Succeeded != opCap || got.Completed.FailureRate != nil {
		t.Fatalf("the common bucket saturates and the rate is null: %+v", got)
	}
	if got.Completed.Cancelled != 10 || got.Created.ByStatus.Cancelled != 10 {
		t.Fatalf("rare CANCELLED must be exact: %+v", got)
	}
	if got.Completed.Failed != 33 || got.Created.ByStatus.Failed != 33 ||
		r != (delivery.ReasonCounts{RetriesExhausted: 25, InternalError: 3, DestinationRejected: 2, EgressBlocked: 1, Unrecorded: 2}) {
		t.Fatalf("rare FAILED and its reasons must be exact: failed %d, reasons %+v", got.Completed.Failed, r)
	}
	other := f.summary(t, f.org2, from, to)
	if other.Created.Total != 30_000 || other.Completed.Failed != 0 || other.Capped {
		t.Fatalf("the other tenant sees only its own rows: %+v", other)
	}

	timed := func(label string, from, to time.Time) {
		best := time.Hour
		for i := 0; i < 5; i++ {
			start := time.Now()
			f.summary(t, f.orgID, from, to)
			if d := time.Since(start); d < best {
				best = d
			}
		}
		t.Logf("EVIDENCE %s: SummaryOf, best of 5 wall-clock runs: %v", label, best)
	}
	timed("big tenant, 30-day window", from, to)
	timed("big tenant, 1-day window", now.Add(-24*time.Hour), to)
	timed("big tenant, 1-hour window", now.Add(-time.Hour), to)

	params := func(from, to time.Time) string {
		return fmt.Sprintf("'%s', '%s', '%s'", f.orgID, from.Format(time.RFC3339Nano), to.Format(time.RFC3339Nano))
	}
	t.Logf("EVIDENCE summary plan, big tenant, 30-day window (EXPLAIN ANALYZE, BUFFERS):\n%s", f.analyze(t, delivery.SummaryStatement, params(from, to)))
	t.Logf("EVIDENCE summary plan, big tenant, 1-day window:\n%s", f.analyze(t, delivery.SummaryStatement, params(now.Add(-24*time.Hour), to)))
	t.Logf("EVIDENCE summary plan, small tenant, 30-day window:\n%s", f.analyze(t, delivery.SummaryStatement,
		fmt.Sprintf("'%s', '%s', '%s'", f.org2, from.Format(time.RFC3339Nano), to.Format(time.RFC3339Nano))))

	// The same rare counter, alone, to see its own cost without the other fourteen.
	rare := `SELECT count(*) FROM (SELECT 1 FROM deliveries WHERE organization_id = $1 AND completed_at >= $2 AND completed_at < $3
	  AND status = 'FAILED' AND failure_reason = 'INTERNAL_ERROR' LIMIT 100000) c`
	t.Logf("EVIDENCE one rare counter (FAILED / INTERNAL_ERROR, 3 matches) over the 30-day window:\n%s", f.analyze(t, rare, params(from, to)))
	if strings.Contains(f.plan(t, rare, params(from, to)), "Seq Scan on deliveries") {
		t.Logf("EVIDENCE: for this wide window PostgreSQL chose a sequential scan of deliveries for the rare counter")
	}
}

// TestLeaseCountersWithManyInFlightRowsOnOtherTenants measures the in-flight and expired-lease counters of
// a small tenant while other tenants hold many IN_FLIGHT rows. Those counters use the global
// deliveries_lease_idx (lease_expires_at, id) WHERE status = 'IN_FLIGHT' with an organization filter, so
// their cost can grow with the global in-flight and expired-lease population (ADR 31, accepted Stage 1
// limitation). The test checks the results and logs plans and timings with and without the other tenants'
// rows; it asserts no duration.
func TestLeaseCountersWithManyInFlightRowsOnOtherTenants(t *testing.T) {
	if testing.Short() {
		t.Skip("seeds about 340,000 rows")
	}
	f := newDeliveryFixture(t)
	orgB, epB, destB := f.extraOrg(t, "org-three")
	cols := "status, attempt_count, lease_expires_at, claim_token, claimed_by, next_attempt_at"
	live := "'IN_FLIGHT', 1, now() + interval '10 minutes', gen_random_uuid(), 'w', now()"
	expired := "'IN_FLIGHT', 1, now() - interval '5 minutes', gen_random_uuid(), 'w', now()"

	// The target tenant: 40 in flight (10 of them expired) and 20 due pending.
	f.bulkTo(t, f.orgID, f.ep1, f.dest1, 30, cols, live, "now()")
	f.bulkTo(t, f.orgID, f.ep1, f.dest1, 10, cols, expired, "now()")
	f.bulk(t, f.orgID, 20, "'PENDING', 0, NULL, NULL, now() - interval '1 hour', NULL")
	f.env.Exec(t, "ANALYZE deliveries")

	timed := func(label string, org string) delivery.QueueSnapshot {
		best, got := time.Hour, delivery.QueueSnapshot{}
		for i := 0; i < 5; i++ {
			start := time.Now()
			got = f.queue(t, org, time.Now().UTC())
			if d := time.Since(start); d < best {
				best = d
			}
		}
		t.Logf("EVIDENCE %s: QueueOf, best of 5 wall-clock runs: %v (%+v)", label, best, got)
		return got
	}
	params := func(org string) string {
		return fmt.Sprintf("'%s', '%s'", org, time.Now().UTC().Format(time.RFC3339Nano))
	}
	inFlightOnly := `SELECT count(*) FROM (SELECT 1 FROM deliveries WHERE organization_id = $1 AND status = 'IN_FLIGHT' AND $2::timestamptz IS NOT NULL LIMIT 100000) c`
	expiredOnly := `SELECT count(*) FROM (SELECT 1 FROM deliveries WHERE organization_id = $1 AND status = 'IN_FLIGHT' AND lease_expires_at <= $2 LIMIT 100000) c`

	before := timed("small tenant alone (40 in flight, 10 expired)", f.orgID)
	if before.InFlight != 40 || before.ExpiredLeases != 10 || before.Pending != 20 || before.Due != 20 || before.Capped {
		t.Fatalf("the small tenant alone: %+v", before)
	}
	t.Logf("EVIDENCE in-flight counter plan, small tenant alone:\n%s", f.analyze(t, inFlightOnly, params(f.orgID)))
	t.Logf("EVIDENCE expired-lease counter plan, small tenant alone:\n%s", f.analyze(t, expiredOnly, params(f.orgID)))

	// Other tenants: org2 has 130,000 in flight (60,000 expired) and org3 60,000 (all expired).
	f.bulkTo(t, f.org2, f.ep2, f.dest2, 70_000, cols, live, "now()")
	f.bulkTo(t, f.org2, f.ep2, f.dest2, 60_000, cols, expired, "now()")
	f.bulkTo(t, orgB, epB, destB, 60_000, cols, expired, "now()")
	f.env.Exec(t, "ANALYZE deliveries")

	after := timed("small tenant while other tenants hold 190,000 in-flight rows (120,000 expired)", f.orgID)
	if after != (delivery.QueueSnapshot{GeneratedAt: after.GeneratedAt, Pending: 20, Due: 20, InFlight: 40, ExpiredLeases: 10, OldestDueAgeSeconds: after.OldestDueAgeSeconds}) {
		t.Fatalf("other tenants' rows must not change the small tenant's numbers: %+v", after)
	}
	t.Logf("EVIDENCE in-flight counter plan, small tenant, other tenants have 190,000 in-flight rows:\n%s", f.analyze(t, inFlightOnly, params(f.orgID)))
	t.Logf("EVIDENCE expired-lease counter plan, small tenant, other tenants have 120,000 expired leases:\n%s", f.analyze(t, expiredOnly, params(f.orgID)))
	t.Logf("EVIDENCE full queue statement plan, small tenant, other tenants loaded:\n%s", f.analyze(t, delivery.QueueStatement, params(f.orgID)))

	// The realistic worst case for these counters: a tenant with a long history (many terminal rows) and few in-flight
	// rows, next to tenants with many. Neither the global lease index (stepping over other tenants' entries) nor the
	// tenant's own created_at index (stepping over its history) is selective for it; the planner picks one.
	f.bulk(t, f.orgID, 150_000, "'SUCCEEDED', 1, 200, now(), now(), NULL")
	f.env.Exec(t, "ANALYZE deliveries")
	history := timed("small in-flight set, 150,000 terminal rows of history, other tenants hold 190,000 in-flight rows", f.orgID)
	if history.InFlight != 40 || history.ExpiredLeases != 10 || history.Pending != 20 || history.Capped {
		t.Fatalf("a long history must not change the in-flight numbers: %+v", history)
	}
	t.Logf("EVIDENCE in-flight counter plan, tenant with 150,000 history rows, other tenants loaded:\n%s", f.analyze(t, inFlightOnly, params(f.orgID)))
	t.Logf("EVIDENCE expired-lease counter plan, tenant with 150,000 history rows, other tenants loaded:\n%s", f.analyze(t, expiredOnly, params(f.orgID)))

	big := timed("tenant with 130,000 in flight (in-flight count saturates)", f.org2)
	if big.InFlight != opCap || big.ExpiredLeases != 60_000 || !big.Capped {
		t.Fatalf("a tenant over the cap: in flight saturates, expired stays exact: %+v", big)
	}
	third := timed("tenant with 60,000 in flight, all expired", orgB)
	if third.InFlight != 60_000 || third.ExpiredLeases != 60_000 || third.Capped || third.Pending != 0 {
		t.Fatalf("a tenant under the cap: %+v", third)
	}
}
