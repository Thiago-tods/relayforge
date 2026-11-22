package database_test

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Thiago-tods/relayforge/internal/database"
	"github.com/Thiago-tods/relayforge/internal/delivery"
)

// attemptEpoch is a fixed instant on a microsecond grid, so start times compare exactly.
var attemptEpoch = time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

// seedAttempts runs fn with the claim-fence trigger of delivery_attempts disabled inside one transaction, so a test can
// place attempts at exact times without driving a delivery through its whole lifecycle. DDL is transactional in
// PostgreSQL: the trigger is enabled again before the transaction commits, and every other constraint (the outcome
// and status consistency, the foreign key, the unique attempt number) stays in force.
func (f *deliveryFixture) seedAttempts(t *testing.T, fn func(ctx context.Context, tx pgx.Tx) error) {
	t.Helper()
	ctx := context.Background()
	err := f.env.Pool.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "ALTER TABLE delivery_attempts DISABLE TRIGGER delivery_attempts_check_claimed"); err != nil {
			return err
		}
		if err := fn(ctx, tx); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, "ALTER TABLE delivery_attempts ENABLE TRIGGER delivery_attempts_check_claimed")
		return err
	})
	if err != nil {
		t.Fatalf("seed attempts: %v", err)
	}
}

// attemptSpec is one attempt to seed. HTTP must be nil for the outcomes that carry no status.
type attemptSpec struct {
	org, delivery string
	number        int
	started       time.Time
	outcome       string
	http          any
	class, reason any
}

func (f *deliveryFixture) addAttempts(t *testing.T, specs ...attemptSpec) []string {
	t.Helper()
	ids := make([]string, 0, len(specs))
	f.seedAttempts(t, func(ctx context.Context, tx pgx.Tx) error {
		for _, s := range specs {
			org := s.org
			if org == "" {
				org = f.orgID
			}
			var id string
			if err := tx.QueryRow(ctx, `INSERT INTO delivery_attempts (organization_id, delivery_id, attempt_number, started_at, finished_at, outcome,
				http_status, error_class, duration_ms, terminal_reason) VALUES ($1, $2, $3, $4, $4::timestamptz + interval '120 milliseconds', $5, $6, $7, 120, $8)
				RETURNING id::text`, org, s.delivery, s.number, s.started.UTC(), s.outcome, s.http, s.class, s.reason).Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return nil
	})
	return ids
}

// attemptWindow is a window wide enough for every fixture time.
func attemptWindow() (time.Time, time.Time) {
	return attemptEpoch.Add(-24 * time.Hour), attemptEpoch.Add(24 * time.Hour)
}

func (f *deliveryFixture) attemptFeed(t *testing.T, org string, filter delivery.AttemptFilter) delivery.AttemptFeed {
	t.Helper()
	q, _ := f.env.Pool.Querier()
	if filter.Limit == 0 {
		filter.Limit = 100
	}
	if filter.From.IsZero() {
		filter.From, _ = attemptWindow()
	}
	if filter.To.IsZero() {
		_, filter.To = attemptWindow()
	}
	out, err := delivery.AttemptFeedOf(context.Background(), q, org, filter, attemptEpoch)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func attemptIDs(items []delivery.AttemptActivityItem) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.AttemptID
	}
	return out
}

// pageThroughAttempts follows NextCursor until the feed ends.
func pageThroughAttempts(t *testing.T, q database.Querier, org string, f delivery.AttemptFilter) []delivery.AttemptActivityItem {
	t.Helper()
	var all []delivery.AttemptActivityItem
	for pages := 0; ; pages++ {
		if pages > 100000 {
			t.Fatal("the attempt feed does not terminate")
		}
		page, err := delivery.AttemptFeedOf(context.Background(), q, org, f, attemptEpoch)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, page.Items...)
		if page.NextCursor == nil {
			return all
		}
		c, err := delivery.ParseAttemptCursor(*page.NextCursor)
		if err != nil {
			t.Fatalf("the feed issued a cursor it cannot read: %v", err)
		}
		f.After = &c
	}
}

func TestAttemptFeedOrdersByStartThenIDAndPagesWithoutGapsOrDuplicates(t *testing.T) {
	f := newDeliveryFixture(t)
	// 23 attempts that share only three start instants, so most of the order is decided by the attempt id.
	var specs []attemptSpec
	for i := 0; i < 23; i++ {
		d := f.opDelivery(t, opRow{status: "SUCCEEDED", completed: attemptEpoch, created: attemptEpoch.Add(-time.Hour)})
		specs = append(specs, attemptSpec{delivery: d, number: 1, started: attemptEpoch.Add(time.Duration(i%3) * time.Minute), outcome: "SUCCESS", http: 200})
	}
	f.addAttempts(t, specs...)
	all := f.attemptFeed(t, f.orgID, delivery.AttemptFilter{})
	if len(all.Items) != 23 || all.NextCursor != nil {
		t.Fatalf("one page of 100 holds all 23 attempts and has no cursor: %d, %v", len(all.Items), all.NextCursor)
	}
	want := all.Items
	if !sort.SliceIsSorted(want, func(i, j int) bool {
		if !want[i].StartedAt.Equal(want[j].StartedAt) {
			return want[i].StartedAt.After(want[j].StartedAt)
		}
		return want[i].AttemptID > want[j].AttemptID
	}) {
		t.Fatalf("the feed must be ordered by started_at DESC, id DESC: %v", attemptIDs(want))
	}
	q, _ := f.env.Pool.Querier()
	for _, size := range []int{1, 2, 4, 7, 22, 23, 24} {
		from, to := attemptWindow()
		got := pageThroughAttempts(t, q, f.orgID, delivery.AttemptFilter{From: from, To: to, Limit: size})
		if strings.Join(attemptIDs(got), ",") != strings.Join(attemptIDs(want), ",") {
			t.Errorf("page size %d: the pages do not add up to the whole feed:\n got %v\nwant %v", size, attemptIDs(got), attemptIDs(want))
		}
	}
	// A page of exactly the remaining rows has no cursor: the extra row read is what tells.
	page := f.attemptFeed(t, f.orgID, delivery.AttemptFilter{Limit: 23})
	if page.NextCursor != nil {
		t.Error("a page that holds everything must not offer a next page")
	}
	page = f.attemptFeed(t, f.orgID, delivery.AttemptFilter{Limit: 22})
	if page.NextCursor == nil || len(page.Items) != 22 {
		t.Errorf("22 of 23 leaves one more: %d items, cursor %v", len(page.Items), page.NextCursor)
	}
}

func TestAttemptFeedWindowIsHalfOpenOnStartedAt(t *testing.T) {
	f := newDeliveryFixture(t)
	d := f.opDelivery(t, opRow{status: "SUCCEEDED", completed: attemptEpoch, created: attemptEpoch.Add(-time.Hour)})
	from, to := attemptEpoch, attemptEpoch.Add(time.Hour)
	ids := f.addAttempts(t,
		attemptSpec{delivery: d, number: 1, started: from.Add(-time.Microsecond), outcome: "TIMEOUT"},
		attemptSpec{delivery: d, number: 2, started: from, outcome: "TIMEOUT"},
		attemptSpec{delivery: d, number: 3, started: to.Add(-time.Microsecond), outcome: "TIMEOUT"},
		attemptSpec{delivery: d, number: 4, started: to, outcome: "TIMEOUT"},
	)
	got := f.attemptFeed(t, f.orgID, delivery.AttemptFilter{From: from, To: to})
	if want := []string{ids[2], ids[1]}; strings.Join(attemptIDs(got.Items), ",") != strings.Join(want, ",") {
		t.Fatalf("[from, to) must include from and exclude to: got %v, want %v", attemptIDs(got.Items), want)
	}
	if !got.From.Equal(from) || !got.To.Equal(to) || !got.GeneratedAt.Equal(attemptEpoch) {
		t.Errorf("the page reports its window and clock: %v %v %v", got.From, got.To, got.GeneratedAt)
	}
}

func TestAttemptFeedFiltersByOutcomeAndDeliveryAndShowsAbandonedAttempts(t *testing.T) {
	f := newDeliveryFixture(t)
	a := f.opDelivery(t, opRow{status: "FAILED", reason: "RETRIES_EXHAUSTED", completed: attemptEpoch, created: attemptEpoch.Add(-time.Hour)})
	b := f.opDelivery(t, opRow{status: "SUCCEEDED", completed: attemptEpoch, created: attemptEpoch.Add(-time.Hour)})
	at := func(m int) time.Time { return attemptEpoch.Add(time.Duration(m) * time.Minute) }
	ids := f.addAttempts(t,
		attemptSpec{delivery: a, number: 1, started: at(1), outcome: "ABANDONED"},
		attemptSpec{delivery: a, number: 2, started: at(2), outcome: "NETWORK_ERROR", class: "network"},
		attemptSpec{delivery: a, number: 3, started: at(3), outcome: "HTTP_ERROR", http: 503, class: "http_5xx"},
		attemptSpec{delivery: a, number: 4, started: at(4), outcome: "HTTP_ERROR", http: 404, class: "http_4xx", reason: "RETRIES_EXHAUSTED"},
		attemptSpec{delivery: b, number: 1, started: at(5), outcome: "SUCCESS", http: 200},
		attemptSpec{delivery: b, number: 2, started: at(6), outcome: "REDIRECT", http: 302},
		attemptSpec{delivery: b, number: 3, started: at(7), outcome: "BLOCKED_BY_POLICY", class: "egress_blocked"},
		attemptSpec{delivery: b, number: 4, started: at(8), outcome: "ABANDONED"},
	)
	// An ABANDONED attempt is part of the feed, like any other.
	if all := f.attemptFeed(t, f.orgID, delivery.AttemptFilter{}); len(all.Items) != 8 {
		t.Fatalf("every attempt, ABANDONED included, is in the feed: %d", len(all.Items))
	}
	for _, c := range []struct {
		name   string
		filter delivery.AttemptFilter
		want   []string
	}{
		{"abandoned", delivery.AttemptFilter{Outcome: "ABANDONED"}, []string{ids[7], ids[0]}},
		{"http error", delivery.AttemptFilter{Outcome: "HTTP_ERROR"}, []string{ids[3], ids[2]}},
		{"blocked", delivery.AttemptFilter{Outcome: "BLOCKED_BY_POLICY"}, []string{ids[6]}},
		{"timeout (none)", delivery.AttemptFilter{Outcome: "TIMEOUT"}, nil},
		{"one delivery", delivery.AttemptFilter{DeliveryID: b}, []string{ids[7], ids[6], ids[5], ids[4]}},
		{"delivery and outcome", delivery.AttemptFilter{DeliveryID: a, Outcome: "ABANDONED"}, []string{ids[0]}},
		{"delivery with no such outcome", delivery.AttemptFilter{DeliveryID: b, Outcome: "NETWORK_ERROR"}, nil},
	} {
		got := f.attemptFeed(t, f.orgID, c.filter)
		if strings.Join(attemptIDs(got.Items), ",") != strings.Join(c.want, ",") {
			t.Errorf("%s: got %v, want %v", c.name, attemptIDs(got.Items), c.want)
		}
	}
	// Only the terminal attempt carries a terminal reason, and a status-less outcome omits the status.
	var terminal delivery.AttemptActivityItem
	for _, it := range f.attemptFeed(t, f.orgID, delivery.AttemptFilter{DeliveryID: a}).Items {
		if it.AttemptNumber == 4 {
			terminal = it
		} else if it.TerminalReason != nil {
			t.Errorf("attempt %d is not terminal but has a reason %q", it.AttemptNumber, *it.TerminalReason)
		}
	}
	if terminal.TerminalReason == nil || *terminal.TerminalReason != "RETRIES_EXHAUSTED" || terminal.HTTPStatus == nil || *terminal.HTTPStatus != 404 {
		t.Errorf("the terminal attempt: %+v", terminal)
	}
	if terminal.DestinationID != f.dest1 || terminal.DestinationName != "Primary" || terminal.DeliveryID != a || terminal.EventID == "" || terminal.DurationMS != 120 {
		t.Errorf("the item identifies its delivery, event and destination: %+v", terminal)
	}
	if !terminal.FinishedAt.Equal(terminal.StartedAt.Add(120 * time.Millisecond)) {
		t.Errorf("finishedAt %v", terminal.FinishedAt)
	}
}

func TestAttemptFeedNeverReturnsAnotherTenantsAttempts(t *testing.T) {
	f := newDeliveryFixture(t)
	mine := f.opDelivery(t, opRow{status: "SUCCEEDED", completed: attemptEpoch, created: attemptEpoch.Add(-time.Hour)})
	theirs := f.opDelivery(t, opRow{org: f.org2, status: "SUCCEEDED", completed: attemptEpoch, created: attemptEpoch.Add(-time.Hour)})
	myIDs := f.addAttempts(t, attemptSpec{delivery: mine, number: 1, started: attemptEpoch, outcome: "SUCCESS", http: 200})
	theirIDs := f.addAttempts(t,
		attemptSpec{org: f.org2, delivery: theirs, number: 1, started: attemptEpoch.Add(time.Minute), outcome: "SUCCESS", http: 200},
		attemptSpec{org: f.org2, delivery: theirs, number: 2, started: attemptEpoch.Add(2 * time.Minute), outcome: "ABANDONED"})

	if got := f.attemptFeed(t, f.orgID, delivery.AttemptFilter{}); strings.Join(attemptIDs(got.Items), ",") != myIDs[0] {
		t.Fatalf("organization A sees %v, want only its own attempt %v", attemptIDs(got.Items), myIDs)
	}
	if got := f.attemptFeed(t, f.org2, delivery.AttemptFilter{}); len(got.Items) != 2 {
		t.Fatalf("organization B sees %v, want its two attempts %v", attemptIDs(got.Items), theirIDs)
	}
	// Another tenant's delivery id as a filter returns nothing, not an error and not their rows.
	if got := f.attemptFeed(t, f.orgID, delivery.AttemptFilter{DeliveryID: theirs}); len(got.Items) != 0 {
		t.Errorf("a delivery id of another tenant matched %d attempts", len(got.Items))
	}
	// A cursor taken from the other tenant only moves a position inside the caller's own data.
	other := f.attemptFeed(t, f.org2, delivery.AttemptFilter{Limit: 1})
	if other.NextCursor == nil {
		t.Fatal("setup: organization B has two attempts, so a one-row page has a cursor")
	}
	c, err := delivery.ParseAttemptCursor(*other.NextCursor)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range f.attemptFeed(t, f.orgID, delivery.AttemptFilter{After: &c}).Items {
		if it.AttemptID != myIDs[0] {
			t.Errorf("a replayed cursor of another tenant exposed %s (own attempt is %s)", it.AttemptID, myIDs[0])
		}
	}
	// The same cursor in the other direction: organization B continues inside its own data only.
	mineCursor := f.attemptFeed(t, f.orgID, delivery.AttemptFilter{Limit: 1})
	if mineCursor.NextCursor != nil {
		t.Fatal("setup: organization A has exactly one attempt")
	}
	cursorFromA := delivery.AttemptCursor{StartedAt: attemptEpoch.Add(time.Hour), ID: myIDs[0]}
	for _, it := range f.attemptFeed(t, f.org2, delivery.AttemptFilter{After: &cursorFromA}).Items {
		found := false
		for _, id := range theirIDs {
			found = found || id == it.AttemptID
		}
		if !found {
			t.Errorf("organization B received a foreign attempt %s", it.AttemptID)
		}
	}
}

func TestAttemptFeedExposesOperationalMetadataOnly(t *testing.T) {
	f := newDeliveryFixture(t)
	d := f.opDelivery(t, opRow{status: "FAILED", reason: "DESTINATION_REJECTED", completed: attemptEpoch, created: attemptEpoch.Add(-time.Hour)})
	f.addAttempts(t, attemptSpec{delivery: d, number: 1, started: attemptEpoch, outcome: "HTTP_ERROR", http: 400, class: "http_4xx", reason: "DESTINATION_REJECTED"})
	raw, err := json.Marshal(f.attemptFeed(t, f.orgID, delivery.AttemptFilter{}))
	if err != nil {
		t.Fatal(err)
	}
	var page struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(raw, &page); err != nil || len(page.Items) != 1 {
		t.Fatalf("%v: %s", err, raw)
	}
	allowed := map[string]bool{"attemptId": true, "deliveryId": true, "eventId": true, "destinationId": true, "destinationName": true, "attemptNumber": true,
		"startedAt": true, "finishedAt": true, "durationMs": true, "outcome": true, "httpStatus": true, "errorClass": true, "terminalReason": true}
	for k := range page.Items[0] {
		if !allowed[k] {
			t.Errorf("the item has an unapproved field %q", k)
		}
	}
	for _, banned := range []string{"url", "payload", "secret", "token", "worker", "lease", "signing", "https://", "claim"} {
		if strings.Contains(strings.ToLower(string(raw)), banned) {
			t.Errorf("the response mentions %q: %s", banned, raw)
		}
	}
}

// The statement must be served by delivery_attempts_history_idx, in its own order, with work bounded by the page and not by
// the table or the window. The data is skewed: one large tenant and one small one, and a common and a rare outcome.
func TestAttemptFeedPlansAtScale(t *testing.T) {
	if testing.Short() {
		t.Skip("seeds about 250,000 rows")
	}
	f := newDeliveryFixture(t)
	ctx := context.Background()
	q, _ := f.env.Pool.Querier()
	const big, small = 200_000, 20_000
	orgB, epB, destB := f.extraOrg(t, "small-attempts")
	f.attemptDataset(t, f.orgID, f.ep1, f.dest1, big)
	f.attemptDataset(t, orgB, epB, destB, small)
	f.env.Exec(t, "ANALYZE deliveries")
	f.env.Exec(t, "ANALYZE delivery_attempts")

	now := time.Now().UTC().Truncate(time.Microsecond)
	win := func(days int) (time.Time, time.Time) {
		return now.Add(-time.Duration(days) * 24 * time.Hour), now.Add(time.Minute)
	}
	var someDelivery string
	if err := q.QueryRow(ctx, "SELECT delivery_id::text FROM delivery_attempts WHERE organization_id = $1 LIMIT 1", f.orgID).Scan(&someDelivery); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		org    string
		days   int
		filter delivery.AttemptFilter
		// max is the bound on rows read and then discarded: it is what an index-ordered scan keeps near the page size.
		max int
	}{
		{"large tenant, 24h, first page", f.orgID, 1, delivery.AttemptFilter{Limit: 50}, 0},
		{"large tenant, 31 days, first page", f.orgID, 31, delivery.AttemptFilter{Limit: 100}, 0},
		{"large tenant, common outcome", f.orgID, 31, delivery.AttemptFilter{Limit: 50, Outcome: "SUCCESS"}, 0},
		{"large tenant, a single delivery", f.orgID, 31, delivery.AttemptFilter{Limit: 50, DeliveryID: someDelivery}, 0},
		{"small tenant, 31 days", orgB, 31, delivery.AttemptFilter{Limit: 50}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.filter.From, c.filter.To = win(c.days)
			plan := f.explainCaptured(t, explainAnalyze, func(db database.Querier) {
				if _, err := delivery.AttemptFeedOf(ctx, db, c.org, c.filter, now); err != nil {
					t.Fatal(err)
				}
			})
			t.Logf("filtered rows: %d\n%s", filteredRows(plan), plan)
			if strings.Contains(plan, "Seq Scan on delivery_attempts") || strings.Contains(plan, "Seq Scan on deliveries") {
				t.Errorf("the attempt feed must not scan a whole table")
			}
			if c.filter.DeliveryID == "" && !strings.Contains(plan, "delivery_attempts_history_idx") {
				t.Errorf("the feed must be served by delivery_attempts_history_idx")
			}
			if filteredRows(plan) > maxFeedFilteredRows {
				t.Errorf("the feed filtered out %d rows to return one page (limit %d)", filteredRows(plan), maxFeedFilteredRows)
			}
		})
	}

	// A deep page is as cheap as the first: the keyset position, not an offset, skips what was already read.
	t.Run("a deep page costs the same as the first", func(t *testing.T) {
		from, to := win(31)
		filter := delivery.AttemptFilter{Limit: 100, From: from, To: to}
		page, err := delivery.AttemptFeedOf(ctx, q, f.orgID, filter, now)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 400 && page.NextCursor != nil; i++ {
			c, _ := delivery.ParseAttemptCursor(*page.NextCursor)
			filter.After = &c
			if page, err = delivery.AttemptFeedOf(ctx, q, f.orgID, filter, now); err != nil {
				t.Fatal(err)
			}
		}
		if filter.After == nil {
			t.Fatal("setup: the dataset must have more than one page")
		}
		plan := f.explainCaptured(t, explainAnalyze, func(db database.Querier) {
			if _, err := delivery.AttemptFeedOf(ctx, db, f.orgID, filter, now); err != nil {
				t.Fatal(err)
			}
		})
		t.Logf("deep page, filtered rows: %d\n%s", filteredRows(plan), plan)
		if strings.Contains(plan, "Seq Scan on delivery_attempts") || filteredRows(plan) > maxFeedFilteredRows {
			t.Errorf("a deep page must stay index-ordered and bounded")
		}
	})
}

// attemptDataset seeds one tenant with n deliveries, each with one finished attempt started at a random instant of the last
// 30 days. The outcomes are skewed: about 96% SUCCESS, 3.9% HTTP_ERROR and 0.1% ABANDONED.
func (f *deliveryFixture) attemptDataset(t *testing.T, org, ep, dest string, n int) {
	t.Helper()
	f.bulkTo(t, org, ep, dest, n, "status, attempt_count, last_http_status, completed_at, next_attempt_at",
		"'SUCCEEDED', 1, 200, now() - interval '1 day', now()", "now() - interval '31 days'")
	f.seedAttempts(t, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO delivery_attempts (organization_id, delivery_id, attempt_number, started_at, finished_at, outcome, http_status, duration_ms)
			SELECT organization_id, id, 1, s, s + interval '50 milliseconds',
			       CASE WHEN r < 0.001 THEN 'ABANDONED' WHEN r < 0.04 THEN 'HTTP_ERROR' ELSE 'SUCCESS' END,
			       CASE WHEN r < 0.001 THEN NULL WHEN r < 0.04 THEN 503 ELSE 200 END, 50
			  FROM (SELECT organization_id, id, now() - random() * interval '30 days' AS s, random() AS r FROM deliveries WHERE organization_id = $1) x`, org)
		return err
	})
}
