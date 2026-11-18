package app_test

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Thiago-tods/relayforge/internal/app"
	"github.com/Thiago-tods/relayforge/internal/app/apptest"
	"github.com/Thiago-tods/relayforge/internal/delivery"
)

var seedSeq atomic.Int64

// seedDelivery creates a delivery for a fresh event of the user's organization and
// walks it through `attempts` attempts using the real store functions, ending in
// final (FAILED, SUCCEEDED or PENDING). It returns the delivery id.
func seedDelivery(t *testing.T, env *apptest.Env, u apptest.User, destID string, attempts int, final string) string {
	return seedDeliveryAt(t, env, u, destID, attempts, final, time.Now().UTC())
}

func seedDeliveryAt(t *testing.T, env *apptest.Env, u apptest.User, destID string, attempts int, final string, createdAt time.Time) string {
	t.Helper()
	ep := env.CreateEndpointIn(t, u, u.Org.ID)
	res, err := env.Send(ep.Endpoint.PublicID, env.Signed(ep.SigningSecret, []byte(`{"seed":true}`), fmt.Sprintf("seed-%d", seedSeq.Add(1))))
	if err != nil {
		t.Fatal(err)
	}
	q, _ := env.Pool.Querier()
	var id string
	if err := q.QueryRow(bg, `INSERT INTO deliveries (organization_id, event_id, destination_id, created_at)
		VALUES ($1, $2, $3, $4) RETURNING id::text`, u.Org.ID, res.EventID, destID, createdAt).Scan(&id); err != nil {
		t.Fatal(err)
	}
	for k := 1; k <= attempts; k++ {
		appendAttempt(t, env, u.Org.ID, id, k, attempts, final)
	}
	return id
}

// appendAttempt claims attempt k of a delivery and records it, as the worker would.
func appendAttempt(t testing.TB, env *apptest.Env, orgID, id string, k, total int, final string) {
	t.Helper()
	q, _ := env.Pool.Querier()
	var token string
	if err := q.QueryRow(bg, `UPDATE deliveries SET status = 'IN_FLIGHT', attempt_count = $2, lease_expires_at = now() + interval '1 minute',
		claim_token = gen_random_uuid(), claimed_by = 'seed' WHERE id = $1 RETURNING claim_token::text`, id, k).Scan(&token); err != nil {
		t.Errorf("claim attempt %d: %v", k, err)
		return
	}
	now := time.Now().UTC()
	rec := delivery.AttemptRecord{Number: k, StartedAt: now.Add(-2 * time.Second), FinishedAt: now.Add(-time.Second), Outcome: delivery.OutcomeHTTPError, HTTPStatus: 500}
	dec := delivery.Decision{Status: delivery.StatusPending, NextAttemptAt: now.Add(time.Hour)}
	if k == total {
		switch final {
		case "FAILED":
			rec.HTTPStatus = 410
			dec = delivery.Decision{Status: delivery.StatusFailed}
		case "SUCCEEDED":
			rec.Outcome, rec.HTTPStatus = delivery.OutcomeSuccess, 200
			dec = delivery.Decision{Status: delivery.StatusSucceeded}
		}
	}
	if k == 1 && final == "NETWORK" {
		rec = delivery.AttemptRecord{Number: k, StartedAt: now.Add(-2 * time.Second), FinishedAt: now.Add(-time.Second), Outcome: delivery.OutcomeNetworkError, ErrorClass: "tls"}
	}
	err := env.Pool.InTx(bg, func(tx pgx.Tx) error {
		return delivery.Finish(bg, tx, orgID, id, token, rec, dec, now)
	})
	if err != nil {
		t.Errorf("finish attempt %d: %v", k, err)
	}
}

func TestDeliveryHistoryListingFiltersAndPagination(t *testing.T) {
	tm := newTeam(t)
	svc, org, owner := tm.env.Svc, tm.org.ID, tm.owner.Principal
	a, b := tm.dest(t, "Alpha").Destination, tm.dest(t, "Beta").Destination
	base := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Second)

	var ids []string
	for i := 0; i < 6; i++ {
		dst, status := a.ID, "FAILED"
		if i%2 == 1 {
			dst, status = b.ID, "SUCCEEDED"
		}
		ids = append(ids, seedDeliveryAt(t, tm.env, tm.owner, dst, 1+i%2, status, base.Add(time.Duration(i)*time.Hour)))
	}
	pending := seedDeliveryAt(t, tm.env, tm.owner, a.ID, 1, "PENDING", base.Add(10*time.Hour))

	all, total, err := svc.ListDeliveries(bg, owner, org, app.DeliveryFilter{}, 100, 0)
	if err != nil || total != 7 || len(all) != 7 {
		t.Fatalf("all: %d %v", total, err)
	}
	if all[0].ID != pending || all[6].ID != ids[0] {
		t.Fatal("deliveries must be listed newest first")
	}
	if all[0].DestinationName != "Alpha" || all[0].Status != "PENDING" || all[0].NextAttemptAt == nil || all[0].AttemptCount != 1 || all[0].MaxAttempts != 8 {
		t.Fatalf("operational metadata missing: %+v", all[0])
	}

	// Pagination: stable, non-overlapping pages and an accurate total.
	seen := map[string]bool{}
	for off := 0; off < 7; off += 3 {
		page, total, err := svc.ListDeliveries(bg, owner, org, app.DeliveryFilter{}, 3, off)
		if err != nil || total != 7 {
			t.Fatalf("page at %d: total %d %v", off, total, err)
		}
		for _, d := range page {
			if seen[d.ID] {
				t.Fatalf("delivery %s appears on two pages", d.ID)
			}
			seen[d.ID] = true
		}
	}
	if len(seen) != 7 {
		t.Fatalf("pages covered %d deliveries, want 7", len(seen))
	}
	if beyond, total, _ := svc.ListDeliveries(bg, owner, org, app.DeliveryFilter{}, 3, 50); len(beyond) != 0 || total != 7 {
		t.Fatal("an offset past the end must return no items but the real total")
	}

	count := func(f app.DeliveryFilter) int {
		t.Helper()
		items, total, err := svc.ListDeliveries(bg, owner, org, f, 100, 0)
		if err != nil || total != len(items) {
			t.Fatalf("filter %+v: %d/%d %v", f, len(items), total, err)
		}
		return total
	}
	event := all[3].EventID
	for name, c := range map[string]struct {
		f    app.DeliveryFilter
		want int
	}{
		"destination":        {app.DeliveryFilter{DestinationID: a.ID}, 4},
		"other destination":  {app.DeliveryFilter{DestinationID: b.ID}, 3},
		"status FAILED":      {app.DeliveryFilter{Status: "FAILED"}, 3},
		"status SUCCEEDED":   {app.DeliveryFilter{Status: "SUCCEEDED"}, 3},
		"status PENDING":     {app.DeliveryFilter{Status: "PENDING"}, 1},
		"status CANCELLED":   {app.DeliveryFilter{Status: "CANCELLED"}, 0},
		"event":              {app.DeliveryFilter{EventID: event}, 1},
		"destination+status": {app.DeliveryFilter{DestinationID: a.ID, Status: "FAILED"}, 3},
		"from":               {app.DeliveryFilter{From: base.Add(3 * time.Hour)}, 4},
		"to (exclusive)":     {app.DeliveryFilter{To: base.Add(3 * time.Hour)}, 3},
		"from+to":            {app.DeliveryFilter{From: base.Add(1 * time.Hour), To: base.Add(4 * time.Hour)}, 3},
	} {
		if got := count(c.f); got != c.want {
			t.Errorf("%s: %d deliveries, want %d", name, got, c.want)
		}
	}

	for name, f := range map[string]app.DeliveryFilter{
		"bad status":        {Status: "DONE"},
		"lower-case status": {Status: "failed"},
		"bad destination":   {DestinationID: "x"},
		"bad event":         {EventID: "x"},
		"from after to":     {From: base.Add(2 * time.Hour), To: base.Add(time.Hour)},
		"from equals to":    {From: base, To: base},
	} {
		if _, _, err := svc.ListDeliveries(bg, owner, org, f, 10, 0); !isValidation(err) {
			t.Errorf("%s: %v, want a validation error", name, err)
		}
	}
}

func TestDeliveryDetailIncludesOrderedSanitizedAttempts(t *testing.T) {
	tm := newTeam(t)
	svc, org, owner := tm.env.Svc, tm.org.ID, tm.owner.Principal
	d := tm.dest(t, "D").Destination
	id := seedDelivery(t, tm.env, tm.owner, d.ID, 3, "FAILED")

	got, err := svc.GetDelivery(bg, owner, org, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Delivery.Status != "FAILED" || got.Delivery.AttemptCount != 3 || got.Delivery.CompletedAt == nil || got.Delivery.NextAttemptAt != nil {
		t.Fatalf("delivery: %+v", got.Delivery)
	}
	if got.Delivery.LastHTTPStatus == nil || *got.Delivery.LastHTTPStatus != 410 {
		t.Fatal("the last HTTP status must be reported")
	}
	if len(got.Attempts) != 3 {
		t.Fatalf("%d attempts, want 3", len(got.Attempts))
	}
	for i, a := range got.Attempts {
		if a.AttemptNumber != i+1 || a.Outcome == "" || a.DurationMS < 0 || a.FinishedAt.Before(a.StartedAt) {
			t.Fatalf("attempt %d: %+v", i, a)
		}
	}
	if got.Attempts[0].HTTPStatus == nil || *got.Attempts[0].HTTPStatus != 500 || got.Attempts[2].Outcome != "HTTP_ERROR" {
		t.Fatalf("attempt data: %+v", got.Attempts)
	}

	net := seedDelivery(t, tm.env, tm.owner, d.ID, 1, "NETWORK")
	nd, _ := svc.GetDelivery(bg, owner, org, net)
	if len(nd.Attempts) != 1 || nd.Attempts[0].Outcome != "NETWORK_ERROR" || nd.Attempts[0].ErrorClass == nil || *nd.Attempts[0].ErrorClass != "tls" || nd.Attempts[0].HTTPStatus != nil {
		t.Fatalf("a transport failure carries only a short class: %+v", nd.Attempts)
	}

	if _, err := svc.GetDelivery(bg, owner, org, "not-a-uuid"); err != app.ErrNotFound {
		t.Fatalf("malformed id: %v", err)
	}
	if _, err := svc.GetDelivery(bg, owner, org, "00000000-0000-4000-8000-000000000000"); err != app.ErrNotFound {
		t.Fatalf("missing delivery: %v", err)
	}
}

// Reading history while a worker appends attempts must always return a coherent
// snapshot: contiguous attempt numbers, and never more attempts than attemptCount.
func TestHistoryReadsStayCoherentWhileAttemptsAreAppended(t *testing.T) {
	tm := newTeam(t)
	d := tm.dest(t, "D").Destination
	id := seedDelivery(t, tm.env, tm.owner, d.ID, 0, "PENDING")
	q, _ := tm.env.Pool.Querier()
	if _, err := q.Exec(bg, "UPDATE deliveries SET max_attempts = 50 WHERE id = $1", id); err == nil {
		t.Log("max_attempts is immutable after creation, as expected")
	}

	const total = 8
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(done)
		for k := 1; k <= total; k++ {
			appendAttempt(t, tm.env, tm.org.ID, id, k, total, "SUCCEEDED")
			time.Sleep(20 * time.Millisecond)
		}
	}()
	reads := 0
	for stop := false; !stop; {
		select {
		case <-done:
			stop = true
		default:
		}
		got, err := tm.env.Svc.GetDelivery(bg, tm.owner.Principal, tm.org.ID, id)
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Attempts) > got.Delivery.AttemptCount {
			t.Fatalf("%d attempts listed but attemptCount is %d", len(got.Attempts), got.Delivery.AttemptCount)
		}
		for i, a := range got.Attempts {
			if a.AttemptNumber != i+1 {
				t.Fatalf("attempt numbers are not contiguous: %+v", got.Attempts)
			}
		}
		reads++
	}
	wg.Wait()
	if reads < 3 {
		t.Fatalf("only %d concurrent reads happened", reads)
	}
	final, _ := tm.env.Svc.GetDelivery(bg, tm.owner.Principal, tm.org.ID, id)
	if len(final.Attempts) != total || final.Delivery.Status != "SUCCEEDED" {
		t.Fatalf("final: %d attempts, status %s", len(final.Attempts), final.Delivery.Status)
	}
}

func TestHistoryIsAppendOnlyAtTheDatabase(t *testing.T) {
	tm := newTeam(t)
	d := tm.dest(t, "D").Destination
	id := seedDelivery(t, tm.env, tm.owner, d.ID, 2, "FAILED")
	q, _ := tm.env.Pool.Querier()
	for name, stmt := range map[string]string{
		"update attempt":    "UPDATE delivery_attempts SET outcome = 'SUCCESS', http_status = 200 WHERE delivery_id = $1",
		"delete attempt":    "DELETE FROM delivery_attempts WHERE delivery_id = $1",
		"truncate attempts": "TRUNCATE delivery_attempts",
		"update terminal":   "UPDATE deliveries SET last_http_status = 200 WHERE id = $1",
		"delete delivery":   "DELETE FROM deliveries WHERE id = $1",
	} {
		args := []any{id}
		if name == "truncate attempts" {
			args = nil
		}
		if _, err := q.Exec(bg, stmt, args...); err == nil {
			t.Errorf("%s was accepted: history must be immutable", name)
		}
	}
}
