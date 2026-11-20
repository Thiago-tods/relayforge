package database_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Thiago-tods/relayforge/internal/delivery"
)

// at returns a fixed instant on a microsecond grid, so completion times compare exactly.
var feedEpoch = time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

func (f *deliveryFixture) feed(t *testing.T, org string, filter delivery.FailureFilter) delivery.FailureFeed {
	t.Helper()
	q, _ := f.env.Pool.Querier()
	if filter.Limit == 0 {
		filter.Limit = 100
	}
	if filter.From.IsZero() {
		filter.From = feedEpoch.Add(-24 * time.Hour)
	}
	if filter.To.IsZero() {
		filter.To = feedEpoch.Add(24 * time.Hour)
	}
	out, err := delivery.FailureFeedOf(context.Background(), q, org, filter, feedEpoch)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func (f *deliveryFixture) failed(t *testing.T, completed time.Time, reason string) string {
	t.Helper()
	return f.opDelivery(t, opRow{status: "FAILED", reason: reason, completed: completed, created: completed.Add(-time.Hour)})
}

func ids(items []delivery.FailureItem) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.DeliveryID
	}
	return out
}

func TestRecentFailureFeedOrdersByCompletionThenIDAndPagesWithoutGapsOrDuplicates(t *testing.T) {
	f := newDeliveryFixture(t)
	// 23 failures that share only three completion instants, so most of the order is decided by the id.
	var created []string
	for i := 0; i < 23; i++ {
		created = append(created, f.failed(t, feedEpoch.Add(time.Duration(i%3)*time.Minute), "RETRIES_EXHAUSTED"))
	}
	want := make([]delivery.FailureItem, 0, len(created))
	all := f.feed(t, f.orgID, delivery.FailureFilter{})
	want = append(want, all.Items...)
	if len(want) != 23 || all.NextCursor != nil {
		t.Fatalf("one page of 100 holds all 23 failures and has no cursor: %d, %v", len(want), all.NextCursor)
	}
	if !sort.SliceIsSorted(want, func(i, j int) bool {
		if !want[i].CompletedAt.Equal(want[j].CompletedAt) {
			return want[i].CompletedAt.After(want[j].CompletedAt)
		}
		return want[i].DeliveryID > want[j].DeliveryID
	}) {
		t.Fatalf("the feed must be ordered by completed_at DESC, id DESC: %v", ids(want))
	}
	q, _ := f.env.Pool.Querier()
	for _, size := range []int{1, 2, 4, 7, 22, 23, 24} {
		got := pageThrough(t, q, f.orgID, delivery.FailureFilter{
			From: feedEpoch.Add(-24 * time.Hour), To: feedEpoch.Add(24 * time.Hour), Limit: size,
		})
		if strings.Join(ids(got), ",") != strings.Join(ids(want), ",") {
			t.Errorf("page size %d: the pages do not add up to the whole feed:\n got %v\nwant %v", size, ids(got), ids(want))
		}
	}
	// The cursor of a full page is present exactly when more rows exist.
	if p := f.feed(t, f.orgID, delivery.FailureFilter{Limit: 23}); p.NextCursor != nil || len(p.Items) != 23 {
		t.Errorf("a page that ends exactly at the last row has no next cursor: %d, %v", len(p.Items), p.NextCursor)
	}
	if p := f.feed(t, f.orgID, delivery.FailureFilter{Limit: 22}); p.NextCursor == nil || len(p.Items) != 22 {
		t.Errorf("a page with one row left has a next cursor: %d, %v", len(p.Items), p.NextCursor)
	}
}

func TestRecentFailureFeedOnlyListsCurrentFailuresAndOnlyOperationalMetadata(t *testing.T) {
	f := newDeliveryFixture(t)
	failed := f.failed(t, feedEpoch, "DESTINATION_REJECTED")
	f.opDelivery(t, opRow{status: "SUCCEEDED", completed: feedEpoch})
	f.opDelivery(t, opRow{status: "CANCELLED", completed: feedEpoch})
	f.opDelivery(t, opRow{status: "PENDING", next: feedEpoch})
	f.opDelivery(t, opRow{status: "IN_FLIGHT", lease: feedEpoch.Add(time.Hour)})

	got := f.feed(t, f.orgID, delivery.FailureFilter{})
	if len(got.Items) != 1 || got.Items[0].DeliveryID != failed {
		t.Fatalf("only the FAILED delivery is a failure: %+v", got.Items)
	}
	it := got.Items[0]
	if it.FailureReason != "DESTINATION_REJECTED" || it.AttemptCount != 1 || it.LastHTTPStatus == nil || *it.LastHTTPStatus != 500 ||
		it.DestinationID != f.dest1 || it.DestinationName != "Primary" || !it.CompletedAt.Equal(feedEpoch) || it.CompletedAt.Location() != time.UTC {
		t.Fatalf("item: %+v", it)
	}
	raw, _ := json.Marshal(got)
	var top map[string]json.RawMessage
	_ = json.Unmarshal(raw, &top)
	var items []map[string]json.RawMessage
	_ = json.Unmarshal(top["items"], &items)
	keys := make([]string, 0)
	for k := range items[0] {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if strings.Join(keys, ",") != "attemptCount,completedAt,createdAt,deliveryId,destinationId,destinationName,eventId,failureReason,lastHttpStatus" {
		t.Errorf("item fields: %v", keys)
	}
	if string(top["nextCursor"]) != "null" {
		t.Errorf("nextCursor is null on the last page: %s", top["nextCursor"])
	}
	for _, banned := range []string{"url", "payload", "secret", "claim", "worker", "lease", "https://"} {
		if strings.Contains(strings.ToLower(string(raw)), banned) {
			t.Errorf("the feed exposes %q: %s", banned, raw)
		}
	}
}

func TestRecentFailureFeedReasonFilterAndLegacyRows(t *testing.T) {
	f := newDeliveryFixture(t)
	reasons := []string{"DESTINATION_REJECTED", "REDIRECT_REFUSED", "EGRESS_BLOCKED", "RETRIES_EXHAUSTED", "INTERNAL_ERROR"}
	byReason := map[string]string{}
	for i, r := range reasons {
		byReason[r] = f.failed(t, feedEpoch.Add(time.Duration(i)*time.Second), r)
	}
	legacy := f.legacyFailed(t, opRow{completed: feedEpoch.Add(10 * time.Second), created: feedEpoch.Add(-time.Hour)})

	all := f.feed(t, f.orgID, delivery.FailureFilter{})
	if len(all.Items) != 6 || all.Items[0].DeliveryID != legacy || all.Items[0].FailureReason != "UNRECORDED" {
		t.Fatalf("a legacy row (NULL reason) is rendered as UNRECORDED: %+v", all.Items)
	}
	for _, r := range reasons {
		got := f.feed(t, f.orgID, delivery.FailureFilter{Reason: r})
		if len(got.Items) != 1 || got.Items[0].DeliveryID != byReason[r] || got.Items[0].FailureReason != r {
			t.Errorf("%s: %+v", r, got.Items)
		}
	}
	got := f.feed(t, f.orgID, delivery.FailureFilter{Reason: "UNRECORDED"})
	if len(got.Items) != 1 || got.Items[0].DeliveryID != legacy {
		t.Fatalf("UNRECORDED selects FAILED deliveries without a reason, and only them: %+v", got.Items)
	}
}

func TestRecentFailureFeedWindowIsHalfOpenAtMicroseconds(t *testing.T) {
	f := newDeliveryFixture(t)
	from, to := feedEpoch, feedEpoch.Add(time.Hour)
	before := f.failed(t, from.Add(-time.Microsecond), "RETRIES_EXHAUSTED")
	atFrom := f.failed(t, from, "RETRIES_EXHAUSTED")
	inside := f.failed(t, to.Add(-time.Microsecond), "RETRIES_EXHAUSTED")
	atTo := f.failed(t, to, "RETRIES_EXHAUSTED")
	got := f.feed(t, f.orgID, delivery.FailureFilter{From: from, To: to})
	if strings.Join(ids(got.Items), ",") != inside+","+atFrom && strings.Join(ids(got.Items), ",") != atFrom+","+inside {
		t.Fatalf("[from, to): got %v, want %s and %s (not %s, %s)", ids(got.Items), atFrom, inside, before, atTo)
	}
	if len(got.Items) != 2 || !got.From.Equal(from) || !got.To.Equal(to) {
		t.Fatalf("%+v", got)
	}
}

func TestRecentFailureFeedIsolatesOrganizationsAndDestinationsAndIgnoresForeignCursors(t *testing.T) {
	f := newDeliveryFixture(t)
	orgB, destB := f.org2, f.dest2
	mine := []string{f.failed(t, feedEpoch.Add(3*time.Minute), "RETRIES_EXHAUSTED"), f.failed(t, feedEpoch.Add(time.Minute), "RETRIES_EXHAUSTED")}
	theirs := f.opDelivery(t, opRow{org: orgB, status: "FAILED", reason: "RETRIES_EXHAUSTED", completed: feedEpoch.Add(2 * time.Minute)})

	if got := f.feed(t, f.orgID, delivery.FailureFilter{}); strings.Join(ids(got.Items), ",") != strings.Join(mine, ",") {
		t.Fatalf("organization A sees only its own failures: %v", ids(got.Items))
	}
	if got := f.feed(t, orgB, delivery.FailureFilter{}); len(got.Items) != 1 || got.Items[0].DeliveryID != theirs {
		t.Fatalf("organization B sees only its own failure: %v", ids(got.Items))
	}
	// A destination of another organization selects nothing, whoever asks.
	if got := f.feed(t, f.orgID, delivery.FailureFilter{DestinationID: destB}); len(got.Items) != 0 {
		t.Fatalf("a foreign destination id must select nothing: %v", ids(got.Items))
	}
	if got := f.feed(t, orgB, delivery.FailureFilter{DestinationID: f.dest1}); len(got.Items) != 0 {
		t.Fatalf("a foreign destination id must select nothing: %v", ids(got.Items))
	}
	if got := f.feed(t, f.orgID, delivery.FailureFilter{DestinationID: f.dest1}); len(got.Items) != 2 {
		t.Fatalf("an own destination filters normally: %v", ids(got.Items))
	}
	// A cursor issued for A's data, replayed against B, only positions B's own rows.
	first := f.feed(t, f.orgID, delivery.FailureFilter{Limit: 1})
	cur, err := delivery.ParseFailureCursor(*first.NextCursor)
	if err != nil {
		t.Fatal(err)
	}
	got := f.feed(t, orgB, delivery.FailureFilter{After: &cur})
	for _, it := range got.Items {
		if it.DeliveryID != theirs || it.DestinationID != destB {
			t.Fatalf("a replayed cursor leaked %+v", it)
		}
	}
	// And a cursor naming one of A's rows, but sent for B, cannot read that row.
	forged := delivery.FailureCursor{CompletedAt: feedEpoch.Add(time.Hour), ID: mine[0]}
	for _, it := range f.feed(t, orgB, delivery.FailureFilter{After: &forged}).Items {
		if it.DeliveryID == mine[0] || it.DeliveryID == mine[1] {
			t.Fatalf("a forged cursor reached another organization's row: %+v", it)
		}
	}
}

func TestRecentFailureFeedDropsARedrivenDeliveryAndShowsItAgainIfItFailsAgain(t *testing.T) {
	f := newDeliveryFixture(t)
	ctx := context.Background()
	keep := f.failed(t, feedEpoch, "RETRIES_EXHAUSTED")
	redriven := f.failed(t, feedEpoch.Add(time.Minute), "RETRIES_EXHAUSTED")
	if n := len(f.feed(t, f.orgID, delivery.FailureFilter{}).Items); n != 2 {
		t.Fatalf("setup: %d failures", n)
	}
	// Redrive is not implemented; simulate its row-level effect the way ADR 30 documents it: status, reason and
	// completion time are cleared together, with the transition trigger disabled inside a transaction that
	// is rolled back at the end, so nothing persists.
	err := f.env.Pool.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "ALTER TABLE deliveries DISABLE TRIGGER deliveries_enforce_transition"); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "UPDATE deliveries SET status = 'PENDING', failure_reason = NULL, completed_at = NULL, next_attempt_at = $2 WHERE id = $1", redriven, feedEpoch); err != nil {
			return err
		}
		got, err := delivery.FailureFeedOf(ctx, tx, f.orgID, delivery.FailureFilter{From: feedEpoch.Add(-time.Hour), To: feedEpoch.Add(time.Hour), Limit: 100}, feedEpoch)
		if err != nil {
			return err
		}
		if len(got.Items) != 1 || got.Items[0].DeliveryID != keep {
			t.Errorf("a redriven delivery is not a current failure: %v", ids(got.Items))
		}
		// It fails again, later: it is a failure again, with its new completion time.
		if _, err := tx.Exec(ctx, "UPDATE deliveries SET status = 'FAILED', failure_reason = 'INTERNAL_ERROR', completed_at = $2, next_attempt_at = $2 WHERE id = $1", redriven, feedEpoch.Add(30*time.Minute)); err != nil {
			return err
		}
		got, err = delivery.FailureFeedOf(ctx, tx, f.orgID, delivery.FailureFilter{From: feedEpoch.Add(-time.Hour), To: feedEpoch.Add(time.Hour), Limit: 100}, feedEpoch)
		if err != nil {
			return err
		}
		if len(got.Items) != 2 || got.Items[0].DeliveryID != redriven || got.Items[0].FailureReason != "INTERNAL_ERROR" {
			t.Errorf("a redriven delivery that failed again: %+v", got.Items)
		}
		return errRollback
	})
	if err != errRollback {
		t.Fatalf("the simulation must roll back: %v", err)
	}
	if f.env.Count(t, "SELECT count(*) FROM pg_trigger WHERE tgname = 'deliveries_enforce_transition' AND tgenabled = 'O'") != 1 {
		t.Fatal("the transition trigger must be enabled again")
	}
}

var errRollback = fmt.Errorf("rollback the simulation")

// TestRecentFailureFeedLateCommitCaveat documents the eventual consistency of the feed (ADR 32): completed_at is the
// worker's clock, so a delivery can commit with a completed_at older than rows a client already read. A
// continuation never revisits that part of the order; starting again from the first page shows it.
func TestRecentFailureFeedLateCommitCaveat(t *testing.T) {
	f := newDeliveryFixture(t)
	a := f.failed(t, feedEpoch.Add(5*time.Minute), "RETRIES_EXHAUSTED")
	b := f.failed(t, feedEpoch.Add(4*time.Minute), "RETRIES_EXHAUSTED")
	c := f.failed(t, feedEpoch.Add(1*time.Minute), "RETRIES_EXHAUSTED")

	page1 := f.feed(t, f.orgID, delivery.FailureFilter{Limit: 2})
	if strings.Join(ids(page1.Items), ",") != a+","+b {
		t.Fatalf("page 1: %v", ids(page1.Items))
	}
	cur, _ := delivery.ParseFailureCursor(*page1.NextCursor)

	// A delivery that finished between a and b commits only now, with its own (older) completion time.
	late := f.failed(t, feedEpoch.Add(4*time.Minute+30*time.Second), "RETRIES_EXHAUSTED")

	page2 := f.feed(t, f.orgID, delivery.FailureFilter{Limit: 2, After: &cur})
	if strings.Join(ids(page2.Items), ",") != c {
		t.Fatalf("the continuation does not revisit what is already behind the cursor: %v (late commit %s)", ids(page2.Items), late)
	}
	restart := f.feed(t, f.orgID, delivery.FailureFilter{Limit: 4})
	if strings.Join(ids(restart.Items), ",") != a+","+late+","+b+","+c {
		t.Fatalf("starting again from the first page shows the late commit: %v", ids(restart.Items))
	}
}
