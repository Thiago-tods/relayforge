package delivery_test

import (
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Thiago-tods/relayforge/internal/delivery"
	"github.com/Thiago-tods/relayforge/internal/egress"
	"github.com/Thiago-tods/relayforge/internal/observability"
	"github.com/Thiago-tods/relayforge/internal/secretbox"
)

// ---- the taxonomy ----

func TestTheTaxonomyCannotBeMutatedFromOutsideThePackage(t *testing.T) {
	first := delivery.AllFailureReasons()
	want := append([]delivery.FailureReason(nil), first...)
	first[0] = "TAMPERED"
	_ = append(first[:1], "EXTRA")
	again := delivery.AllFailureReasons()
	if len(again) != len(want) {
		t.Fatalf("the taxonomy changed size: %v", again)
	}
	for i := range want {
		if again[i] != want[i] || !again[i].Valid() {
			t.Fatalf("the taxonomy was mutated: %v", again)
		}
	}
	if delivery.FailureReason("TAMPERED").Valid() || delivery.FailureReason("EXTRA").Valid() {
		t.Fatal("a caller extended the set of valid reasons")
	}
}

func TestFailureReasonTaxonomyIsClosed(t *testing.T) {
	want := []string{"DESTINATION_REJECTED", "REDIRECT_REFUSED", "EGRESS_BLOCKED", "RETRIES_EXHAUSTED", "INTERNAL_ERROR"}
	var got []string
	for _, r := range delivery.AllFailureReasons() {
		got = append(got, r.String())
		if !r.Valid() {
			t.Errorf("%s must be valid", r)
		}
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("the taxonomy changed: %v, want %v", got, want)
	}
	for _, bad := range []string{"", "retries_exhausted", "Retries_Exhausted", "RETRIES_EXHAUSTED ", "UNKNOWN_TERMINAL_FAILURE", "DESTINATION_DISABLED", "non_retryable", "exhausted"} {
		if delivery.FailureReason(bad).Valid() {
			t.Errorf("%q must not be a valid failure reason", bad)
		}
	}
}

// The seven ways a delivery can become FAILED (audit of Phase 4.4.4), each with its reason.
func TestDecideClassifiesEverySituationThatEndsInFailed(t *testing.T) {
	p := delivery.RetryPolicy{Base: time.Second, Factor: 2, Cap: time.Minute, MaxAttempts: 3}
	now := time.Unix(1_700_000_000, 0)
	cases := []struct {
		name    string
		attempt int
		res     delivery.AttemptResult
		want    delivery.FailureReason
		reason  string // Decision.Reason
	}{
		{"1 redirect", 1, delivery.AttemptResult{Outcome: delivery.OutcomeRedirect, HTTPStatus: 301}, delivery.FailureRedirectRefused, delivery.ReasonNonRetryable},
		{"2 final 4xx (404)", 1, delivery.ResultFromResponse(404, "", now), delivery.FailureDestinationRejected, delivery.ReasonNonRetryable},
		{"2 final 4xx (400)", 2, delivery.ResultFromResponse(400, "", now), delivery.FailureDestinationRejected, delivery.ReasonNonRetryable},
		{"2 final 4xx (410)", 1, delivery.ResultFromResponse(410, "", now), delivery.FailureDestinationRejected, delivery.ReasonNonRetryable},
		{"2 final 4xx (499)", 1, delivery.ResultFromResponse(499, "", now), delivery.FailureDestinationRejected, delivery.ReasonNonRetryable},
		{"3 egress: blocked address", 1, delivery.ResultFromRequestError(egress.ClassBlockedAddress).Result, delivery.FailureEgressBlocked, delivery.ReasonNonRetryable},
		{"3 egress: invalid url", 1, delivery.ResultFromRequestError(egress.ClassInvalidURL).Result, delivery.FailureEgressBlocked, delivery.ReasonNonRetryable},
		{"3 egress: invalid request", 1, delivery.ResultFromRequestError(egress.ClassInvalidRequest).Result, delivery.FailureEgressBlocked, delivery.ReasonNonRetryable},
		{"4 data missing", 1, delivery.AttemptResult{Outcome: delivery.OutcomeBlockedByPolicy, ErrorClass: delivery.ErrorClassDataMissing}, delivery.FailureInternalError, delivery.ReasonNonRetryable},
		{"5 exhausted: 503", 3, delivery.ResultFromResponse(503, "", now), delivery.FailureRetriesExhausted, delivery.ReasonExhausted},
		{"5 exhausted: 429", 3, delivery.ResultFromResponse(429, "", now), delivery.FailureRetriesExhausted, delivery.ReasonExhausted},
		{"5 exhausted: 408", 3, delivery.ResultFromResponse(408, "", now), delivery.FailureRetriesExhausted, delivery.ReasonExhausted},
		{"5 exhausted: 425", 3, delivery.ResultFromResponse(425, "", now), delivery.FailureRetriesExhausted, delivery.ReasonExhausted},
		{"5 exhausted: timeout", 3, delivery.ResultFromRequestError(egress.ClassTimeout).Result, delivery.FailureRetriesExhausted, delivery.ReasonExhausted},
		{"5 exhausted: network", 3, delivery.ResultFromRequestError(egress.ClassConnect).Result, delivery.FailureRetriesExhausted, delivery.ReasonExhausted},
		{"5 exhausted: tls", 3, delivery.ResultFromRequestError(egress.ClassTLS).Result, delivery.FailureRetriesExhausted, delivery.ReasonExhausted},
		{"6 exhausted: destination inactive", 3, delivery.AttemptResult{Outcome: delivery.OutcomeAbandoned, ErrorClass: "destination_inactive"}, delivery.FailureRetriesExhausted, delivery.ReasonExhausted},
		{"6 exhausted: secret unavailable", 3, delivery.AttemptResult{Outcome: delivery.OutcomeAbandoned, ErrorClass: "secret_unavailable"}, delivery.FailureRetriesExhausted, delivery.ReasonExhausted},
		{"6 exhausted: shutdown interrupted", 3, delivery.AttemptResult{Outcome: delivery.OutcomeAbandoned, ErrorClass: delivery.ErrorClassShutdownInterrupted}, delivery.FailureRetriesExhausted, delivery.ReasonExhausted},
		{"7 exhausted: lease expired", 3, delivery.AttemptResult{Outcome: delivery.OutcomeAbandoned, ErrorClass: "lease_expired"}, delivery.FailureRetriesExhausted, delivery.ReasonExhausted},
		{"unrecognized outcome is still classified", 1, delivery.AttemptResult{Outcome: "SOMETHING_NEW"}, delivery.FailureInternalError, delivery.ReasonNonRetryable},
	}
	covered := map[delivery.FailureReason]bool{}
	for _, c := range cases {
		d := p.Decide(c.attempt, c.res, now, nil)
		if d.Status != delivery.StatusFailed || d.FailureReason != c.want || d.Reason != c.reason || !d.FailureReason.Valid() || !d.NextAttemptAt.IsZero() {
			t.Errorf("%s: %+v, want FAILED %s (%s)", c.name, d, c.want, c.reason)
		}
		covered[d.FailureReason] = true
	}
	for _, r := range delivery.AllFailureReasons() {
		if !covered[r] {
			t.Errorf("no case produces %s", r)
		}
	}
}

// A retryable result before the budget is spent must stay retryable and carry no reason.
func TestRetryableFailuresBeforeExhaustionAreNeverTerminal(t *testing.T) {
	p := delivery.RetryPolicy{Base: time.Second, Factor: 2, Cap: time.Minute, MaxAttempts: 4}
	now := time.Unix(1_700_000_000, 0)
	results := []delivery.AttemptResult{
		delivery.ResultFromResponse(408, "", now), delivery.ResultFromResponse(425, "", now), delivery.ResultFromResponse(429, "30", now),
		delivery.ResultFromResponse(500, "", now), delivery.ResultFromResponse(502, "", now), delivery.ResultFromResponse(503, "", now), delivery.ResultFromResponse(599, "", now),
		delivery.ResultFromRequestError(egress.ClassDNS).Result, delivery.ResultFromRequestError(egress.ClassConnect).Result,
		delivery.ResultFromRequestError(egress.ClassTLS).Result, delivery.ResultFromRequestError(egress.ClassTimeout).Result,
		delivery.ResultFromRequestError(egress.ClassProtocol).Result,
		{Outcome: delivery.OutcomeAbandoned, ErrorClass: "lease_expired"}, {Outcome: delivery.OutcomeAbandoned, ErrorClass: "secret_unavailable"},
	}
	for _, res := range results {
		for attempt := 1; attempt < p.MaxAttempts; attempt++ {
			d := p.Decide(attempt, res, now, nil)
			if d.Status != delivery.StatusPending || d.FailureReason != "" || d.NextAttemptAt.IsZero() {
				t.Errorf("%+v at attempt %d: %+v, want a retry without a reason", res, attempt, d)
			}
		}
	}
}

// Exhaustive property: over every outcome, status code, error class and attempt number, a
// decision carries a valid reason exactly when it is FAILED, and the reason agrees with why.
func TestEveryDecisionCarriesAReasonExactlyWhenItIsFailed(t *testing.T) {
	outcomes := []delivery.Outcome{delivery.OutcomeSuccess, delivery.OutcomeHTTPError, delivery.OutcomeRedirect, delivery.OutcomeNetworkError,
		delivery.OutcomeTimeout, delivery.OutcomeBlockedByPolicy, delivery.OutcomeAbandoned, "SOMETHING_NEW"}
	statuses := []int{0, 99, 200, 204, 301, 400, 404, 408, 410, 425, 429, 499, 500, 503, 599}
	classes := []string{"", "blocked_address", "invalid_url", "dns_error", "timeout", delivery.ErrorClassDataMissing, "lease_expired",
		"destination_inactive", "secret_unavailable", delivery.ErrorClassShutdownInterrupted, "tls_error"}
	now := time.Unix(1_700_000_000, 0)
	for _, max := range []int{1, 2, 3, 8, 50} {
		p := delivery.RetryPolicy{Base: time.Second, Factor: 2, Cap: time.Minute, MaxAttempts: max}
		for _, o := range outcomes {
			for _, st := range statuses {
				for _, c := range classes {
					res := delivery.AttemptResult{Outcome: o, HTTPStatus: st, ErrorClass: c}
					for attempt := 1; attempt <= max; attempt++ {
						d := p.Decide(attempt, res, now, nil)
						label := fmt.Sprintf("max=%d attempt=%d %+v", max, attempt, res)
						switch d.Status {
						case delivery.StatusFailed:
							if !d.FailureReason.Valid() {
								t.Fatalf("%s: FAILED without a valid reason: %+v", label, d)
							}
							if (d.Reason == delivery.ReasonExhausted) != (d.FailureReason == delivery.FailureRetriesExhausted) {
								t.Fatalf("%s: reason %q disagrees with %q", label, d.Reason, d.FailureReason)
							}
							if d.Reason == delivery.ReasonExhausted && (!res.Retryable() || attempt < max) {
								t.Fatalf("%s: exhausted too early or on a final result", label)
							}
						case delivery.StatusPending, delivery.StatusSucceeded:
							if d.FailureReason != "" {
								t.Fatalf("%s: %s must not carry a failure reason: %+v", label, d.Status, d)
							}
						default:
							t.Fatalf("%s: unexpected status %s", label, d.Status)
						}
						if o == delivery.OutcomeSuccess && d.Status != delivery.StatusSucceeded {
							t.Fatalf("%s: success must succeed", label)
						}
					}
				}
			}
		}
	}
}

// Finish refuses a decision whose status and reason disagree before it writes anything, so a
// programming error cannot reach the database as a half-written result.
func TestFinishRejectsDecisionsWhoseStatusAndReasonDisagree(t *testing.T) {
	now := time.Now()
	rec := delivery.AttemptRecord{Number: 1, StartedAt: now, FinishedAt: now, Outcome: delivery.OutcomeHTTPError, HTTPStatus: 404}
	bad := map[string]delivery.Decision{
		"FAILED without a reason":       {Status: delivery.StatusFailed},
		"FAILED with an invented one":   {Status: delivery.StatusFailed, FailureReason: "BECAUSE"},
		"FAILED with a lowercase one":   {Status: delivery.StatusFailed, FailureReason: "internal_error"},
		"PENDING with a reason":         {Status: delivery.StatusPending, NextAttemptAt: now, FailureReason: delivery.FailureRetriesExhausted},
		"SUCCEEDED with a reason":       {Status: delivery.StatusSucceeded, FailureReason: delivery.FailureDestinationRejected},
		"PENDING with an invalid one":   {Status: delivery.StatusPending, NextAttemptAt: now, FailureReason: "x"},
		"SUCCEEDED with an invalid one": {Status: delivery.StatusSucceeded, FailureReason: "x"},
	}
	for name, d := range bad {
		// A nil transaction proves nothing was attempted: using it would panic.
		if err := delivery.Finish(bg, nil, "org", "id", "token", rec, d, now); err == nil {
			t.Errorf("%s: Finish accepted it", name)
		}
	}
}

// ---- persistence through the worker ----

// reasonsOf returns the delivery's status and failure reason, and every attempt's terminal reason.
func (f *fixture) reasonsOf(t *testing.T, id string) (status string, reason *string, terminal []*string) {
	t.Helper()
	q, _ := f.env.Pool.Querier()
	if err := q.QueryRow(bg, "SELECT status, failure_reason FROM deliveries WHERE id = $1", id).Scan(&status, &reason); err != nil {
		t.Fatal(err)
	}
	rows, err := q.Query(bg, "SELECT terminal_reason FROM delivery_attempts WHERE delivery_id = $1 ORDER BY attempt_number", id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var r *string
		if err := rows.Scan(&r); err != nil {
			t.Fatal(err)
		}
		terminal = append(terminal, r)
	}
	return
}

func str(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

// counters parses a rendered registry into name -> value, skipping comments and labelled series.
func counters(reg *observability.Registry) map[string]string {
	var sb strings.Builder
	reg.Render(&sb)
	out := map[string]string{}
	for _, line := range strings.Split(sb.String(), "\n") {
		if line == "" || strings.HasPrefix(line, "#") || strings.Contains(line, "{") {
			continue
		}
		if f := strings.Fields(line); len(f) == 2 {
			out[f[0]] = f[1]
		}
	}
	return out
}

func TestEachFailureReasonIsStoredOnTheDeliveryAndOnlyOnTheTerminalAttempt(t *testing.T) {
	cases := []struct {
		name     string
		max      int
		respond  func(egress.Request, int) (egress.Response, error)
		want     delivery.FailureReason
		attempts int
	}{
		{"final 404", 8, status(404), delivery.FailureDestinationRejected, 1},
		{"final 410", 8, status(410), delivery.FailureDestinationRejected, 1},
		{"final 400 on the last of one attempt", 1, status(400), delivery.FailureDestinationRejected, 1},
		{"redirect", 8, status(301), delivery.FailureRedirectRefused, 1},
		{"redirect 308", 8, status(308), delivery.FailureRedirectRefused, 1},
		{"blocked address", 8, requestErr(egress.ClassBlockedAddress), delivery.FailureEgressBlocked, 1},
		{"invalid url", 8, requestErr(egress.ClassInvalidURL), delivery.FailureEgressBlocked, 1},
		{"503 with max_attempts=1", 1, status(503), delivery.FailureRetriesExhausted, 1},
		{"timeout with max_attempts=1", 1, requestErr(egress.ClassTimeout), delivery.FailureRetriesExhausted, 1},
		{"429 exhausted after three", 3, status(429), delivery.FailureRetriesExhausted, 3},
		{"network error exhausted after three", 3, requestErr(egress.ClassConnect), delivery.FailureRetriesExhausted, 3},
		{"tls error exhausted after two", 2, requestErr(egress.ClassTLS), delivery.FailureRetriesExhausted, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			org, user := f.owner.Org.ID, f.owner.Principal.User.ID
			d := f.destination(t, org, user, "D")
			id := f.pending(t, org, f.event(t, `{}`), d.ID, c.max)
			reg := observability.NewRegistry()
			w := f.worker(t, &fakeSender{respond: c.respond}, func(dd *delivery.WorkerDeps) { dd.Metrics = delivery.NewMetrics(reg) })

			for i := 1; i <= c.attempts; i++ {
				if n := f.runOnce(t, w); n != 1 {
					t.Fatalf("attempt %d processed %d deliveries", i, n)
				}
				st, reason, terminal := f.reasonsOf(t, id)
				if i < c.attempts {
					if st != "PENDING" || reason != nil || len(terminal) != i {
						t.Fatalf("after attempt %d the delivery must still be retryable without a reason: %s %s %v", i, st, str(reason), terminal)
					}
					for _, tr := range terminal {
						if tr != nil {
							t.Fatalf("a retryable attempt got terminal reason %s", *tr)
						}
					}
				}
				f.env.Clock.Advance(7 * time.Hour)
			}
			st, reason, terminal := f.reasonsOf(t, id)
			if st != "FAILED" || reason == nil || *reason != string(c.want) {
				t.Fatalf("delivery: %s %s, want FAILED %s", st, str(reason), c.want)
			}
			if len(terminal) != c.attempts {
				t.Fatalf("%d attempts, want %d", len(terminal), c.attempts)
			}
			for i, tr := range terminal {
				last := i == len(terminal)-1
				if last && (tr == nil || *tr != string(c.want)) {
					t.Fatalf("the terminal attempt has reason %s, want %s", str(tr), c.want)
				}
				if !last && tr != nil {
					t.Fatalf("attempt %d is not terminal but has reason %s", i+1, *tr)
				}
			}
			// One failure, counted once in the total and once under its own reason.
			got := counters(reg)
			if got["relayforge_delivery_failed_total"] != "1" {
				t.Fatalf("total counter: %v", got)
			}
			for _, r := range delivery.AllFailureReasons() {
				want := "0"
				if r == c.want {
					want = "1"
				}
				if got[delivery.FailedReasonMetricName(r)] != want {
					t.Errorf("%s = %s, want %s", delivery.FailedReasonMetricName(r), got[delivery.FailedReasonMetricName(r)], want)
				}
			}
		})
	}
}

func TestRetryableResultsStayPendingWithoutAReasonUntilTheBudgetIsSpent(t *testing.T) {
	f := newFixture(t)
	org, user := f.owner.Org.ID, f.owner.Principal.User.ID
	d := f.destination(t, org, user, "D")
	id := f.pending(t, org, f.event(t, `{}`), d.ID, 5)
	w := f.worker(t, &fakeSender{respond: status(503)}, nil)
	for i := 1; i <= 4; i++ {
		f.runOnce(t, w)
		if st, reason, _ := f.reasonsOf(t, id); st != "PENDING" || reason != nil {
			t.Fatalf("attempt %d: %s %s", i, st, str(reason))
		}
		f.env.Clock.Advance(7 * time.Hour)
	}
	f.runOnce(t, w)
	if st, reason, terminal := f.reasonsOf(t, id); st != "FAILED" || str(reason) != "RETRIES_EXHAUSTED" || len(terminal) != 5 {
		t.Fatalf("attempt 5: %s %s %v", st, str(reason), terminal)
	}
}

func TestASuccessfulDeliveryNeverGetsAReason(t *testing.T) {
	f := newFixture(t)
	org, user := f.owner.Org.ID, f.owner.Principal.User.ID
	d := f.destination(t, org, user, "D")
	id := f.pending(t, org, f.event(t, `{}`), d.ID, 8)
	f.runOnce(t, f.worker(t, &fakeSender{respond: status(204)}, nil))
	if st, reason, terminal := f.reasonsOf(t, id); st != "SUCCEEDED" || reason != nil || len(terminal) != 1 || terminal[0] != nil {
		t.Fatalf("%s %s %v", st, str(reason), terminal)
	}
}

// Situation 6: platform problems that retry until the budget is gone are RETRIES_EXHAUSTED, and the
// attempt's error class (already exposed) still says what the problem was.
func TestPlatformProblemsOnTheLastAttemptEndAsRetriesExhausted(t *testing.T) {
	t.Run("destination paused after the claim", func(t *testing.T) {
		f := newFixture(t)
		org, user := f.owner.Org.ID, f.owner.Principal.User.ID
		d := f.destination(t, org, user, "D")
		id := f.pending(t, org, f.event(t, `{}`), d.ID, 1)
		w := f.worker(t, &fakeSender{}, func(dd *delivery.WorkerDeps) {
			dd.AfterClaim = func() { f.env.Exec(t, "UPDATE destinations SET status = 'PAUSED' WHERE id = $1", d.ID) }
		})
		f.runOnce(t, w)
		st, reason, terminal := f.reasonsOf(t, id)
		a := f.attempts(t, id)
		if st != "FAILED" || str(reason) != "RETRIES_EXHAUSTED" || str(terminal[0]) != "RETRIES_EXHAUSTED" || str(a[0].ErrorClass) != "destination_inactive" {
			t.Fatalf("%s %s %v %+v", st, str(reason), terminal, a)
		}
	})
	t.Run("secret cannot be decrypted", func(t *testing.T) {
		f := newFixture(t)
		org, user := f.owner.Org.ID, f.owner.Principal.User.ID
		d := f.destination(t, org, user, "D")
		id := f.pending(t, org, f.event(t, `{}`), d.ID, 1)
		otherKey, _ := secretbox.New([]byte("another-32-byte-key-for-testing!"), 1)
		f.runOnce(t, f.worker(t, &fakeSender{}, func(dd *delivery.WorkerDeps) { dd.Box = otherKey }))
		st, reason, terminal := f.reasonsOf(t, id)
		a := f.attempts(t, id)
		if st != "FAILED" || str(reason) != "RETRIES_EXHAUSTED" || str(terminal[0]) != "RETRIES_EXHAUSTED" || str(a[0].ErrorClass) != "secret_unavailable" {
			t.Fatalf("%s %s %v %+v", st, str(reason), terminal, a)
		}
	})
}

// Situation 4 cannot be provoked through the worker (foreign keys keep the data), so it is driven
// through the same Decide and Finish path the worker uses.
func TestAnUnclassifiableFinalResultIsStoredAsInternalError(t *testing.T) {
	f := newFixture(t)
	org, user := f.owner.Org.ID, f.owner.Principal.User.ID
	d := f.destination(t, org, user, "D")
	id := f.pending(t, org, f.event(t, `{}`), d.ID, 8)
	c := claimOne(t, f, "w")
	now := f.env.Clock.Now()
	res := delivery.AttemptResult{Outcome: delivery.OutcomeBlockedByPolicy, ErrorClass: delivery.ErrorClassDataMissing}
	dec := delivery.DefaultRetryPolicy().Decide(c.Attempt, res, now, nil)
	rec := delivery.AttemptRecord{Number: c.Attempt, StartedAt: now, FinishedAt: now, Outcome: res.Outcome, ErrorClass: res.ErrorClass}
	if err := f.env.Pool.InTx(bg, func(tx pgx.Tx) error {
		return delivery.Finish(bg, tx, c.OrganizationID, c.DeliveryID, c.ClaimToken, rec, dec, now)
	}); err != nil {
		t.Fatal(err)
	}
	if st, reason, terminal := f.reasonsOf(t, id); st != "FAILED" || str(reason) != "INTERNAL_ERROR" || str(terminal[0]) != "INTERNAL_ERROR" {
		t.Fatalf("%s %s %v", st, str(reason), terminal)
	}
}

// Situation 7: the reaper exhausts a delivery whose last attempt was abandoned.
func TestLeaseRecoveryExhaustionStoresRetriesExhausted(t *testing.T) {
	for _, max := range []int{1, 3} {
		t.Run(fmt.Sprintf("max_attempts=%d", max), func(t *testing.T) {
			f := newFixture(t)
			org, user := f.owner.Org.ID, f.owner.Principal.User.ID
			d := f.destination(t, org, user, "D")
			id := f.pending(t, org, f.event(t, `{}`), d.ID, max)
			reg := observability.NewRegistry()
			w := f.worker(t, &fakeSender{}, func(dd *delivery.WorkerDeps) { dd.Metrics = delivery.NewMetrics(reg) })
			for round := 1; round <= max; round++ {
				claimOne(t, f, "crashing")
				f.env.Clock.Advance(3 * time.Minute)
				if n, err := w.RecoverExpired(bg); err != nil || n != 1 {
					t.Fatalf("round %d: %d %v", round, n, err)
				}
				st, reason, terminal := f.reasonsOf(t, id)
				if round < max && (st != "PENDING" || reason != nil || terminal[round-1] != nil) {
					t.Fatalf("round %d must be retryable without a reason: %s %s %v", round, st, str(reason), terminal)
				}
				f.env.Clock.Advance(7 * time.Hour)
			}
			st, reason, terminal := f.reasonsOf(t, id)
			if st != "FAILED" || str(reason) != "RETRIES_EXHAUSTED" || len(terminal) != max || str(terminal[max-1]) != "RETRIES_EXHAUSTED" {
				t.Fatalf("%s %s %v", st, str(reason), terminal)
			}
			for _, tr := range terminal[:max-1] {
				if tr != nil {
					t.Fatalf("only the terminal attempt gets a reason: %v", terminal)
				}
			}
			if got := counters(reg); got["relayforge_delivery_failed_total"] != "1" || got["relayforge_delivery_failed_retries_exhausted_total"] != "1" {
				t.Fatalf("metrics: %v", got)
			}
		})
	}
}

// ---- atomicity, fencing and concurrency ----

func TestAStaleClaimStoresNeitherReason(t *testing.T) {
	f := newFixture(t)
	org, user := f.owner.Org.ID, f.owner.Principal.User.ID
	d := f.destination(t, org, user, "D")
	id := f.pending(t, org, f.event(t, `{}`), d.ID, 8)
	c := claimOne(t, f, "w")
	var wrong string
	q, _ := f.env.Pool.Querier()
	if err := q.QueryRow(bg, "SELECT gen_random_uuid()::text").Scan(&wrong); err != nil {
		t.Fatal(err)
	}
	now := f.env.Clock.Now()
	rec := delivery.AttemptRecord{Number: c.Attempt, StartedAt: now, FinishedAt: now, Outcome: delivery.OutcomeHTTPError, HTTPStatus: 404}
	err := f.env.Pool.InTx(bg, func(tx pgx.Tx) error {
		return delivery.Finish(bg, tx, c.OrganizationID, c.DeliveryID, wrong, rec,
			delivery.Decision{Status: delivery.StatusFailed, Reason: delivery.ReasonNonRetryable, FailureReason: delivery.FailureDestinationRejected}, now)
	})
	if err != delivery.ErrStaleClaim {
		t.Fatalf("got %v, want ErrStaleClaim", err)
	}
	st, reason, terminal := f.reasonsOf(t, id)
	if st != "IN_FLIGHT" || reason != nil || len(terminal) != 0 {
		t.Fatalf("a rejected finalization left %s %s %v behind", st, str(reason), terminal)
	}
}

// If the delivery update fails after the attempt was inserted, the attempt and its terminal
// reason roll back with it: the two reasons are written together or not at all.
func TestAFailedDeliveryUpdateRollsBackTheAttemptsTerminalReason(t *testing.T) {
	f := newFixture(t)
	org, user := f.owner.Org.ID, f.owner.Principal.User.ID
	d := f.destination(t, org, user, "D")
	id := f.pending(t, org, f.event(t, `{}`), d.ID, 8)
	c := claimOne(t, f, "w")
	f.env.Exec(t, `CREATE FUNCTION fail_egress_blocked() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN IF NEW.failure_reason = 'EGRESS_BLOCKED' THEN RAISE EXCEPTION 'injected failure' USING ERRCODE = 'integrity_constraint_violation'; END IF; RETURN NEW; END; $$`)
	f.env.Exec(t, "CREATE TRIGGER fail_egress_blocked BEFORE UPDATE ON deliveries FOR EACH ROW EXECUTE FUNCTION fail_egress_blocked()")

	now := f.env.Clock.Now()
	res := delivery.AttemptResult{Outcome: delivery.OutcomeBlockedByPolicy, ErrorClass: "blocked_address"}
	dec := delivery.DefaultRetryPolicy().Decide(c.Attempt, res, now, nil)
	if dec.FailureReason != delivery.FailureEgressBlocked {
		t.Fatalf("setup: %+v", dec)
	}
	rec := delivery.AttemptRecord{Number: c.Attempt, StartedAt: now, FinishedAt: now, Outcome: res.Outcome, ErrorClass: res.ErrorClass}
	err := f.env.Pool.InTx(bg, func(tx pgx.Tx) error {
		return delivery.Finish(bg, tx, c.OrganizationID, c.DeliveryID, c.ClaimToken, rec, dec, now)
	})
	if err == nil {
		t.Fatal("the injected failure must surface")
	}
	if st, reason, terminal := f.reasonsOf(t, id); st != "IN_FLIGHT" || reason != nil || len(terminal) != 0 {
		t.Fatalf("a partial result survived: %s %s %v", st, str(reason), terminal)
	}
}

// A worker and the reaper race on the last attempt of a delivery: exactly one of them records
// it, and both reasons come from that one winner.
func TestWorkerAndRecoveryRacingOnTheLastAttemptStoreOneConsistentReason(t *testing.T) {
	f := newFixture(t)
	org, user := f.owner.Org.ID, f.owner.Principal.User.ID
	d := f.destination(t, org, user, "D")
	w := f.worker(t, &fakeSender{}, nil)
	var workerWins, reaperWins int
	for i := 0; i < 30; i++ {
		id := f.pending(t, org, f.event(t, fmt.Sprintf(`{"i":%d}`, i)), d.ID, 1)
		c := claimOne(t, f, "slow")
		if c.DeliveryID != id {
			t.Fatalf("claimed %s, want %s", c.DeliveryID, id)
		}
		f.env.Clock.Advance(3 * time.Minute)
		now := f.env.Clock.Now()
		rec := delivery.AttemptRecord{Number: c.Attempt, StartedAt: now, FinishedAt: now, Outcome: delivery.OutcomeHTTPError, HTTPStatus: 404}
		dec := delivery.Decision{Status: delivery.StatusFailed, Reason: delivery.ReasonNonRetryable, FailureReason: delivery.FailureDestinationRejected}

		var wg sync.WaitGroup
		start := make(chan struct{})
		var workerErr, recErr error
		var recovered int
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			workerErr = f.env.Pool.InTx(bg, func(tx pgx.Tx) error {
				return delivery.Finish(bg, tx, c.OrganizationID, c.DeliveryID, c.ClaimToken, rec, dec, now)
			})
		}()
		go func() { defer wg.Done(); <-start; recovered, recErr = w.RecoverExpired(bg) }()
		close(start)
		wg.Wait()
		if recErr != nil {
			t.Fatalf("iteration %d: %v", i, recErr)
		}
		st, reason, terminal := f.reasonsOf(t, id)
		if st != "FAILED" || len(terminal) != 1 || reason == nil || terminal[0] == nil || *reason != *terminal[0] {
			t.Fatalf("iteration %d: %s %s %v (worker %v, recovered %d)", i, st, str(reason), terminal, workerErr, recovered)
		}
		switch {
		case workerErr == nil && recovered == 0 && *reason == "DESTINATION_REJECTED":
			workerWins++
		case workerErr != nil && recovered == 1 && *reason == "RETRIES_EXHAUSTED":
			reaperWins++
		default:
			t.Fatalf("iteration %d: inconsistent: worker %v, recovered %d, reason %s", i, workerErr, recovered, *reason)
		}
	}
	t.Logf("worker won %d, reaper won %d", workerWins, reaperWins)
}

func TestConcurrentWorkersStoreOneReasonPerDeliveryAndCountItOnce(t *testing.T) {
	f := newFixture(t)
	org, user := f.owner.Org.ID, f.owner.Principal.User.ID
	d := f.destination(t, org, user, "D")
	const total = 24
	ids := make([]string, total)
	for i := range ids {
		ids[i] = f.pending(t, org, f.event(t, fmt.Sprintf(`{"i":%d}`, i)), d.ID, 1)
	}
	reg := observability.NewRegistry()
	metrics := delivery.NewMetrics(reg)
	s := &fakeSender{respond: status(404)}
	var wg sync.WaitGroup
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := f.worker(t, s, func(dd *delivery.WorkerDeps) { dd.Metrics = metrics })
			for {
				n, err := w.RunOnce(bg)
				if err != nil {
					t.Error(err)
					return
				}
				if n == 0 {
					return
				}
			}
		}()
	}
	wg.Wait()
	for _, id := range ids {
		st, reason, terminal := f.reasonsOf(t, id)
		if st != "FAILED" || str(reason) != "DESTINATION_REJECTED" || len(terminal) != 1 || str(terminal[0]) != "DESTINATION_REJECTED" {
			t.Errorf("delivery %s: %s %s %v", id, st, str(reason), terminal)
		}
	}
	got := counters(reg)
	if got["relayforge_delivery_failed_total"] != "24" || got["relayforge_delivery_failed_destination_rejected_total"] != "24" {
		t.Fatalf("metrics: %v", got)
	}
}

// ---- metrics and logs ----

func TestFailureMetricsAreFixedNamedAndUnlabelled(t *testing.T) {
	reg := observability.NewRegistry()
	delivery.NewMetrics(reg)
	var names []string
	for name := range counters(reg) {
		if strings.HasPrefix(name, "relayforge_delivery_failed") {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	want := []string{
		"relayforge_delivery_failed_destination_rejected_total", "relayforge_delivery_failed_egress_blocked_total",
		"relayforge_delivery_failed_internal_error_total", "relayforge_delivery_failed_redirect_refused_total",
		"relayforge_delivery_failed_retries_exhausted_total", "relayforge_delivery_failed_total",
	}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("failure metrics: %v, want %v", names, want)
	}
	var out strings.Builder
	reg.Render(&out)
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(line, "relayforge_delivery_failed") && strings.ContainsAny(line, "{}\"") {
			t.Errorf("a failure metric carries labels: %s", line)
		}
	}
	// A nil registry and a zero Metrics must stay harmless (the worker defaults to one).
	delivery.NewMetrics(nil).FailedByReason[delivery.FailureInternalError].Inc()
	(&delivery.Metrics{}).FailedByReason[delivery.FailureInternalError].Inc()
}

func TestFailureLogsCarryTheReasonAndNothingSensitive(t *testing.T) {
	f := newFixture(t)
	org, user := f.owner.Org.ID, f.owner.Principal.User.ID
	d := f.destination(t, org, user, "D")
	f.pending(t, org, f.event(t, `{"card":"4111-PAYLOAD-SECRET"}`), d.ID, 8) // final 404
	f.pending(t, org, f.event(t, `{"card":"4111-PAYLOAD-SECRET-2"}`), d.ID, 1)
	var logs strings.Builder
	var mu sync.Mutex
	logger := slog.New(slog.NewTextHandler(writerFunc(func(p []byte) (int, error) { mu.Lock(); defer mu.Unlock(); return logs.Write(p) }), &slog.HandlerOptions{Level: slog.LevelDebug}))
	s := &fakeSender{respond: func(req egress.Request, _ int) (egress.Response, error) {
		if strings.Contains(string(req.Body), "PAYLOAD-SECRET-2") {
			return egress.Response{StatusCode: 503}, nil // max_attempts=1: exhausted
		}
		return egress.Response{StatusCode: 404}, nil
	}}
	w := f.worker(t, s, func(dd *delivery.WorkerDeps) { dd.Log = logger })
	f.runOnce(t, w)
	// A third delivery fails through lease recovery.
	f.pending(t, org, f.event(t, `{"card":"3"}`), d.ID, 1)
	claimOne(t, f, "crashing")
	f.env.Clock.Advance(3 * time.Minute)
	if _, err := w.RecoverExpired(bg); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	out := logs.String()
	mu.Unlock()
	for _, want := range []string{"failure_reason=DESTINATION_REJECTED", "failure_reason=RETRIES_EXHAUSTED"} {
		if !strings.Contains(out, want) {
			t.Errorf("logs miss %q:\n%s", want, out)
		}
	}
	if strings.Count(out, "failure_reason=RETRIES_EXHAUSTED") < 2 {
		t.Errorf("both the worker and the recovery path must log the reason:\n%s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "failure_reason=") && !strings.Contains(line, "status=FAILED") {
			t.Errorf("a reason was logged for a delivery that did not fail: %s", line)
		}
	}
	for _, banned := range []string{"PAYLOAD-SECRET", "SECRETTOKEN", d.Secret, "hooks.example.com", "Authorization", "v1="} {
		if strings.Contains(out, banned) {
			t.Errorf("logs contain %q", banned)
		}
	}
}
