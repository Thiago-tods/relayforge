package database_test

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Thiago-tods/relayforge/internal/database"
	"github.com/Thiago-tods/relayforge/internal/delivery"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// captureQuerier records the last statement and arguments sent through Query or QueryRow, so a plan test
// can EXPLAIN the exact SQL the production function runs, without that function exporting it.
type captureQuerier struct {
	database.Querier
	sql  string // the last statement of either kind
	args []any
	// The last QueryRow statement (a count, for the delivery list) and the last Query statement (its page).
	rowSQL, rowsSQL   string
	rowArgs, rowsArgs []any
}

func (c *captureQuerier) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	c.sql, c.args = sql, args
	c.rowsSQL, c.rowsArgs = sql, args
	return c.Querier.Query(ctx, sql, args...)
}

func (c *captureQuerier) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	c.sql, c.args = sql, args
	c.rowSQL, c.rowArgs = sql, args
	return c.Querier.QueryRow(ctx, sql, args...)
}

func (c *captureQuerier) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return c.Querier.Exec(ctx, sql, args...)
}

// explainCaptured runs fn (which must issue exactly the production statement through the querier it is given),
// then returns EXPLAIN output of that captured statement with the captured arguments bound, as the
// application binds them.
func (f *deliveryFixture) explainCaptured(t *testing.T, explain string, fn func(q database.Querier)) string {
	t.Helper()
	return f.explainWhich(t, explain, fn, func(c *captureQuerier) (string, []any) { return c.sql, c.args })
}

// explainWhich is explainCaptured for a function that issues several statements: pick chooses which one to explain.
func (f *deliveryFixture) explainWhich(t *testing.T, explain string, fn func(q database.Querier), pick func(*captureQuerier) (string, []any)) string {
	t.Helper()
	real, _ := f.env.Pool.Querier()
	cq := &captureQuerier{Querier: real}
	fn(cq)
	sql, args := pick(cq)
	if sql == "" {
		t.Fatal("the function under test issued no such statement")
	}
	rows, err := real.Query(context.Background(), explain+" "+sql, args...)
	if err != nil {
		t.Fatalf("explain captured statement: %v\n%s", err, sql)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(lines, "\n")
}

const explainAnalyze = "EXPLAIN (ANALYZE, BUFFERS, SUMMARY ON)"

var removedByFilter = regexp.MustCompile(`Rows Removed by Filter: (\d+)`)

// filteredRows sums every "Rows Removed by Filter" of a plan (per worker figures included).
func filteredRows(plan string) int {
	total := 0
	for _, m := range removedByFilter.FindAllStringSubmatch(plan, -1) {
		n, _ := strconv.Atoi(m[1])
		total += n
	}
	return total
}

// failureDataset seeds one tenant with n completed deliveries over the last 30 days, of which `failed` FAILED
// (RETRIES_EXHAUSTED) and `legacy` FAILED without a recorded reason. Completion times are spread at random so a
// failure is equally likely anywhere in the window.
func (f *deliveryFixture) failureDataset(t *testing.T, org, ep, dest string, n, failed, legacy int) {
	t.Helper()
	const created = "now() - interval '31 days'"
	spread := "now() - random() * interval '30 days'"
	cols := "status, attempt_count, last_http_status, completed_at, next_attempt_at, failure_reason"
	f.bulkTo(t, org, ep, dest, n-failed-legacy, cols, "'SUCCEEDED', 1, 200, "+spread+", now(), NULL", created)
	if failed > 0 {
		f.bulkTo(t, org, ep, dest, failed, cols, "'FAILED', 8, 503, "+spread+", now(), 'RETRIES_EXHAUSTED'", created)
	}
	if legacy > 0 {
		f.env.Exec(t, "ALTER TABLE deliveries DROP CONSTRAINT deliveries_failed_has_reason")
		f.bulkTo(t, org, ep, dest, legacy, cols, "'FAILED', 8, 503, "+spread+", now(), NULL", created)
		f.env.Exec(t, "ALTER TABLE deliveries ADD CONSTRAINT deliveries_failed_has_reason CHECK (status <> 'FAILED' OR failure_reason IS NOT NULL) NOT VALID")
	}
}

// pageThrough reads the whole feed page by page and returns every item, failing on a duplicate.
func pageThrough(t *testing.T, q database.Querier, org string, f delivery.FailureFilter) []delivery.FailureItem {
	t.Helper()
	var all []delivery.FailureItem
	seen := map[string]bool{}
	for page := 0; ; page++ {
		got, err := delivery.FailureFeedOf(context.Background(), q, org, f, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range got.Items {
			if seen[it.DeliveryID] {
				t.Fatalf("delivery %s returned twice", it.DeliveryID)
			}
			seen[it.DeliveryID] = true
			all = append(all, it)
		}
		if got.NextCursor == nil {
			return all
		}
		c, err := delivery.ParseFailureCursor(*got.NextCursor)
		if err != nil {
			t.Fatal(err)
		}
		f.After = &c
		if page > 10000 {
			t.Fatal("the feed does not end")
		}
	}
}

// TestRecentFailureFeedPlansAtScale is the evidence the migration 00021 decision rests on (ADR 32). It seeds realistic
// tenants whose failures are rare, runs the production feed statement (captured, never copied), logs EXPLAIN
// (ANALYZE, BUFFERS) and asserts results. It never asserts a duration.
func TestRecentFailureFeedPlansAtScale(t *testing.T) {
	if testing.Short() {
		t.Skip("seeds about 600,000 rows")
	}
	f := newDeliveryFixture(t)
	ctx := context.Background()
	q, _ := f.env.Pool.Querier()
	const n = 300_000

	// Tenant A: 0.1% failures (300), 20 of them legacy (no reason). Tenant B: 0.001% failures (3).
	f.failureDataset(t, f.orgID, f.ep1, f.dest1, n, 280, 20)
	orgB, epB, destB := f.extraOrg(t, "rare-failures")
	f.failureDataset(t, orgB, epB, destB, n, 3, 0)
	f.env.Exec(t, "ANALYZE deliveries")

	now := time.Now().UTC().Truncate(time.Microsecond)
	win := func(days int) (time.Time, time.Time) {
		return now.Add(-time.Duration(days) * 24 * time.Hour), now.Add(time.Minute)
	}

	cases := []struct {
		name      string
		org       string
		filter    delivery.FailureFilter
		wantTotal int
	}{
		{"0.1% failures, 7 days, first page", f.orgID, delivery.FailureFilter{Limit: 50}, -1},
		{"0.1% failures, 31 days, first page", f.orgID, delivery.FailureFilter{Limit: 50}, 300},
		{"0.1% failures, a rare reason (legacy NULL), 31 days", f.orgID, delivery.FailureFilter{Limit: 50, Reason: delivery.UnrecordedFailureReason}, 20},
		{"0.001% failures, 7 days", orgB, delivery.FailureFilter{Limit: 50}, -1},
		{"0.001% failures, 31 days", orgB, delivery.FailureFilter{Limit: 50}, 3},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			days := 31
			if strings.Contains(c.name, "7 days") {
				days = 7
			}
			c.filter.From, c.filter.To = win(days)
			if c.wantTotal >= 0 {
				if got := pageThrough(t, q, c.org, c.filter); len(got) != c.wantTotal {
					t.Fatalf("paged through %d failures, want %d", len(got), c.wantTotal)
				}
			}
			plan := f.explainCaptured(t, explainAnalyze, func(db database.Querier) {
				if _, err := delivery.FailureFeedOf(ctx, db, c.org, c.filter, now); err != nil {
					t.Fatal(err)
				}
			})
			t.Logf("filtered rows: %d\n%s", filteredRows(plan), plan)
			if strings.Contains(plan, "Seq Scan on deliveries") {
				t.Errorf("the failure feed must not scan the whole table")
			}
			if filteredRows(plan) > maxFeedFilteredRows {
				t.Errorf("the feed filtered out %d rows to return one page (limit %d): too much scanning for an index-served read", filteredRows(plan), maxFeedFilteredRows)
			}
		})
	}

	// The evidence behind migration 00021, repeatable: without the failed index (dropped inside a transaction that is rolled
	// back) the same feed statements breach the gate that the cases above pass.
	t.Run("without the failed index the feed reads the whole window", func(t *testing.T) {
		const index = "deliveries_org_failed_idx"
		for _, c := range []struct {
			name string
			days int
		}{{"7 days", 7}, {"31 days", 31}} {
			filter := delivery.FailureFilter{Limit: 50}
			filter.From, filter.To = win(c.days)
			var plan string
			err := f.env.Pool.InTx(ctx, func(tx pgx.Tx) error {
				if _, err := tx.Exec(ctx, "DROP INDEX "+index); err != nil {
					return err
				}
				cq := &captureQuerier{Querier: tx}
				if _, err := delivery.FailureFeedOf(ctx, cq, f.orgID, filter, now); err != nil {
					return err
				}
				rows, err := tx.Query(ctx, explainAnalyze+" "+cq.sql, cq.args...)
				if err != nil {
					return err
				}
				defer rows.Close()
				var lines []string
				for rows.Next() {
					var line string
					if err := rows.Scan(&line); err != nil {
						return err
					}
					lines = append(lines, line)
				}
				plan = strings.Join(lines, "\n")
				if err := rows.Err(); err != nil {
					return err
				}
				return errRollback
			})
			if err != errRollback {
				t.Fatal(err)
			}
			t.Logf("%s, no failed index (rows removed by filter: %d):\n%s", c.name, filteredRows(plan), plan)
			if !strings.Contains(plan, "Seq Scan on deliveries") && filteredRows(plan) <= maxFeedFilteredRows {
				t.Errorf("%s: without %s the feed stays within the gate; migration 00021 would not be justified", c.name, index)
			}
		}
		if f.indexState(t, index).valid != true {
			t.Fatal("the rolled-back DROP INDEX must leave the index in place")
		}
	})

	// The filtered delivery list keeps its exact total (ADR 32, D6). Its plans are recorded as evidence; correctness is
	// what is asserted. The list is ordered by created_at, so a rare reason is served by deliveries_organization_idx
	// with a filter, not by the failed-deliveries index: the evidence below shows what that costs.
	t.Run("the filtered delivery list keeps an exact total", func(t *testing.T) {
		for _, c := range []struct {
			reason string
			total  int
		}{{"RETRIES_EXHAUSTED", 280}, {delivery.UnrecordedFailureReason, 20}, {"INTERNAL_ERROR", 0}} {
			hf := delivery.HistoryFilter{FailureReason: c.reason}
			list, total, err := delivery.ListDeliveries(ctx, q, f.orgID, hf, 50, 0)
			if err != nil {
				t.Fatal(err)
			}
			if total != c.total || len(list) != min(c.total, 50) {
				t.Fatalf("%s: total %d with %d items, want %d", c.reason, total, len(list), c.total)
			}
			for _, which := range []struct {
				name string
				pick func(*captureQuerier) (string, []any)
			}{
				{"count", func(c *captureQuerier) (string, []any) { return c.rowSQL, c.rowArgs }},
				{"page", func(c *captureQuerier) (string, []any) { return c.rowsSQL, c.rowsArgs }},
			} {
				plan := f.explainWhich(t, explainAnalyze, func(db database.Querier) {
					if _, _, err := delivery.ListDeliveries(ctx, db, f.orgID, hf, 50, 0); err != nil {
						t.Fatal(err)
					}
				}, which.pick)
				t.Logf("filtered list %s, reason %s (rows removed by filter: %d):\n%s", which.name, c.reason, filteredRows(plan), plan)
			}
		}
	})
}

// maxFeedFilteredRows is the gate of the migration decision: a first page may discard at most this many rows. It
// is the number of rows a page of 50 reads when about one delivery in 400 is a failure, so a feed served by an
// index that does not know about failures only meets it while failures are common.
const maxFeedFilteredRows = 20_000
