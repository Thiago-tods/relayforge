# 33. Attempt activity feed

Date: 2026-11-21

## Status

Accepted. Phase 4.4.7b. Builds on ADR 24 (delivery lifecycle), ADR 30 (terminal reasons), ADR 31 and ADR 32 (operational
read models). Delivery execution, retry, claim, lease, recovery and worker behavior are unchanged, and nothing here writes.
There is **no migration and no new index**.

## Context

ADR 32 showed which deliveries are failing now. It could not show what the workers have been doing: the attempts that were
made, across deliveries, most recent first, including attempts that did not end a delivery (retries, and attempts
abandoned when a lease expired or a worker shut down). The attempts of one delivery are already part of the delivery
detail response; there was no cross-delivery view. ADR 32 deferred it.

## Decision

One read-only endpoint, computed live from PostgreSQL, organization-scoped, under the existing session authentication and
the existing `deliveries:read` permission (OWNER, ADMIN, DEVELOPER and VIEWER, unchanged). A non-member or a malformed
organization id gets `404` **before** any parameter is validated. There is no new role or permission, no write route, no
cache, no rollup and no bulkhead.

### `GET /api/v1/organizations/{organizationId}/operations/attempts`

Query parameters, all optional: `limit` (1 to 100, default 50), `cursor`, `outcome`, `deliveryId`, `from`, `to`.

```json
{
  "generatedAt": "2026-11-21T12:00:00.000001Z",
  "from": "2026-11-20T12:00:00.000001Z",
  "to": "2026-11-21T12:00:00.000001Z",
  "items": [{
    "attemptId": "...", "deliveryId": "...", "eventId": "...",
    "destinationId": "...", "destinationName": "billing-hook",
    "attemptNumber": 3, "startedAt": "2026-11-21T11:59:58.100Z", "finishedAt": "2026-11-21T11:59:58.350Z",
    "durationMs": 250, "outcome": "HTTP_ERROR", "httpStatus": 503, "errorClass": "http_5xx"
  }],
  "nextCursor": "YTE6MTc2Mz..."
}
```

- **Fields are stored fields only.** Nothing is derived: there is no `endsDelivery` or similar flag. `httpStatus`,
  `errorClass` and `terminalReason` are omitted when NULL. `terminalReason` is set only on an attempt that ended a failure episode of its delivery
  (it made the delivery `FAILED`, ADR 30). A delivery that was redriven (ADR 34) and failed again has one such attempt per
  failure episode. `errorClass` is the bounded identifier the worker records, never a raw error message.
- **Window.** Half-open `[from, to)` on `started_at`. The default is the **last 24 hours** (attempts are far more numerous
  than failed deliveries, so the default is shorter than the failure feed's seven days) and the longest is **31 days**. Bounds
  use the same normalization and validation as ADR 31 and 32 (RFC 3339, UTC, microseconds, years 0001 to 9999, `from < to`).
- **Order and pagination.** `started_at DESC, id DESC` with **keyset** pagination: never an offset and never a count. A page
  reads `limit + 1` rows to learn whether another page exists. `nextCursor` is `null` on the last page and `items` is always
  an array.
- **Cursor.** Opaque base64url (no padding) of `a1:<unix microseconds>:<attempt id>`. The `a1` tag differs from the failure
  feed's `v1`, so a cursor of one feed is refused by the other (`400`). It carries a **position and nothing else**: the
  organization predicate applies to every query whatever the cursor says, so another tenant's cursor can only move a position
  inside the caller's own rows. Parsing is strict (canonical base64url, exact shape, a timestamp in years 0001 to 9999, a
  lowercase UUID, and re-encoding must give the same text); a malformed or altered cursor is `400 validation_failed`.
- **Filters.** `outcome` is one of the seven recorded outcomes (`SUCCESS`, `HTTP_ERROR`, `REDIRECT`, `NETWORK_ERROR`,
  `TIMEOUT`, `BLOCKED_BY_POLICY`, `ABANDONED`), exact upper case. `deliveryId` must be a UUID; a delivery of another
  organization matches nothing. **There is no `destinationId` filter in this version**: an attempt row has no destination
  column, so the filter would join through `deliveries` and scan a time-ordered index for a rare destination. It is deferred.
  An unknown query parameter, `destinationId` included, is ignored, as everywhere else in the API.
- **`ABANDONED` attempts are in the feed.** They are recorded when a lease expires or a worker is interrupted
  (`lease_expired`, `destination_inactive`, `secret_unavailable`, `shutdown_interrupted`) and consume an attempt number even
  when no request was sent. An `ABANDONED` attempt says a worker did not finish, **not** that the destination failed; read
  `outcome` before drawing conclusions about a destination.
- **Only operational metadata.** No payload, response body, destination URL, signing secret, raw error, claim token, worker
  identifier or lease data. `destinationName` is already visible under `deliveries:read`. All timestamps are UTC.

**Eventual consistency.** One page is one statement and so one snapshot. Across pages the feed is a live tail:
`started_at` is stamped by the worker clock (or, for an attempt recorded by lease recovery, taken from the claim time), so an
attempt that commits late can carry a `started_at` older than rows a client has already read, and a continuation from a
cursor never revisits that part of the order. A client that wants the newest attempts starts again from the first page.
Attempts are append-only and are never unfinished rows, so a row, once visible, never changes.

## Query and plan

```sql
SELECT a.id, a.delivery_id, d.event_id, d.destination_id, x.name, a.attempt_number, a.started_at, a.finished_at,
       a.duration_ms, a.outcome, a.http_status, a.error_class, a.terminal_reason
  FROM delivery_attempts a
  JOIN deliveries d   ON d.id = a.delivery_id AND d.organization_id = a.organization_id
  JOIN destinations x ON x.id = d.destination_id AND x.organization_id = d.organization_id
 WHERE a.organization_id = $1 AND a.started_at >= $2 AND a.started_at < $3
   [AND a.outcome = '<one of seven constants>'] [AND a.delivery_id = $n::uuid]
   [AND (a.started_at, a.id) < ($n, $n)]
 ORDER BY a.started_at DESC, a.id DESC LIMIT $n + 1
```

Only constant fragments are concatenated. Every value is a bind parameter, except the outcome, which is validated against the
closed set first (so it is one of seven constants, and the planner sees the literal). The organization predicate is on the
attempt and on both joins.

The existing index `delivery_attempts_history_idx (organization_id, started_at DESC, id DESC)` serves the window, the
order and the cursor, so the scan is bounded by `LIMIT` and not by the size of the table or the window. `deliveryId` uses
the unique key `delivery_attempts_delivery_number_key (delivery_id, attempt_number)`.

**Measured evidence (local PostgreSQL 17.10, synthetic data; not a production measurement).** A test seeds 200,000 attempts
for a large tenant and 20,000 for a small one over 30 days (about 96% `SUCCESS`, 3.9% `HTTP_ERROR`, 0.1% `ABANDONED`) and
captures `EXPLAIN (ANALYZE, BUFFERS)` of the exact statement the code issues. In every case the plan is an index scan of
`delivery_attempts_history_idx` in index order (or the unique key for one delivery), with 0 to 3 rows filtered and no
sequential scan: 24 hours, 31 days, a common outcome, one delivery, the small tenant and a deep page (about 400 pages in) all
read about one page of index entries and ran in well under one millisecond on a warm local database. The test asserts the
plan shape (no sequential scan, the named index, filtered rows bounded) and does not assert timings.

**Known limit, not measured at scale.** A **rare** `outcome` (for example `ABANDONED` at 0.1%) is a filter on the time-ordered
index, so finding a page can read many index entries and heap rows: the cost is about `limit / frequency`, bounded by the
five-second deadline, not by the feed's structure. Nothing here claims a guarantee for that case. If it matters in practice, a
partial index on the rare outcomes is the candidate, to be added only after measurement.

## Security

- Authorization first, then validation, exactly as ADR 32. A test sends garbage (including repeated parameters) as a
  non-member and as a member and compares the `404` and `400` responses.
- Cross-tenant tests: another tenant's cursor, delivery id and organization id return nothing and expose nothing.
- A response-shape test pins the exact JSON keys of an item, so a new field (and a possible leak) is a conscious change.

## Deadline

The read runs under the same five-second operations deadline as ADR 31 and 32 (`503 read_model_timeout`), taken from the
request context, so a client that goes away cancels the statement and is not reported as a timeout.

## Not in this phase

A `destinationId` filter, attempt statistics (counts by outcome, HTTP error distributions), time series, rollups,
caching, an operational-read bulkhead, destination health, and redrive. Attempt statistics, if added, must stay separate from
delivery statistics (an attempt is not a delivery). Destination health remains deferred; see the closure note of Phase 4.4.7a.
