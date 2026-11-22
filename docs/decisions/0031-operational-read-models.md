# 31. Operational read models: live, organization-scoped and bounded

Date: 2026-11-18

## Status

Accepted. Builds on ADR 24 (lifecycle), ADR 28 (operational reliability) and ADR 30 (failure reasons). Delivery
execution, retry, claim, lease and worker behavior are unchanged: these models only read.

## Context

Operators could see a delivery (`GET /deliveries`) but not the state of the system. The only aggregates were the
worker's gauges (`QueueCounts`, ADR 28), which are global and carry no organization: the metrics registry has no
labels, so a per-organization number cannot be a metric. A tenant could not tell whether its backlog was growing,
whether deliveries were failing, or why.

## Decision

Two read-only endpoints, computed live from PostgreSQL (the source of truth), with no cache, materialized view or
background aggregation. Both are organization-scoped, need `deliveries:read` (OWNER, ADMIN, DEVELOPER and VIEWER,
unchanged) and use the usual `404` for a non-member or a malformed organization id.

### `GET /api/v1/organizations/{organizationId}/operations/queue`

A snapshot at `generatedAt`, taken from one SQL statement (one snapshot, so the numbers agree with each other).

| Field | Meaning |
|---|---|
| `pending` | Every `PENDING` delivery, **delayed ones included**. |
| `due` | `PENDING` with `next_attempt_at <= generatedAt`. |
| `delayed` | `PENDING` with `next_attempt_at > generatedAt`. Counted with its own predicate, never as `pending - due`. |
| `oldestDueAgeSeconds` | Exact age of the longest-waiting due delivery, in whole seconds. `0` when nothing is due. |
| `inFlight` | Every `IN_FLIGHT` delivery, **expired leases included**. |
| `expiredLeases` | `IN_FLIGHT` with `lease_expires_at <= generatedAt`. |
| `capped` | `true` when at least one count reached the cap (below). |

**Due is not claimable.** The claim path (`ClaimDue`) also requires the destination to be `ACTIVE`. A delivery of a
`PAUSED` destination is still due, so a paused destination raises `due` and `oldestDueAgeSeconds` even though no
worker is meant to take it. This is the same meaning the existing `relayforge_delivery_due_pending` and
`relayforge_delivery_oldest_due_age_seconds` gauges have. No paused-destination join and no "blocked" metric exist.

**An expired lease is not a dead worker.** It is a delivery whose lease ran out and that waits for lease recovery
(`recoverBatch`, every `RecoveryInterval`). The worker may be slow, may have been shut down, or may be fine and
about to lose a race it will discard (ADR 24). A short-lived non-zero value is normal; a growing one means recovery
is not running.

### `GET /api/v1/organizations/{organizationId}/operations/deliveries?from=&to=`

Two independent cohorts over one half-open window `[from, to)`, using two different columns:

- `created`: deliveries with `created_at` in the window: `total`, and `byStatus` with the keys `PENDING`,
  `IN_FLIGHT`, `SUCCEEDED`, `FAILED`, `CANCELLED`. These are **current** statuses of deliveries created in the
  window, so a delivery created in the window that failed an hour later is `FAILED` here.
- `completed`: deliveries that reached a terminal status with `completed_at` in the window: `succeeded`, `failed`,
  `cancelled`, `failureRate` and `byFailureReason`. A delivery created last week that failed today counts here, and
  not in `created`.

The two cohorts answer different questions and are not expected to match. Everything is delivery-level; attempt-level
statistics (retries, outcomes, abandoned attempts) are deliberately not mixed in and are not part of this phase.

`from` and `to` are optional RFC 3339 timestamps (offsets are honored, the response is in UTC). `to` defaults to the
service's current time and `from` to `to` minus 24 hours. The window must be positive and at most 31 days (exactly 31
days is allowed). Anything else is `400 validation_failed`. Unknown query parameters are ignored, like on every
other route. The response echoes `from`, `to` and `generatedAt`.

**Normalization.** Explicit and defaulted bounds are treated the same way: converted to UTC and truncated to
microseconds, the precision PostgreSQL stores, so the window the response echoes is exactly the window that was read
(a bound with nanoseconds is truncated, and 31 days plus one nanosecond is therefore exactly 31 days). Both bounds must
fall in the years 0001 to 9999 **in UTC**: RFC 3339 allows year `0000`, but PostgreSQL has no year zero, and an offset can
push a valid local date outside the range (`0001-01-01T00:00:00+14:00` is year 0000 in UTC). Such values are `400
validation_failed` and the deliveries table is never read for them. Authorization still comes first: a caller who is not
a member gets `404` whatever the window is.

**Clock semantics.** `generatedAt` is the **service clock** when the request started (UTC, microseconds), not a
database timestamp, and a defaulted `to` is that same instant: it is the requested endpoint of the window. Every time
comparison of the queue snapshot (due, delayed, expired leases, the oldest due age) uses that one value. `created_at`,
`completed_at` and the delivery timestamps are written by workers and by the database. Two consequences are accepted:
if the service clock and the database clock differ, a window ending "now" can include or miss the most recent
deliveries by that difference, and a delivery created or completed just before `to` by a transaction that commits after
the read is not in that response and can be in a later response for the same window. The newest edge of a window
that ends now is therefore not final; closed windows in the past are stable.

**Failure rate** is `failed / (succeeded + failed)` over the `completed` cohort. Cancelled deliveries (a revoked
destination) are excluded from it and reported separately. It is `null` when the denominator is zero, and `null`
when the `succeeded` or the `failed` count is capped, because a ratio of two lower bounds is not a rate. The field is
always present.

**`byFailureReason`** always has exactly six keys: the five reasons of ADR 30 (`DESTINATION_REJECTED`,
`REDIRECT_REFUSED`, `EGRESS_BLOCKED`, `RETRIES_EXHAUSTED`, `INTERNAL_ERROR`) and `UNRECORDED`. `UNRECORDED` is a
response-only bucket: `FAILED` deliveries with a NULL `failure_reason`, that is, the deliveries that failed before
migration 00019 recorded reasons (ADR 30 does not backfill). It is never stored, never accepted by a `CHECK`
constraint and not part of the closed taxonomy.

### Count cap

Every counter is computed as `SELECT count(*) FROM (SELECT 1 ... LIMIT 100000)`. **What the `LIMIT` bounds, and what
it does not:**

- It bounds the number of **matching rows** each count aggregate receives (and therefore the size of the number it can
  return): at most 100,000.
- It does **not** bound the index entries or heap rows PostgreSQL examines to find them. A counter stops reading once
  it has found 100,000 matches; a counter with few matches keeps reading until the end of its range. A **selective
  bucket** (for example `CANCELLED`, `FAILED`, or one failure reason, in a tenant that is mostly `SUCCEEDED`) can
  therefore examine a substantial part of the tenant's rows in the window, because `status` and `failure_reason` are
  not index columns of the indexes that serve the window.
- The work of a request is bounded in time, not by the cap: the five-second context deadline (below) cancels the
  statement. That is an operational bound, subject to PostgreSQL honoring the cancellation (normally within
  milliseconds, but not instantaneous).

A counter that reaches **100,000 is saturated: it reports 100,000 and means "at least 100,000"**, never an exact count.
The top-level `capped` is `true` when at least one returned count reached the cap. Consequences:

- A capped value is a **lower bound**. A tenant with exactly 100,000 rows also reports `capped: true`; the cap is
  "reached", not "exceeded".
- Counters that did not reach the cap stay exact even when `capped` is `true`.
- Each counter saturates on its own. `delayed` is counted by its own predicate, so it stays exact while `pending` and
  `due` are saturated; the same holds for the status and failure-reason buckets.
- Below the cap the arithmetic holds: `pending = due + delayed`, `created.total = sum(byStatus)`, and
  `failed = sum(byFailureReason)`. At or above it, none of these sums is guaranteed.
- The rate, as above, is `null` when either side is capped. `oldestDueAgeSeconds` is a minimum, not a count, and is
  exact regardless.

## Queries and indexes

One statement per endpoint, built from scalar sub-selects, so each response is one snapshot (PostgreSQL's per-statement
snapshot under READ COMMITTED). Every scan of `deliveries` starts with `organization_id = $1`, takes no row lock
(no `FOR UPDATE`) and never joins another table, so a read cannot block a worker. Status predicates are written as
literals (`status = 'PENDING'`), because a partial index matches only a constant predicate, not a bind parameter.
The window is bound as parameters; nothing from a request is concatenated into SQL.

Migration 00020 (`NO TRANSACTION`, `CREATE INDEX CONCURRENTLY`, idempotent: it drops an interrupted build's leftover
first, and its `Down` drops both indexes) adds two partial indexes:

- `deliveries_org_pending_idx (organization_id, next_attempt_at) WHERE status = 'PENDING'`: pending, due, delayed and
  the oldest due delivery of one organization are an index range scan. The existing `deliveries_due_idx
  (next_attempt_at, id)` has no `organization_id`; without the new index, counting one tenant's backlog walks every
  tenant's pending entries.
- `deliveries_org_completed_idx (organization_id, completed_at) WHERE completed_at IS NOT NULL`: the `completed`
  cohort. Nothing indexed `completed_at` before.

The `created` cohort uses the existing `deliveries_organization_idx (organization_id, created_at DESC, id DESC)`.

**In-flight and expired-lease counters use the global `deliveries_lease_idx`.** That index is
`(lease_expires_at, id) WHERE status = 'IN_FLIGHT'`: it has no `organization_id`, so these counters read it (or a
bitmap with `deliveries_organization_idx`, as the planner decides) **with the organization as a filter**. It is not a
tenant-isolated index. The cost of `inFlight` and `expiredLeases` can therefore grow with the **global** in-flight and
expired-lease population, not only with the tenant's own: during an outage of lease recovery, other tenants' expired
leases are entries these counters may have to step over. This is an **accepted Stage 1 limitation**. The in-flight set
is normally small (workers claim in small batches), and no index is added for it. The scale test
`TestLeaseCountersWithManyInFlightRowsOnOtherTenants` seeds 190,000 in-flight rows on other tenants and records the
plans and timings (see "Measured evidence"); if a deployment shows it matters, an `(organization_id, ...) WHERE status =
'IN_FLIGHT'` index is the next step, with the write overhead of every claim and finalization as its price.

### Repeated scans in the summary

The summary has 15 counters over two ranges: six over the `created_at` window (the total and five statuses) and nine
over the `completed_at` window (succeeded, failed, cancelled and six failure-reason buckets). Each counter scans its
range separately. They cannot share one scan without changing the contract: a single `LIMIT 100000` over the cohort,
aggregated with `FILTER`, would turn every bucket into a lower bound of the **cohort** (a tenant with 150,000
successes would report `failed` as "at least some number" although it has three failures), and the approved semantics
are that each counter saturates independently. Independent caps therefore need independent scans, and the SQL is left as
it is. The cost of the repeated scans is the cost recorded in the evidence below, and the deadline bounds it.

### Measured evidence

The scale tests (`operations_scale_test.go`, `operations_read_model_test.go`; skipped with `-short`) log
`EXPLAIN (ANALYZE, BUFFERS)` plans and wall-clock timings; they assert exact results, never a duration. Run them with
`-v` to read the evidence. They cover the thresholds at exactly 99,999, 100,000 and 100,001 rows, a 250,000-row tenant
with rare buckets over a 30-day window, and tenants with 130,000 and 60,000 in-flight rows beside a small one.

Numbers recorded when this phase was remediated, on one developer machine (PostgreSQL 17, everything in cache, best of
five runs). They are informational, not a performance contract:

| Case | Result |
|---|---|
| Summary, tenant with 250,000 `SUCCEEDED` rows over 29 days plus 33 rare rows (10 `CANCELLED`, 33 `FAILED` with reasons), another tenant with 30,000 rows; window of 30 days | 752 ms (`EXPLAIN ANALYZE` 1,028 ms). PostgreSQL chose sequential scans of `deliveries` for the created and completed counters, because the window covers almost the whole table: the saturated counters stop after 100,000 matches, the rare ones read every row (`Rows Removed by Filter: 93,337` per worker in a parallel scan) |
| Same tenant, window of 1 day / 1 hour | 43 ms / 1.6 ms (index-only scans of `deliveries_organization_idx`) |
| One rare counter alone (`FAILED` and `INTERNAL_ERROR`, 3 matches), 30-day window | 70 ms, parallel sequential scan, 3 rows found after reading the whole tenant |
| Queue, small tenant alone / with 190,000 in-flight rows on other tenants | 0.5 ms / 0.9 ms |
| Queue, tenant with 150,000 history rows and 40 in flight, other tenants holding 190,000 in-flight rows | 52.7 ms. The in-flight counter chose a sequential scan (340,020 rows removed by the filter, 35 ms); the expired-lease counter used `deliveries_lease_idx` and stepped over 120,000 entries of other tenants (16.5 ms) |
| Queue, tenant with 130,000 in flight (saturated) / 60,000 in flight, all expired | 37.5 ms / 31.9 ms |

What this shows: the cost of a summary follows the number of rows in the window, not the cap, and the cost of the
lease counters can follow the global in-flight population (and, when the planner prefers it, the table size). In this
stress data set in-flight rows are more than half of the table; with a realistic share (a tiny fraction) the planner is
expected to use the partial lease index, but that was not measured. These costs are within the five-second deadline at
this size and are not judged unacceptable, so no index was added. Costs were not measured beyond 340,000 rows, and a
linear extrapolation to tens of millions of rows is a reason to revisit, not a measurement.

The claim path is unchanged and keeps its global index: a test shows the due-selection of `ClaimDue` still plans on
`deliveries_due_idx` with the new indexes present, and never on `deliveries_org_pending_idx`, which cannot give the
global `ORDER BY next_attempt_at, id`.

### Index write overhead

The pending index is maintained on the same events as `deliveries_due_idx`: a delivery enters `PENDING` (fan-out
insert, a retry back to `PENDING`) and leaves it (a claim), so the hot path pays about one more index entry per
transition. The completed index adds one entry per delivery, when it reaches a terminal status. Rows, constraints and
triggers are unchanged. **The write overhead of these indexes was not benchmarked.** The extra index maintenance
described here is an architectural estimate derived from which statements touch the indexed columns, not a measured
result; the throughput and latency cost on the claim, retry and fan-out paths is unknown. The cost is accepted because
the alternative is every tenant's queue read scanning the whole system's pending set. Measure it before adding more
indexes; none are added for failed-delivery listings in this phase.

## Safety limits

- The database work of one request has a **five-second deadline** (`app.Config.OperationsDeadline`, zero meaning five
  seconds). When it is exceeded the response is `503 read_model_timeout` ("try a shorter time window"), with no
  database detail. This is an operational bound on the request: the statement is cancelled through its context, which
  needs PostgreSQL to honor the cancellation. A client that disconnects is not a timeout: that error is not mapped to
  503 and is handled like any other unexpected error.
- The window is at most 31 days. The cap bounds the **matching rows** per counter, not the rows examined (see "Count
  cap"); the deadline is what bounds the time.
- There is no per-organization rate limiter in this phase. A heavy read competes for I/O and for one of the API's
  pooled connections (default 10); the deadline, the window limit and the cap are what bound that.
- The worker, claim, lease and retry code do not change, and nothing here writes.

## What is exposed

Counts and timestamps only. No payload, destination name or URL, secret, error text, delivery or event id, claim field
or database detail is returned. Responses are `Cache-Control: no-store` like every API response.

## Why not persist aggregates

A cache, a materialized view or a background rollup would add a second source of truth, a staleness window and a job
to operate, to answer questions PostgreSQL already answers from an index. The measured plans for a small tenant
sharing a table with a very large one are index scans with no table scan. For a large tenant the cap bounds each
counter's result, not its work: a rare bucket over a wide window can examine most of the tenant's window, and the
deadline is the bound. A persisted aggregate becomes justified only if a measured high-volume organization cannot meet the
deadline; hourly rollups would be the next step and are explicitly deferred.

## Consequences

- Operators get per-organization queue health, delivery outcomes and failure causes without new permissions.
- `capped` makes truncation explicit; clients must treat a capped count as a lower bound.
- A paused destination's backlog shows as due. Operators should read `due` together with their destinations' status.
- Redrive (a later phase, implemented in ADR 34) clears `completed_at` and `failure_reason`, so a redriven delivery leaves the `completed`
  cohort and is counted by its current status; attempt history keeps its terminal reasons.

## Not in this phase

Destination health, attempt activity, time series, a queue-age distribution, a recent-failure feed, a failure-reason
filter on the delivery list, indexes for failed-delivery listings, caching and redrive.
