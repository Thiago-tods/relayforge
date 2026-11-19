package delivery

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Thiago-tods/relayforge/internal/database"
)

// CountCap is the largest value any operational counter reports. A counter that reaches it is
// saturated: the real number is AT LEAST CountCap, not exactly CountCap.
//
// Each counter is computed as count(*) over a subquery with LIMIT CountCap. That bounds the number of
// MATCHING rows the aggregate receives. It does not bound the index entries or heap rows PostgreSQL
// examines to find them: a selective counter (a rare status or failure reason) keeps reading until the
// end of its range, so it can examine a large part of a tenant's window. The request is bounded in
// time by the context deadline of the caller (ADR 31), not by this cap.
const CountCap = 100_000

// UnrecordedFailureReason is the response-only bucket for deliveries that failed before
// migration 00019 and therefore have a NULL failure_reason. It is NOT a FailureReason: it is never
// stored, never validated by the database and never part of the closed taxonomy.
const UnrecordedFailureReason = "UNRECORDED"

// QueueSnapshot is the queue health of one organization at GeneratedAt (ADR 31). Every number
// is a delivery count, never an attempt count.
//
//   - Pending is every PENDING delivery, delayed ones included.
//   - Due is PENDING with next_attempt_at <= GeneratedAt. Due does NOT mean claimable: the
//     claim path also requires an ACTIVE destination, so deliveries of a PAUSED destination are due.
//   - Delayed is PENDING with next_attempt_at > GeneratedAt, counted on its own (never Pending - Due).
//   - InFlight is every IN_FLIGHT delivery, expired leases included.
//   - ExpiredLeases is IN_FLIGHT with lease_expires_at <= GeneratedAt. They await lease recovery;
//     the worker that held the lease is not necessarily dead.
//   - OldestDueAgeSeconds is the exact age of the longest-waiting due delivery (0 when none is due).
//   - Capped is true when at least one count reached CountCap (that count is a lower bound).
type QueueSnapshot struct {
	GeneratedAt         time.Time `json:"generatedAt"`
	Pending             int       `json:"pending"`
	Due                 int       `json:"due"`
	Delayed             int       `json:"delayed"`
	OldestDueAgeSeconds int64     `json:"oldestDueAgeSeconds"`
	InFlight            int       `json:"inFlight"`
	ExpiredLeases       int       `json:"expiredLeases"`
	Capped              bool      `json:"capped"`
}

// StatusCounts counts deliveries by their current status.
type StatusCounts struct {
	Pending   int `json:"PENDING"`
	InFlight  int `json:"IN_FLIGHT"`
	Succeeded int `json:"SUCCEEDED"`
	Failed    int `json:"FAILED"`
	Cancelled int `json:"CANCELLED"`
}

// ReasonCounts counts FAILED deliveries by failure reason. It always has exactly these six keys:
// the five FailureReason values and the response-only UNRECORDED bucket.
type ReasonCounts struct {
	DestinationRejected int `json:"DESTINATION_REJECTED"`
	RedirectRefused     int `json:"REDIRECT_REFUSED"`
	EgressBlocked       int `json:"EGRESS_BLOCKED"`
	RetriesExhausted    int `json:"RETRIES_EXHAUSTED"`
	InternalError       int `json:"INTERNAL_ERROR"`
	Unrecorded          int `json:"UNRECORDED"`
}

// CreatedCohort describes the deliveries CREATED in the window, by their CURRENT status.
type CreatedCohort struct {
	Total    int          `json:"total"`
	ByStatus StatusCounts `json:"byStatus"`
}

// CompletedCohort describes the deliveries that reached a terminal status inside the window
// (completed_at in [From, To)).
//
// FailureRate is failed / (succeeded + failed). Cancelled deliveries (a revoked destination) are
// shown separately and excluded from it. It is null when the denominator is zero and when the
// succeeded or the failed count is capped, because a ratio of lower bounds means nothing.
type CompletedCohort struct {
	Succeeded       int          `json:"succeeded"`
	Failed          int          `json:"failed"`
	Cancelled       int          `json:"cancelled"`
	FailureRate     *float64     `json:"failureRate"`
	ByFailureReason ReasonCounts `json:"byFailureReason"`
}

// DeliverySummary is the delivery-level activity of one organization over [From, To). The two
// cohorts use two independent windows on two different columns: created_at for Created and
// completed_at for Completed. Capped is true when at least one count reached CountCap.
type DeliverySummary struct {
	GeneratedAt time.Time       `json:"generatedAt"`
	From        time.Time       `json:"from"`
	To          time.Time       `json:"to"`
	Created     CreatedCohort   `json:"created"`
	Completed   CompletedCohort `json:"completed"`
	Capped      bool            `json:"capped"`
}

// capped builds one saturating counter: it counts at most CountCap matching rows (see CountCap for
// what that does and does not bound). Each counter has its own LIMIT on purpose: counters saturate
// independently, so they cannot share one limited scan. The predicates are constants of this file
// (never client text); every value is a bind parameter.
func capped(where string) string {
	return fmt.Sprintf("(SELECT count(*) FROM (SELECT 1 FROM deliveries WHERE %s LIMIT %d) c)", where, CountCap)
}

// QueueStatement and SummaryStatement are the exact SQL of QueueOf and SummaryOf. They are exported
// only so tests can inspect their query plans (EXPLAIN); callers use QueueOf and SummaryOf.
//
// In-flight and expired-lease counters have no organization index of their own: they use the global
// deliveries_lease_idx with an organization filter (ADR 31, accepted Stage 1 limitation).
//
// The status literals below are deliberate: a partial index is used only when the query
// repeats its predicate as a constant (a bind parameter would not match it).
var QueueStatement = "SELECT " + strings.Join([]string{
	capped("organization_id = $1 AND status = 'PENDING'"),
	capped("organization_id = $1 AND status = 'PENDING' AND next_attempt_at <= $2"),
	capped("organization_id = $1 AND status = 'PENDING' AND next_attempt_at > $2"),
	capped("organization_id = $1 AND status = 'IN_FLIGHT'"),
	capped("organization_id = $1 AND status = 'IN_FLIGHT' AND lease_expires_at <= $2"),
	"(SELECT min(next_attempt_at) FROM deliveries WHERE organization_id = $1 AND status = 'PENDING' AND next_attempt_at <= $2)",
}, ", ")

// QueueOf reads the queue health of an organization in one statement, so every number comes from
// the same snapshot. It takes no row lock and never blocks a worker.
func QueueOf(ctx context.Context, db database.Querier, orgID string, now time.Time) (QueueSnapshot, error) {
	now = now.UTC()
	var pending, due, delayed, inFlight, expired int
	var oldest *time.Time
	if err := db.QueryRow(ctx, QueueStatement, orgID, now).Scan(&pending, &due, &delayed, &inFlight, &expired, &oldest); err != nil {
		return QueueSnapshot{}, fmt.Errorf("read queue health: %w", err)
	}
	q := QueueSnapshot{GeneratedAt: now, Pending: pending, Due: due, Delayed: delayed, InFlight: inFlight, ExpiredLeases: expired}
	if oldest != nil && now.After(*oldest) {
		q.OldestDueAgeSeconds = int64(now.Sub(*oldest) / time.Second)
	}
	q.Capped = anyAtCap(pending, due, delayed, inFlight, expired)
	return q, nil
}

const (
	createdWindow   = "organization_id = $1 AND created_at >= $2 AND created_at < $3"
	completedWindow = "organization_id = $1 AND completed_at >= $2 AND completed_at < $3"
)

func createdCount(status string) string {
	if status == "" {
		return capped(createdWindow)
	}
	return capped(createdWindow + " AND status = '" + status + "'")
}

func failedBy(reason string) string {
	if reason == "" {
		return capped(completedWindow + " AND status = 'FAILED' AND failure_reason IS NULL")
	}
	return capped(completedWindow + " AND status = 'FAILED' AND failure_reason = '" + reason + "'")
}

// SummaryStatement: see QueueStatement.
var SummaryStatement = "SELECT " + strings.Join([]string{
	createdCount(""),
	createdCount("PENDING"), createdCount("IN_FLIGHT"), createdCount("SUCCEEDED"), createdCount("FAILED"), createdCount("CANCELLED"),
	capped(completedWindow + " AND status = 'SUCCEEDED'"),
	capped(completedWindow + " AND status = 'FAILED'"),
	capped(completedWindow + " AND status = 'CANCELLED'"),
	failedBy(string(FailureDestinationRejected)), failedBy(string(FailureRedirectRefused)), failedBy(string(FailureEgressBlocked)),
	failedBy(string(FailureRetriesExhausted)), failedBy(string(FailureInternalError)), failedBy(""),
}, ", ")

// SummaryOf reads the delivery activity of an organization for the half-open window [from, to)
// in one statement (one snapshot). The caller validates and normalizes the window (UTC, microseconds,
// a supported range); this function trusts it only as bind parameters, so a hostile window can change a
// result but never the statement. now is reported as GeneratedAt and is not used in the SQL.
func SummaryOf(ctx context.Context, db database.Querier, orgID string, from, to, now time.Time) (DeliverySummary, error) {
	from, to, now = from.UTC(), to.UTC(), now.UTC()
	var c CreatedCohort
	var s CompletedCohort
	if err := db.QueryRow(ctx, SummaryStatement, orgID, from, to).Scan(
		&c.Total, &c.ByStatus.Pending, &c.ByStatus.InFlight, &c.ByStatus.Succeeded, &c.ByStatus.Failed, &c.ByStatus.Cancelled,
		&s.Succeeded, &s.Failed, &s.Cancelled,
		&s.ByFailureReason.DestinationRejected, &s.ByFailureReason.RedirectRefused, &s.ByFailureReason.EgressBlocked,
		&s.ByFailureReason.RetriesExhausted, &s.ByFailureReason.InternalError, &s.ByFailureReason.Unrecorded,
	); err != nil {
		return DeliverySummary{}, fmt.Errorf("read delivery summary: %w", err)
	}
	s.FailureRate = failureRate(s.Succeeded, s.Failed)
	r := s.ByFailureReason
	return DeliverySummary{
		GeneratedAt: now, From: from, To: to, Created: c, Completed: s,
		Capped: anyAtCap(c.Total, c.ByStatus.Pending, c.ByStatus.InFlight, c.ByStatus.Succeeded, c.ByStatus.Failed, c.ByStatus.Cancelled,
			s.Succeeded, s.Failed, s.Cancelled,
			r.DestinationRejected, r.RedirectRefused, r.EgressBlocked, r.RetriesExhausted, r.InternalError, r.Unrecorded),
	}, nil
}

// failureRate is failed / (succeeded + failed), or nil when it would be meaningless: nothing
// completed, or either side saturated (a ratio of two lower bounds is not a rate).
func failureRate(succeeded, failed int) *float64 {
	if succeeded >= CountCap || failed >= CountCap || succeeded+failed == 0 {
		return nil
	}
	rate := float64(failed) / float64(succeeded+failed)
	return &rate
}

func anyAtCap(counts ...int) bool {
	for _, n := range counts {
		if n >= CountCap {
			return true
		}
	}
	return false
}
