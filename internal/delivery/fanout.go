package delivery

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Thiago-tods/relayforge/internal/database"
)

// activeAtAcceptance is the event-time eligibility rule shared by FanOut and
// Reconcile. A subscription is a lifecycle interval [created_at, revoked_at); it
// applies to an event when the event was accepted inside that interval. The
// current status is deliberately not consulted: revoking a subscription later
// must not remove an event that was accepted while it was active, and a
// subscription created later must not receive history. Timestamps are
// transaction-start times (now()) in UTC, so comparing them is consistent:
// created_at <= received_at (inclusive) and revoked_at > received_at (exclusive).
const activeAtAcceptance = `s.created_at <= e.received_at AND (s.revoked_at IS NULL OR s.revoked_at > e.received_at)`

// eligibleDeliveriesSQL selects the (event, destination) pairs that are due for the
// events in the CTE or table named by source. FanOut and Reconcile both use it, so the
// ADR 26 rule exists in exactly one place.
func eligibleDeliveriesSQL(source string) string {
	return `
		SELECT e.organization_id, e.id AS event_id, s.destination_id
		  FROM ` + source + ` b
		  JOIN webhook_events e ON e.id = b.event_id AND e.organization_id = b.organization_id
		  JOIN subscriptions s ON s.ingestion_endpoint_id = e.ingestion_endpoint_id AND s.organization_id = e.organization_id
		       AND ` + activeAtAcceptance + `
		  JOIN destinations x ON x.id = s.destination_id AND x.organization_id = s.organization_id
		       AND (x.revoked_at IS NULL OR x.revoked_at > e.received_at)`
}

// FanOut creates one PENDING delivery for every subscription that was active when
// the event was accepted, marks the event's outbox row as fanned out, and returns how
// many deliveries it created. A destination that was revoked after acceptance still
// gets its delivery, which the worker then cancels, so the decision stays auditable;
// a destination revoked before acceptance gets none. It is idempotent:
// UNIQUE (event_id, destination_id) with ON CONFLICT DO NOTHING means a duplicate or
// concurrent call creates nothing new.
//
// The marker (outbox.fanned_out_at, migration 00018) is written in the caller's
// transaction, before the deliveries, so the two commit or roll back together. It is
// written first on purpose: Reconcile locks the outbox row and then inserts deliveries,
// so FanOut takes the same locks in the same order (outbox row, then delivery keys).
// Opposite orders would allow a lock-order inversion between the two. (A stress test
// did not manage to provoke a deadlock with the opposite order, so this is a precaution
// rather than a fix for an observed failure.) The update only touches an unmarked row,
// so a repeat keeps the time of the first commit.
//
// The organization is matched in both statements, so a message that names the wrong
// organization finds no event, marks nothing and creates nothing.
// Deliveries for paused destinations are created and simply wait: the claim
// query skips them until the destination is activated.
func FanOut(ctx context.Context, tx database.Querier, orgID, eventID string, now time.Time) (int64, error) {
	if _, err := tx.Exec(ctx, `
		UPDATE outbox SET fanned_out_at = $3
		 WHERE event_id = $1 AND organization_id = $2 AND fanned_out_at IS NULL`, eventID, orgID, now); err != nil {
		return 0, fmt.Errorf("mark event fanned out: %w", err)
	}
	tag, err := tx.Exec(ctx, `
		INSERT INTO deliveries (organization_id, event_id, destination_id, next_attempt_at)
		SELECT organization_id, event_id, destination_id, $3::timestamptz
		  FROM (`+eligibleDeliveriesSQL(`(SELECT $1::uuid AS event_id, $2::uuid AS organization_id)`)+`) due
		ON CONFLICT (event_id, destination_id) DO NOTHING`, eventID, orgID, now)
	if err != nil {
		return 0, fmt.Errorf("fan out event: %w", err)
	}
	return tag.RowsAffected(), nil
}

// Lane selects which part of the unmarked backlog a reconciliation batch works on.
//
// Two lanes keep a large historical backlog from delaying the repair of a fan-out
// that was lost recently (ADR 29). After migration 00018 every historical outbox row is
// unmarked; in one FIFO queue a loss from today would wait behind all of them.
type Lane string

const (
	// LaneLive covers rows accepted between the grace period and LiveWindow ago.
	LaneLive Lane = "live"
	// LaneHistory covers rows older than LiveWindow, with no upper age limit.
	LaneHistory Lane = "history"
)

// DefaultLiveWindow is the age that separates the lanes. It equals the JetStream stream's
// MaxAge: a row older than that cannot be helped by the broker any more.
const DefaultLiveWindow = 24 * time.Hour

// MaxReconcileSkip bounds ReconcileParams.Skip.
const MaxReconcileSkip = 1000

// ReconcileParams selects and bounds one reconciliation batch.
type ReconcileParams struct {
	Now  time.Time
	Lane Lane
	// Grace leaves recently accepted events to the normal path.
	Grace time.Duration
	// LiveWindow separates the lanes; it must exceed Grace.
	LiveWindow time.Duration
	// Limit is the most rows one batch examines.
	Limit int
	// Skip lists outbox ids not to select, so rows that already failed in this sweep
	// are not retried by the next batch of the same sweep. At most MaxReconcileSkip.
	Skip []string
}

func (p ReconcileParams) validate() error {
	switch {
	case p.Lane != LaneLive && p.Lane != LaneHistory:
		return fmt.Errorf("reconcile: unknown lane %q", p.Lane)
	case p.Grace <= 0 || p.LiveWindow <= p.Grace:
		return errors.New("reconcile: the live window must be longer than the grace period, which must be positive")
	case p.Limit < 1:
		return errors.New("reconcile: limit must be positive")
	case len(p.Skip) > MaxReconcileSkip:
		return errors.New("reconcile: too many rows to skip")
	}
	return nil
}

// RowFailure describes one outbox row whose reconciliation failed with an integrity or
// data error. It carries identifiers and an error class only: never a payload, a URL,
// a value from the database error, or SQL text.
type RowFailure struct {
	OutboxID       string
	EventID        string
	OrganizationID string
	// Class is "sqlstate:<code>", followed by ":<constraint>" when the error names one.
	Class string
}

// ReconcileResult reports one reconciliation batch.
type ReconcileResult struct {
	// Examined is how many unmarked rows the batch selected and locked.
	Examined int64
	// Events is how many of them it marked as fanned out.
	Events int64
	// Deliveries is how many missing deliveries it created.
	Deliveries int64
	// Failed lists the rows that could not be reconciled. They stay unmarked and
	// left no partial deliveries behind.
	Failed []RowFailure
}

type outboxRow struct{ id, orgID, eventID string }

const (
	reconcileSelectHead = `
		SELECT o.id::text, o.organization_id::text, o.event_id::text
		  FROM outbox o
		 WHERE o.fanned_out_at IS NULL`
	reconcileSelectTail = `
		   AND NOT (o.id::text = ANY($3::text[]))
		 ORDER BY o.created_at, o.id
		 LIMIT $4
		   FOR UPDATE OF o SKIP LOCKED`
	// The live lane is a range and the history lane is open-ended below. They are
	// separate statements, not one with an optional bound, so each is a plain range scan
	// of outbox_unfanned_idx (created_at, id) WHERE fanned_out_at IS NULL.
	reconcileSelectLive    = reconcileSelectHead + ` AND o.created_at >= $1::timestamptz AND o.created_at < $2::timestamptz` + reconcileSelectTail
	reconcileSelectHistory = reconcileSelectHead + ` AND o.created_at < $1::timestamptz AND $2::timestamptz IS NOT NULL` + reconcileSelectTail
)

// reconcileRowsSQL creates the missing deliveries for a set of outbox rows and marks them.
var reconcileRowsSQL = `
	WITH batch AS (
		SELECT t.outbox_id::uuid AS outbox_id, t.organization_id::uuid AS organization_id, t.event_id::uuid AS event_id
		  FROM unnest($1::text[], $2::text[], $3::text[]) AS t(outbox_id, organization_id, event_id)
	), created AS (
		INSERT INTO deliveries (organization_id, event_id, destination_id, next_attempt_at)
		SELECT organization_id, event_id, destination_id, $4::timestamptz FROM (` + eligibleDeliveriesSQL(`batch`) + `) due
		ON CONFLICT (event_id, destination_id) DO NOTHING
		RETURNING 1
	), marked AS (
		UPDATE outbox o SET fanned_out_at = $4::timestamptz FROM batch b
		 WHERE o.id = b.outbox_id AND o.fanned_out_at IS NULL
		RETURNING 1
	)
	SELECT (SELECT count(*) FROM marked), (SELECT count(*) FROM created)`

func reconcileRows(ctx context.Context, q database.Querier, now time.Time, rows []outboxRow) (events, deliveries int64, err error) {
	ids, orgs, evs := make([]string, len(rows)), make([]string, len(rows)), make([]string, len(rows))
	for i, r := range rows {
		ids[i], orgs[i], evs[i] = r.id, r.orgID, r.eventID
	}
	err = q.QueryRow(ctx, reconcileRowsSQL, ids, orgs, evs, now).Scan(&events, &deliveries)
	return events, deliveries, err
}

// rowFailureClass classifies a database error that may be isolated to one row. Only
// integrity-constraint (class 23) and data (class 22) errors qualify: they are
// deterministic for the row. Connection loss, cancellation, deadlocks, serialization
// failures, statement timeouts and every other error are transient or global, say nothing
// about one row, and must fail the whole sweep. The class names the SQLSTATE and the
// constraint, never a value.
func rowFailureClass(err error) (class string, isolatable bool) {
	var pg *pgconn.PgError
	if !errors.As(err, &pg) || len(pg.Code) != 5 {
		return "", false
	}
	if pg.Code[:2] != "22" && pg.Code[:2] != "23" {
		return "", false
	}
	class = "sqlstate:" + pg.Code
	if pg.ConstraintName != "" {
		class += ":" + pg.ConstraintName
	}
	return class, true
}

// Reconcile is the safety net behind the message broker. PostgreSQL is the source of
// truth, so an accepted event whose fan-out message was lost, expired or exhausted its
// redeliveries must still be delivered.
//
// It takes up to Limit outbox rows of one lane whose fan-out is not marked
// (fanned_out_at IS NULL), oldest first, creates their missing deliveries by the same
// event-time rule as FanOut, and marks them, all in the caller's transaction. Because the
// marker is the work list, a healthy system has an empty list and a sweep reads nothing
// but the index; and there is no lookback window, so a lost fan-out is repaired however
// old it is, even after JetStream expired the message. The grace period leaves recent
// events to the normal path.
//
// Lanes. The live lane (younger than LiveWindow) and the history lane (older, unbounded)
// are separate queues with separate budgets, so neither can starve the other. A row that
// crosses the boundary between two sweeps moves lanes; within one call both bounds come
// from one Now, so a row is in exactly one lane, and a marked row is in neither.
//
// Rows are locked FOR UPDATE SKIP LOCKED, so concurrent reconcilers take different rows
// and skip a row a FanOut is handling right now; the lock order (outbox row, then delivery
// keys) matches FanOut. Marked rows are never revisited. A row is marked even when no
// subscription was eligible; verifying it again later creates only what is missing under
// the event-time rule, so it is safe.
//
// Failure isolation. The whole batch is first tried as one statement inside a savepoint.
// If that fails with an integrity or data error (SQLSTATE class 22 or 23), the savepoint
// is rolled back and the rows are retried one by one, each in its own savepoint. Healthy
// rows are reconciled; a failing row is reported in Failed, stays UNMARKED (it is never
// presented as fanned out and is never discarded) and leaves no partial delivery. Any
// other error is returned and fails the whole sweep; the caller rolls the transaction back.
//
// The grace period is measured from outbox.created_at, which equals the event's
// received_at: both are the start time of the one ingestion transaction.
func Reconcile(ctx context.Context, tx pgx.Tx, p ReconcileParams) (ReconcileResult, error) {
	if err := p.validate(); err != nil {
		return ReconcileResult{}, err
	}
	skip := p.Skip
	if skip == nil {
		skip = []string{} // a nil slice would be sent as NULL and exclude every row
	}
	var rs pgx.Rows
	var err error
	if p.Lane == LaneLive {
		rs, err = tx.Query(ctx, reconcileSelectLive, p.Now.Add(-p.LiveWindow), p.Now.Add(-p.Grace), skip, p.Limit)
	} else {
		rs, err = tx.Query(ctx, reconcileSelectHistory, p.Now.Add(-p.LiveWindow), p.Now, skip, p.Limit)
	}
	if err != nil {
		return ReconcileResult{}, fmt.Errorf("select unmarked outbox rows: %w", err)
	}
	var rows []outboxRow
	for rs.Next() {
		var r outboxRow
		if err := rs.Scan(&r.id, &r.orgID, &r.eventID); err != nil {
			rs.Close()
			return ReconcileResult{}, fmt.Errorf("scan unmarked outbox row: %w", err)
		}
		rows = append(rows, r)
	}
	rs.Close()
	if err := rs.Err(); err != nil {
		return ReconcileResult{}, fmt.Errorf("select unmarked outbox rows: %w", err)
	}
	res := ReconcileResult{Examined: int64(len(rows))}
	if len(rows) == 0 {
		return res, nil
	}

	// First attempt: the whole batch as one statement, in a savepoint.
	sp, err := tx.Begin(ctx)
	if err != nil {
		return ReconcileResult{}, fmt.Errorf("reconcile savepoint: %w", err)
	}
	events, deliveries, err := reconcileRows(ctx, sp, p.Now, rows)
	if err == nil {
		if err := sp.Commit(ctx); err != nil {
			return ReconcileResult{}, fmt.Errorf("release reconcile savepoint: %w", err)
		}
		res.Events, res.Deliveries = events, deliveries
		return res, nil
	}
	if rbErr := sp.Rollback(ctx); rbErr != nil {
		return ReconcileResult{}, fmt.Errorf("roll back reconcile savepoint: %w", rbErr)
	}
	if _, ok := rowFailureClass(err); !ok {
		return ReconcileResult{}, fmt.Errorf("reconcile deliveries: %w", err)
	}

	// Fallback: one row at a time, so one poison row cannot block the others.
	for _, r := range rows {
		rsp, err := tx.Begin(ctx)
		if err != nil {
			return ReconcileResult{}, fmt.Errorf("reconcile row savepoint: %w", err)
		}
		e, d, rerr := reconcileRows(ctx, rsp, p.Now, []outboxRow{r})
		if rerr == nil {
			if err := rsp.Commit(ctx); err != nil {
				return ReconcileResult{}, fmt.Errorf("release reconcile row savepoint: %w", err)
			}
			res.Events += e
			res.Deliveries += d
			continue
		}
		if rbErr := rsp.Rollback(ctx); rbErr != nil {
			return ReconcileResult{}, fmt.Errorf("roll back reconcile row savepoint: %w", rbErr)
		}
		class, ok := rowFailureClass(rerr)
		if !ok {
			return ReconcileResult{}, fmt.Errorf("reconcile deliveries: %w", rerr)
		}
		res.Failed = append(res.Failed, RowFailure{OutboxID: r.id, EventID: r.eventID, OrganizationID: r.orgID, Class: class})
	}
	return res, nil
}

// OldestUnmarkedAges returns how long the oldest unmarked outbox row of each lane has
// been waiting past the grace period; zero means the lane is empty. A live-lane age that
// keeps growing means reconciliation is not keeping up or a row is failing. A history-lane
// age is expected to be large while the one-time backfill after migration 00018 drains.
// Both queries read only the first entry of outbox_unfanned_idx.
func OldestUnmarkedAges(ctx context.Context, q database.Querier, now time.Time, grace, liveWindow time.Duration) (live, history time.Duration, err error) {
	var oldestLive, oldestHistory *time.Time
	err = q.QueryRow(ctx, `
		SELECT (SELECT min(created_at) FROM outbox WHERE fanned_out_at IS NULL AND created_at >= $1::timestamptz AND created_at < $2::timestamptz),
		       (SELECT min(created_at) FROM outbox WHERE fanned_out_at IS NULL AND created_at < $1::timestamptz)`,
		now.Add(-liveWindow), now.Add(-grace)).Scan(&oldestLive, &oldestHistory)
	if err != nil {
		return 0, 0, fmt.Errorf("oldest unmarked outbox rows: %w", err)
	}
	if oldestLive != nil {
		live = max(now.Sub(*oldestLive), 0)
	}
	if oldestHistory != nil {
		history = max(now.Sub(*oldestHistory), 0)
	}
	return live, history, nil
}
