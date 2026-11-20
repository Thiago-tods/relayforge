# 32. Operational failure visibility: recent-failure feed, reason filter and queue-age distribution

Date: 2026-11-18

## Status

Accepted. Phase 4.4.5b. Builds on ADR 30 (failure reasons) and ADR 31 (operational read models). Delivery execution,
retry, claim, lease, recovery and worker behavior are unchanged, and nothing here writes.

## Context

ADR 31 told an operator how many deliveries failed and why, in counts. It could not show **which** deliveries failed,
most recent first, and the delivery list could not be narrowed to one failure reason. It also could not show how long
due work had been waiting beyond the single oldest age. Destination health, attempt activity and time series were
deferred (see "Not in this phase"); this ADR covers the three capabilities that need no new concept: a feed of
current failures, a failure-reason filter on the existing list, and a queue-age distribution.

## Decision

Three read-only additions, all computed live from PostgreSQL, organization-scoped, under the existing session
authentication and the existing `deliveries:read` permission (OWNER, ADMIN, DEVELOPER and VIEWER, unchanged). A non-member
or a malformed organization id gets the usual `404`, **before** any parameter is validated. There is no new role, no new
permission, no write route, no cache and no bulkhead.

### `GET /api/v1/organizations/{organizationId}/operations/failures`

Query parameters, all optional: `limit` (1 to 100, default 50), `cursor`, `failureReason`, `destinationId`, `from`, `to`.

```json
{
  "generatedAt": "2026-11-18T12:00:00.000001Z",
  "from": "2026-11-11T12:00:00.000001Z",
  "to": "2026-11-18T12:00:00.000001Z",
  "items": [{
    "deliveryId": "...", "eventId": "...", "destinationId": "...", "destinationName": "billing-hook",
    "failureReason": "RETRIES_EXHAUSTED", "attemptCount": 8,
    "lastHttpStatus": 503, "lastErrorClass": "http_5xx",
    "createdAt": "2026-11-18T10:00:00Z", "completedAt": "2026-11-18T11:30:00Z"
  }],
  "nextCursor": "djE6MTc2MzQ2..."
}
```

- **A current failure** is a delivery with `status = 'FAILED'` and a `completed_at` inside the window. A delivery that a
  future redrive moves back out of `FAILED` (status, reason and completion time are cleared together, ADR 30) is no
  longer a current failure and leaves the feed by itself; if it fails again it returns with its new completion time.
  A test simulates that row-level effect inside a rolled-back transaction.
- **Window.** Half-open `[from, to)` on `completed_at`. The default is the **last seven days** and the longest is **31
  days**. Bounds go through the same normalization and validation as ADR 31 (`operationsWindow`: RFC 3339, UTC, truncated
  to microseconds, years 0001 to 9999 (UTC), `from < to`); only the default span differs. The response echoes the
  window as it was read.
- **Order and pagination.** `completed_at DESC, id DESC`, with **keyset** pagination: never an offset and never a count.
  A page reads `limit + 1` rows to learn whether another page exists; `nextCursor` is `null` on the last page. `items` is
  always an array.
- **The cursor** is opaque base64url (no padding) of `v1:<unix microseconds>:<delivery id>`: the `(completed_at, id)` of
  the last item. It carries a **position and nothing else**. It is not a capability and it carries no authorization state:
  `organization_id = $1` is applied to every query whatever the cursor says, so a cursor copied from another tenant can
  only move a position inside the caller's own rows. Parsing is strict (canonical base64url, exact shape, a timestamp
  in years 0001 to 9999, a lowercase UUID, and re-encoding must give the same text), and a malformed or altered cursor is
  `400 validation_failed`, never a `500`.
- **`failureReason`** is one of the five persisted reasons or `UNRECORDED`. **`UNRECORDED`** is the response-only
  value for a `FAILED` delivery that failed before ADR 30 and has a NULL `failure_reason` (the same bucket the summary
  uses). The feed **always** renders the field: a legacy NULL is `"UNRECORDED"`, never `null` and never omitted (D9).
  The filter value `UNRECORDED` means `failure_reason IS NULL`.
- **`destinationId`** must be a UUID. A destination of another organization simply matches nothing, whoever asks.
- **Only operational metadata.** No payload, no destination URL, no signing secret, no raw error text, no claim token,
  worker identifier or lease data. `lastErrorClass` is a closed machine class (the schema forbids free text from a
  destination), and `destinationName` is already visible under `deliveries:read`. All timestamps are UTC.

**Eventual consistency.** One page is one statement and so one snapshot. Across pages the feed is **eventually
consistent**: `completed_at` is stamped by the worker clock when it finishes a delivery, so a delivery that commits late can
carry a `completed_at` older than rows a client has already read. A continuation from a cursor never revisits that
part of the order. A client that wants the newest failures starts again from the first page and polls it. A test
(`TestRecentFailureFeedLateCommitCaveat`) documents exactly this.

### Failure-reason filter on `GET /api/v1/organizations/{organizationId}/deliveries`

An optional `failureReason` with the same six values. The change is additive and backward compatible:

- Without the parameter (or with an empty one) the two statements of the list are **character-for-character the ones
  that shipped before**, the response is byte-identical and there is no deadline. A unit test compares the SQL text and
  an HTTP test compares response bytes.
- A reason **implies `status = 'FAILED'`**. `status=FAILED` together with a reason is accepted; any other `status` is
  `400 validation_failed` (D5). `UNRECORDED` means `status = 'FAILED' AND failure_reason IS NULL`. An unknown value
  (including a different case) is `400`.
- **Authorization first.** For a request with a non-empty `failureReason`, the handler passes the raw query to the service
  (`ListDeliveriesParsed`), which authorizes membership and `deliveries:read` with one query and only then reads
  `limit`, `offset`, `from` and `to` and validates them. A non-member, or a malformed or unknown organization id, gets the
  same `404` whatever the parameters say (the response bytes are identical to those for a valid request), and a member gets
  `400` for the same input. A request **without** `failureReason` is deliberately unchanged, including the order in which
  it has always validated (pagination and timestamps, then authorization) and its bytes.
- Ordering (`created_at DESC, id DESC`), offset pagination, the response shape, the **exact `total`** (no silent cap)
  and the existing omission of a NULL `failureReason` from list items are all unchanged (D6, D9).
- **Only a filtered request runs under the five-second operations deadline** (below). The reason clause is one of six
  constants chosen after validating the value against the closed taxonomy, so client text never reaches the SQL.

### `GET /api/v1/organizations/{organizationId}/operations/queue/age`

The distribution of how long **due** deliveries have been overdue, in seven fixed buckets (D7):

| Label | Minimum (inclusive) | Maximum (exclusive) |
|---|---:|---:|
| `<1m` | 0 | 60 |
| `1-5m` | 60 | 300 |
| `5-15m` | 300 | 900 |
| `15-60m` | 900 | 3600 |
| `1-6h` | 3600 | 21600 |
| `6-24h` | 21600 | 86400 |
| `>24h` | 86400 | none (`maxSeconds` is `null`) |

```json
{ "generatedAt": "2026-11-18T12:00:00Z", "capped": false,
  "buckets": [ {"label": "<1m", "minSeconds": 0, "maxSeconds": 60, "count": 12}, "..." ] }
```

- **Overdue age is `generatedAt - next_attempt_at`**, over `PENDING` deliveries with `next_attempt_at <= generatedAt`.
  It is **time overdue, not age since creation**: a retried delivery's clock restarts when its retry becomes due. A due
  delivery of a **paused** destination is counted (due does not mean claimable, as in ADR 31). Future deliveries are
  excluded. Buckets are half-open and contiguous; the last one takes every age of 86,400 seconds or more.
- **One statement, one snapshot, one instant.** The seven counters are independent scalar sub-selects of one SQL
  statement, all compared with the same bind parameter, the **service clock** at the start of the request (UTC, microseconds),
  which is also `generatedAt`. There is no destination join and no database clock in the statement.
- **Independent saturation.** Each bucket is `count(*)` over a subquery with `LIMIT 100000`, like every ADR 31 counter,
  and saturates on its own; `capped` is `true` when at least one bucket reached 100,000, and a capped bucket means "at
  least 100,000". The same caveat applies as in ADR 31: the `LIMIT` bounds matching rows, not rows examined (a bucket is
  an index range, so here the two are close), and the deadline bounds the time. At exactly 99,999, 100,000 and 100,001
  rows in one bucket the others stay exact.
- The buckets add up to `due` of the queue snapshot taken at the same instant while nothing is capped.

## Queries and indexes

Both new statements put `status = 'FAILED'` or `status = 'PENDING'` and every other predicate as constants or bind
parameters, always with `organization_id = $1`, so partial indexes can match.

- **Feed:** `... WHERE d.organization_id = $1 AND d.status = 'FAILED' AND d.completed_at >= $2 AND d.completed_at < $3
  [AND reason] [AND destination] [AND (d.completed_at, d.id) < ($, $)] ORDER BY d.completed_at DESC, d.id DESC LIMIT n+1`,
  joined to `destinations` (by id **and** organization) for the name. Every optional clause is appended only when its
  parameter is present, so the planner sees plain range and equality conditions (not `$n IS NULL OR ...`), and all
  values are bind parameters.
- **Queue age:** seven counters over `deliveries_org_pending_idx (organization_id, next_attempt_at) WHERE status =
  'PENDING'` (ADR 31, no new index), each a range of `next_attempt_at`.

### Migration 00021: `deliveries_org_failed_idx`

```sql
CREATE INDEX CONCURRENTLY deliveries_org_failed_idx
    ON deliveries (organization_id, completed_at DESC, id DESC) WHERE status = 'FAILED';
```

**The index was added because the evidence required it (D4), not by default.** The gate, fixed before measuring: the
feed must not scan the table sequentially, and a first page may discard at most 20,000 rows. Before the index, with
only `deliveries_org_completed_idx`, which knows nothing about the status, the feed had to read every completed delivery
of the window, successes included, and discard those that had not failed. Local test evidence (below) shows that
it failed the gate in **all five** representative cases, including a sequential scan of the table. After the
index every case passes with no sequential scan and (almost) no discarded row. The migration follows 00020's
conventions: `-- +goose NO TRANSACTION`, `CREATE INDEX CONCURRENTLY` preceded by `DROP INDEX CONCURRENTLY IF EXISTS`
(an interrupted build leaves an INVALID index, which the next run replaces), idempotent `Up` and a reversible `Down`.
Tests cover up/down/up, a real INVALID index (a unique concurrent build over duplicate rows), a leftover index with another
definition, a rerun of a finished migration (the index is not rebuilt) and the catalog state after every step.

**Write overhead was not benchmarked.** The index holds one entry per delivery that ends as `FAILED`, the rare outcome,
and nothing for successes, retries, claims or cancellations. That statement is an **architectural estimate** derived from
which statements touch the indexed rows. It is not a measured result, and the throughput and latency cost on the
terminal write path is unknown. It must be measured before further indexes are added on that path (the future
destination-health index in particular is not part of this phase).

The same index also serves the filtered delivery list: its `status = 'FAILED'` literal matches the partial index, so a
filtered count or page reads only failures (see the evidence), even though the list stays ordered by `created_at`.

### Measured evidence

Recorded when this phase was implemented, on one developer machine (PostgreSQL 17, everything in cache, a single run, no
tuning). **These are local test evidence, not production performance, and the tests assert results and plan shapes, never durations.**
`TestRecentFailureFeedPlansAtScale` seeds two tenants of 300,000 completed deliveries (spread randomly over 30
days): tenant A with **0.1%** failures (300, 20 of them legacy rows with no reason) and tenant B with **0.001%** (3). It
runs the production statement (captured through a recording `Querier`, so the test cannot drift from the code),
logs `EXPLAIN (ANALYZE, BUFFERS)`, and asserts correctness, no `Seq Scan on deliveries` and at most 20,000 filtered rows.

| Case (first page, limit 50) | Without the failed index | With `deliveries_org_failed_idx` |
|---|---|---|
| 0.1% failures, 7 days | bitmap scan of the completed index, 70,150 rows discarded, 18.4 ms | index scan, 0 discarded, 0.08 ms |
| 0.1% failures, 31 days | parallel sequential scan, about 200,000 rows discarded per worker, 83 ms | index scan, 0 discarded, 0.08 ms |
| 0.1%, rare reason (`UNRECORDED`), 31 days | sequential scan, about 200,000 discarded per worker, 161 ms | index scan, 280 failures filtered, 0.13 ms |
| 0.001% failures, 7 days | bitmap scan, 69,971 discarded, 20.6 ms | index scan, 0.02 ms |
| 0.001% failures, 31 days | sequential scan, about 200,000 discarded per worker, 164 ms | index scan, 0.03 ms |

The "without" column is the plan of the first run; the planner is free to choose differently on another run (a
repeated run chose an ordered index scan that discarded about 51,000 rows over 31 days). Either way it breaches the gate, and
the repeatable test `without the failed index the feed reads the whole window` drops the index inside a transaction that
is rolled back and asserts exactly that.

Filtered delivery list (exact total kept): with the index, count and page of `RETRIES_EXHAUSTED` (280), `UNRECORDED` (20)
and a reason with no rows (0) each ran in 0.1 to 0.7 ms, reading only failures.

Queue age, 100,001 rows in one bucket and 100,000 in another (plus smaller buckets): one `Result` node with seven
`InitPlan`s, each an index scan of `deliveries_org_pending_idx`, no sequential scan of `deliveries` (the test asserts both),
37 ms. At realistic queue sizes it is a handful of short index range scans.

The claim path is unchanged: `ClaimDue` still plans on the global `deliveries_due_idx` and ignores the organization
indexes, and still claims (`TestQueueAgeAndFeedLeaveTheClaimPathAlone`).

## Safety limits

- The database work of the feed, the queue-age distribution and a **filtered** delivery list has the same **five-second
  deadline** as ADR 31 (`app.Config.OperationsDeadline`, zero meaning five seconds). An overrun is `503 read_model_timeout`
  with a fixed safe message and no database detail. A client that goes away (`context.Canceled`) is **not** a timeout
  and is handled like any other unexpected error, as in ADR 31. The unfiltered delivery list is untouched and has no
  deadline (D6).
- There is no bulkhead and no concurrency limit on operational reads (D8). Each request holds one pooled connection for at
  most the deadline; the page size (100), the window (31 days) and the deadline are what bound the cost.
- `limit` is validated after authorization (1 to 100), never truncated silently. A feed page never counts anything.
- The window and the timestamp rules are those of ADR 31 and are validated before the table is read, which a test
  proves with the table locked.

## Consequences

- Operators can answer "which deliveries failed, and why, most recent first" and "how long has due work been waiting"
  without leaving the API, and can narrow the delivery list to one reason.
- `capped` and the deadline stay the honest bounds: nothing here counts rows past the cap or hides a truncation.
- One more partial index on the terminal write path. Its write cost is estimated, not measured.
- The feed is a live tail, not a ledger: it is eventually consistent across pages, and a redriven delivery leaves it.
- The unfiltered list gains nothing and loses nothing.

## Not in this phase

Destination health (facts only, no score or label, with delivery-level consecutive failures), the cross-delivery attempt
activity feed, time-series delivery metrics, rollups and any bulkhead on operational reads. Destination health will need
its own index on the terminal write path and a benchmark of that overhead first. A persisted aggregate is justified only
if a measured high-volume organization cannot meet the deadline with live queries.
