# 28. Operational reliability: correlation, claim loop, shutdown and readiness

Date: 2026-11-18

## Status

Accepted. Builds on ADR 24 (delivery lifecycle), ADR 25 (relay and reconciliation) and ADR 27. This is the
first part of Phase 4.4; the schema and API stages (failure reason, operational read model, redrive,
per-destination limits) are separate decisions.

## Context

An audit of the delivery path found no defect in the lease, fencing or recovery logic, but several
operability gaps:

- No request identifier or access log: an API error could not be tied to a log line.
- The claim loop slept a whole `PollInterval` after a full batch whenever every slot was busy, which capped
  a worker near `Concurrency` deliveries per interval regardless of destination speed. A test reproduced it:
  12 of 40 fast deliveries finished in 6 s with a 2 s interval.
- A request interrupted by shutdown was left `IN_FLIGHT` until its lease expired (2 min) and the next
  sweep ran (30 s), so each rolling deploy delayed in-flight work by up to 2.5 minutes.
- `ShutdownGrace` (20 s) plus the result write (15 s) could exceed `ShutdownTimeout` (15 s).
- Readiness checked PostgreSQL only: a wedged claim loop or a lost NATS connection looked healthy.
- Gauges were counts; none showed how long work had been waiting. `QueueCounts` scanned the whole
  deliveries table, which is never deleted from.
- Wrapped database errors were logged whole, and PostgreSQL error details repeat the offending values.

## Decision

**Correlation.** Every response carries `X-Request-Id`. A caller's id is kept only if it is 8 to 64 characters
of `A-Za-z0-9._-`; otherwise a random 128-bit id replaces it, so a client cannot inject log lines. One access
line per request logs the route pattern, not the path, so identifiers and tokens in URLs never reach the log
and cardinality stays bounded; the query string, headers and body are never logged. Failed requests log the
same id. Unclassified errors are logged through `SafeError`, which keeps the SQLSTATE and constraint name of a
database error and drops its detail.

**Claim loop.** After a full batch with no free slot the loop waits for a slot to free (or the poll interval)
instead of sleeping the whole interval. An empty or short claim still waits `PollInterval`.

**Interrupted requests.** Shutdown records the attempt immediately as `ABANDONED` with error class
`shutdown_interrupted` and makes the delivery due again after 5 s. The attempt still counts toward
`max_attempts`: the request may have reached the destination, and the attempt number must stay consistent with
what the destination might have seen. This is the same price lease recovery charges, paid in seconds instead of
minutes. If the write fails, lease recovery remains the fallback.

**Shutdown budget.** The result write is bounded by `FinalizeTimeout` (5 s). At start-up the worker's grace is
reduced to fit `ShutdownTimeout - FinalizeTimeout`, with a warning. The default grace is 10 s. The worker and the
HTTP drain run concurrently, so the budget is the larger of the two, not their sum.

**Readiness.** A `Heartbeat` records progress of each loop. `/readyz` reports `delivery_worker`
(claim and maintenance loops), `outbox_publisher`, `fanout_consumer` and `nats`. Limits are generous multiples
of each loop's interval with a floor, so a slow database is not reported as a stalled loop.

**Metrics.** Two age gauges (oldest due delivery, oldest unpublished outbox record) show stuck work, and
`relayforge_db_pool_*` gauges show pool saturation. `QueueCounts` names the status in every subquery so
PostgreSQL uses the partial indexes. A process has one registry and one `/metrics` route.

## Consequences

- Behaviour change: an interrupted request is retried in about 5 seconds instead of about 2.5 minutes, and a
  deploy that interrupts a request consumes one of its attempts either way.
- The default shutdown grace is shorter. A request still running after 10 s of shutdown is interrupted and
  retried, which is now cheap.
- Readiness can fail for reasons other than the database. An orchestrator that restarts on readiness will
  restart a wedged worker, which is the intent.
- Not changed here, and recorded as follow-ups: lease times still come from the application host clock;
  `Reconcile` still has no index on `webhook_events.received_at`; no per-destination concurrency limit,
  no organization fairness in the claim order, and no replay of FAILED deliveries.
