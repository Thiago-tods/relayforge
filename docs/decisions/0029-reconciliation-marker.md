# 29. Reconciliation works from a durable marker, in two lanes, with per-row failure isolation

Date: 2026-11-18

## Status

Accepted. Replaces the lookback-window reconciliation of ADR 25. Event-time eligibility (ADR 26) is unchanged.

## Context

`Reconcile` repaired lost fan-out work by examining every event received between 48 hours and 2 minutes ago
and probing `deliveries` for each. Two problems, both measured on PostgreSQL 17 with 1,000,000 events over
30 days (everything healthy, nothing missing):

- **Cost proportional to the window, not to the work.** A healthy sweep read 66,619 events and probed
  `deliveries` 66,619 times: 290 ms, about 223,000 buffers, every minute, in every consumer process, to find
  nothing.
- **A hole.** An event whose fan-out was lost and which was older than 48 hours was never repaired. With 200
  lost fan-outs (100 inside the window, 100 older) the old query found 100.

A review of the first marker-based version found three further concerns, addressed below: historical rows
queued in front of recent losses, one failing row blocking all reconciliation, and no database guarantee
that every event has an outbox row.

## Decision

### The marker

`outbox.fanned_out_at timestamptz` (migration 00018) is the work list.

- **Meaning.** `NULL`: the deliveries of this event have not been confirmed. Non-`NULL`: the transaction that
  created them committed at that instant. It means "fan-out ran", not "deliveries exist": an event with no
  eligible subscription is marked too.
- **Who writes it.** `delivery.FanOut` and `delivery.Reconcile`, in the same transaction that inserts the
  deliveries. They commit or roll back together, so the marker cannot be set without the deliveries, and cannot
  be missing after they committed.
- **Order of locks.** `FanOut` marks first (`UPDATE ... WHERE fanned_out_at IS NULL`), then inserts. `Reconcile`
  locks the outbox row first (`FOR UPDATE SKIP LOCKED`) and inserts afterwards. Both take the outbox row and
  then the delivery keys, so there is no lock-order inversion. (A stress test with the opposite order did not
  provoke a deadlock either, so this is a precaution, not the fix for an observed failure.)
- **Grace** is measured from `outbox.created_at`, which equals `webhook_events.received_at`: both are the start
  time of the one ingestion transaction (`ingest.Service`).

| Situation | Outcome |
|---|---|
| Crash before commit | Deliveries and marker roll back together. The message is redelivered, or reconciliation takes the row after the grace period. |
| Crash after commit, before the JetStream ack | The message is redelivered; `FanOut` finds the marker set, keeps its first timestamp, inserts nothing (`ON CONFLICT DO NOTHING`). |
| Deliveries inserted but the marker update fails | Cannot happen separately: one transaction. A failing marker update fails the whole fan-out and the message is retried (tested with an injected failure). |
| Marker set but the transaction rolls back | The marker rolls back too and the row is unmarked again (tested, including a failure after the marker was written). |
| Old, unpublished or unmarked event | Reconciled after the grace period regardless of age or of whether the publisher ran. |
| Concurrent reconcilers | `SKIP LOCKED` gives them disjoint rows. A row a `FanOut` holds is skipped. |
| Concurrent `FanOut` and `Reconcile` on one event | Both are idempotent; the unique key allows one delivery per (event, destination). |

The 48-hour lookback is gone. That is safe because the marker, not the clock, decides what is outstanding, and
because every reconciled row is marked (even with zero deliveries), so the unmarked set drains and is not
rescanned.

### Two lanes

After migration 00018 every historical outbox row is unmarked. In one oldest-first queue a fan-out lost today
would wait behind all of them: with 1,000,000 historical rows, 999,758 rows were ahead of the newest loss, about
100 sweeps at 10,000 rows per sweep.

Reconciliation therefore has two lanes, each its own bounded queue with its own batch budget per sweep:

- **Live lane**: unmarked rows older than the grace period and younger than `LiveWindow` (24 hours, the stream's
  `MaxAge`: older than that the broker cannot help any more).
- **History lane**: unmarked rows older than `LiveWindow`, with no upper age limit, so an event of any age is
  still recovered.

Defaults: 500 rows per batch and 10 batches per lane per sweep, so a sweep reads at most 10,000 rows, the same
total as before. The two lanes run in one sweep, each with its budget, so neither starves the other. Both use
the same `FOR UPDATE SKIP LOCKED` statement shape with a range predicate on `outbox_unfanned_idx`, so locking,
idempotency and ADR 26 eligibility are unchanged.

A row that crosses the 24-hour boundary between sweeps moves from the live to the history lane. Within one call
both bounds come from one clock reading (`>=` the boundary is live, `<` is history), so a row is in exactly one
lane, and a marked row is in neither: it is neither lost nor processed twice (tested with concurrent reconcilers
reading different clocks around the boundary).

Limitation: rows that were less than 24 hours old when the migration ran still share the live lane with
genuinely lost rows. That is at most one day of traffic, and it drains at 5,000 rows per sweep (about 7 sweeps for
the 33,318 such rows in the measured data set). Telling them apart would require writing to historical rows,
which the migration deliberately avoids.

### Failure isolation

One deterministic failure used to fail the whole batch statement, so the same batch was selected and failed
again every sweep and no younger row was ever reconciled. Now:

1. The batch is tried as one statement inside a savepoint.
2. If it fails with an integrity or data error (SQLSTATE class 23 or 22), the savepoint is rolled back and the
   rows are retried one at a time, each in its own savepoint.
3. A row that still fails is reported in `ReconcileResult.Failed`. It stays **unmarked**: it is never presented as
   fanned out, never discarded, and leaves no partial delivery (the savepoint rolled it back). Healthy rows in
   the batch are reconciled.
4. The consumer skips the failed rows for the rest of the sweep (so the next batch does not select them again),
   counts them, logs them, and tries them again in the next sweep. When the cause is fixed, the next ordinary
   sweep recovers them with no operator action.

Only class 22 and 23 errors are isolated, because only they are deterministic for the row. Connection loss,
context cancellation, deadlocks (40P01), serialization failures (40001), statement timeouts (57014), lock timeouts
and every other error fail the whole sweep and roll its transaction back, exactly as before.

Bounds: a batch that fails costs one set-based attempt plus at most `Limit` single-row statements (measured at
443 ms for a batch of 500 with one poison row, comparable to the 381 ms of a successful batch of 500). A sweep
stops once `ReconcileMaxRowFailures` (100) rows have failed in it. So retries per sweep are bounded by the batch
budget, and across sweeps by the sweep interval.

There is no dead-letter table: the unmarked outbox row is the dead letter, and it is visible in the logs, the
counter and the age gauge below.

### Diagnostics

A failed row is reported with its outbox, event and organization ids, the lane, and an error class of the form
`sqlstate:<code>[:<constraint>]`. Payloads, secrets, URLs, values from the database error message and SQL text
are never logged. At most 10 failed rows are logged per sweep, followed by one summary line with the totals.

| Metric | Meaning |
|---|---|
| `relayforge_reconcile_row_failures_total` | Rows that could not be reconciled (isolated). |
| `relayforge_reconcile_errors_total` | Whole sweeps that failed (not row-specific). |
| `relayforge_reconciled_events_total` | Rows examined and marked, including backfill. |
| `relayforge_reconcile_oldest_live_unmarked_age_seconds` | Age of the oldest unmarked live-lane row. **Alert on this**: it should be near zero, and keeps growing while rows fail or reconciliation falls behind. |
| `relayforge_reconcile_oldest_history_unmarked_age_seconds` | Same for the history lane; large while the one-time backfill drains, so do not alert on it during that period. |

Two gauges rather than one, because a single age would be permanently large while the backfill drains and could
not be used as an alarm.

### Event and outbox integrity

Every `webhook_events` row must have an `outbox` row, because reconciliation works from outbox rows. Ingestion
writes both in one transaction (`ingest.Service.persist`), and no other production code writes either table.
This is kept by the transaction boundary, by `TestOutboxFailureRollsBackTheEvent` and
`TestNoCommittedEventLacksItsOutboxRow` (accepted, duplicate, conflict, rejected signature and injected failure),
and by an architecture guard test that fails if `INSERT INTO webhook_events` or `INSERT INTO outbox` appears in
any production file other than `internal/ingest/store.go`.

A database constraint (a deferred constraint trigger) was considered and not adopted: it adds a lookup to every
insert on the hottest write path and would require changing every schema test that inserts events directly,
to protect against a second writer that does not exist. Add the constraint, and update the guard, if a second
writer ever appears (for example a bulk import or an archive replay).

### Backfill is bounded

The migration does **not** update existing rows. Historical rows are verified by the two lanes at the bounded
rate above. Verifying a row whose deliveries already exist inserts nothing and marks it, and applies the same
event-time rule, so it never grants a subscription created later an earlier event (tested). The migration is
safe on a large live table: `ADD COLUMN` without a default is a catalog change, and the index is created
`CONCURRENTLY` (the file declares `NO TRANSACTION`). Every statement is idempotent, so an interrupted run, which
can leave an INVALID index, is repaired by running it again (tested).

## Measurements

PostgreSQL 17 on one machine, 1,000,010 events, deliveries and outbox rows over 30 days, `VACUUM ANALYZE`d,
`EXPLAIN (ANALYZE, BUFFERS)`. They show the shape (work follows outstanding rows, not history), not a
service-level figure.

| Case | Old window query | Marker version |
|---|---|---|
| Healthy, nothing missing | 290.6 ms, about 223,000 buffers, 66,619 events read | live select 0.031 ms (2 buffers) + history select 0.028 ms (1 buffer) + both age queries 0.195 ms (11 buffers) |
| 200 lost (100 inside 48 h, 100 older) | finds 100 | repairs 200 (single statement of the first version: 61.8 ms, includes writing them) |
| Post-migration, all 1,000,210 rows unmarked: select 500 rows | n/a | live lane 9.0 ms, 1,507 buffers; history lane 8.2 ms, 1,505 buffers |
| Post-migration: age queries | n/a | 0.051 ms, 8 buffers |
| Post-migration: process one batch of 500 (unnest form used by the code) | n/a | 380.6 ms, about 14,500 buffers (the first version's single statement measured 414.9 ms in a different cache state; no improvement is claimed) |
| Poison row in a batch of 500, in the integration test | blocks every sweep | 443 ms: reconciles 499, reports 1 |

Queue position of a fan-out lost 10 to 30 minutes ago, 1,000,000 unmarked historical rows: 999,758 rows ahead in
one queue; in the lane design it is behind only the live lane (33,318 rows in this data set), which drains at
5,000 rows per sweep.

## Consequences

- Lost fan-out is repaired whatever its age, a healthy system does no reconciliation work, and a historical
  backlog cannot delay recent losses.
- `delivery.Reconcile` takes a `pgx.Tx` (it needs savepoints) and `ReconcileParams`, and returns a
  `ReconcileResult`; `relay.ConsumerConfig` replaces `Lookback` with `LiveWindow`, per-lane batch budgets and
  `ReconcileMaxRowFailures`.
- Outbox rows are updated once more (the marker), which rewrites the tuple. Rows are never deleted, so the
  outbox table grows with history; retention is a separate topic.

## Limitations and risks

- Rows younger than 24 hours at migration time share the live lane with real losses (see above).
- A row whose reconciliation always fails is retried once per sweep, forever, and costs up to one single-row
  statement per row of its batch each time. It no longer blocks others, but a flood of failing rows
  (100 per sweep) means something systemic; the counter and the live-lane age gauge are the alarms. Back-off
  state per row (new columns) was not added because it is only needed if hundreds of rows can fail at once.
- If more than `ReconcileMaxRowFailures` rows fail in a sweep, the rest of the sweep is skipped, including the
  history lane.
- Without a database constraint, an event without an outbox row would never be reconciled; the guard test and
  the ingestion tests are the protection.
- The marker is not a delivery receipt. It says fan-out ran, not that anything was sent.
