# Development notes

## Toolchain

See the prerequisites in the [README](../../README.md).

## Common tasks

| Task                  | Command                                                              |
| --------------------- | -------------------------------------------------------------------- |
| Format Go code        | `gofmt -w .`                                                         |
| Run Go tests          | `go test ./cmd/... ./internal/...`                                                      |
| Run frontend tests    | `cd web; npm test`                                                   |
| Format frontend       | `cd web; npm run format`                                             |
| Validate Compose file | `docker compose --env-file deploy/.env.example -f deploy/docker-compose.yml config` |
| Apply migrations      | `go run ./cmd/migrate up` (needs `RELAYFORGE_DATABASE_URL`)          |
| Migration status      | `go run ./cmd/migrate status`                                        |
| Integration tests     | set `RELAYFORGE_TEST_DATABASE_URL`, then `go test -count=1 ./cmd/... ./internal/...` |
| Stop infrastructure   | `docker compose --env-file deploy/.env -f deploy/docker-compose.yml down` |

Add `-v` to `down` only if you want to delete the persistent data volumes.

## Health endpoints

Every Go process serves `GET /healthz` (liveness) and `GET /readyz` (readiness) on its HTTP address.
Readiness lists one check per dependency or loop, and answers 503 when any of them fails:

| Process | Checks |
|---|---|
| all with a database | `postgres` |
| `delivery-worker` | `delivery_worker`: the claim loop and the maintenance loop are still turning |
| `outbox-publisher` | `nats`, `outbox_publisher`, `fanout_consumer` |

A loop that stops turning (for example a claim query that never returns) makes the process not ready even
though the database answers, so an orchestrator can restart it.

Every HTTP response carries an `X-Request-Id` header. A caller may send its own (8 to 64 characters from
`A-Za-z0-9._-`); anything else is replaced. Each request writes one access line with the same
`request_id`, the route **pattern** (never the raw path or query string), method, status and duration.
Probes and scrapes log at debug level. Failed requests log the same `request_id`, so a client report
can be tied to the server log.

## Writing migrations

1. Add `internal/database/migrations/NNNNN_description.sql` with the next sequential number.
2. Use `-- +goose Up` and, only when reversal is safe and lossless, `-- +goose Down`. Wrap statements
   containing `;` inside bodies (functions, triggers) in `-- +goose StatementBegin` / `StatementEnd`.
3. Never edit a migration that has been merged; add a new one instead.
4. Run `go run ./cmd/migrate up` twice locally: the second run must report no pending migrations.

## Integration tests

Integration tests use `internal/database/dbtest`, which creates a uniquely named database per test on
the server in `RELAYFORGE_TEST_DATABASE_URL` and drops it at cleanup. Use a disposable server.
`CI=true` or `RELAYFORGE_REQUIRE_INTEGRATION=1` turn a missing test database into a failure.

## Working on identity and tenancy

- Add a permission by extending the matrix in `internal/tenancy/role.go` and its table-driven test,
  then call `authorize` or `lockAndAuthorize` in `internal/app` before touching data.
- Operations that change memberships must start with `lockAndAuthorize` (row lock on the
  organization) and write their audit event in the same transaction.
- Every new tenant-owned table must be reachable only after `ActiveMembership` succeeds, and needs a
  cross-tenant test.
- Use `apptest.New(t)` for integration tests: it creates a disposable migrated database and a
  service with cheap Argon2 parameters and a controllable clock.
- Never log or return password hashes, session, CSRF or invitation tokens.

## Working on ingestion

- `apptest.Env` also provides `CreateEndpoint`, `Signed` (a correctly signed request using the fake
  clock) and `Send` (runs `Lookup` then `Accept` like the HTTP handler).
- Keep the request path ordered as in ADR 0017; do not parse or normalize the body before the signature
  is verified, and never log bodies, signatures or secrets (there is a test that scans the logs).
- Any new write on the accepted-event path must happen inside the same transaction as the event and
  outbox rows.
- Rate limits use `ratelimit.Hit` (count every request) or `Exceeded` + `Record` (count only failures).
- Generate a local encryption key with
  `[Convert]::ToBase64String([System.Security.Cryptography.RandomNumberGenerator]::GetBytes(32))` and set
  `RELAYFORGE_ENCRYPTION_KEY` in your shell. Never commit it.

## Working on delivery (Phase 4.2)

Processes (each is a separate binary and needs PostgreSQL; none needs Docker):

| Binary | Role | Extra requirements |
|---|---|---|
| `cmd/outbox-publisher` | Publishes outbox rows to JetStream, fans messages out into deliveries, reconciles | `RELAYFORGE_NATS_URL` |
| `cmd/delivery-worker` | Claims and sends deliveries, recovers expired leases | `RELAYFORGE_ENCRYPTION_KEY` |

Configuration (all optional unless noted; durations accept `30s` or plain seconds):

| Variable | Default | Meaning |
|---|---|---|
| `RELAYFORGE_NATS_URL` | none (required by the publisher) | May embed credentials; never logged |
| `RELAYFORGE_NATS_STREAM` / `RELAYFORGE_NATS_CONSUMER` | `RELAYFORGE_EVENTS` / `delivery-fanout` | Names |
| `RELAYFORGE_DELIVERY_CONCURRENCY` | 8 (1 to 64) | Maximum simultaneous requests |
| `RELAYFORGE_DELIVERY_BATCH_SIZE` | concurrency | Deliveries claimed per poll (not above concurrency) |
| `RELAYFORGE_DELIVERY_POLL_INTERVAL` | 1s | Claim polling |
| `RELAYFORGE_DELIVERY_RECOVERY_INTERVAL` | 30s | Expired lease reaper |
| `RELAYFORGE_DELIVERY_LEASE` | 2m | At least 30s and longer than the request timeout |
| `RELAYFORGE_DELIVERY_REQUEST_TIMEOUT` | 30s | Whole request, including connect and TLS |
| `RELAYFORGE_DELIVERY_SHUTDOWN_GRACE` | 10s | Wait for in-flight attempts on shutdown |

Publisher and reconciliation tuning (batch 50, lease 1m, reconcile every 1m with a 2m grace, 500 rows per
batch, 10 batches per lane per sweep, a 24h live window) are code defaults in `internal/relay`; change them
there until a real need appears. Reconciliation reads `outbox.fanned_out_at` (ADR 29): there is no lookback
window. Two lanes, each with its own budget, keep a large historical backlog from delaying recent losses: the
live lane (younger than 24h) and the history lane (older, unbounded). After migration 00018 every historical
outbox row is unmarked and is verified at up to 5,000 rows per lane per minute, so a large history drains
gradually; watch `relayforge_reconciled_events_total`.

A row that fails with an integrity or data error (SQLSTATE class 22 or 23) is isolated: healthy rows still go
through, the failing row stays unmarked, is logged (at most 10 per sweep) with its ids and an error class, counted
in `relayforge_reconcile_row_failures_total`, and retried every sweep, so it recovers by itself once the cause
is fixed. Alert on `relayforge_reconcile_oldest_live_unmarked_age_seconds` staying above a few minutes. Every
other error fails the whole sweep (`relayforge_reconcile_errors_total`).

Metrics (enable with `RELAYFORGE_METRICS_ENABLED=true`, unauthenticated, restrict at the network level)
have fixed names and no labels: `relayforge_delivery_*`, `relayforge_outbox_*`, `relayforge_fanout_*`,
`relayforge_reconcile*`, `relayforge_db_pool_*`. Logs carry an `error_class`, never URLs, payloads, headers or secrets.

Stuck-work signals: `relayforge_delivery_oldest_due_age_seconds` (how long the longest-waiting due delivery
has been due) and `relayforge_outbox_oldest_unpublished_age_seconds`. A count can be a healthy backlog; an
age that keeps growing is not. `relayforge_delivery_expired_leases` above zero for longer than the recovery
interval means no worker is running recovery. Alert on the ages, not on the counts.

### Running the tests

- Everything except the PostgreSQL suites runs with `go test ./...`. NATS tests start an **embedded**
  `nats-server` on a random loopback port with a temporary store: no Docker, no installed server.
- To use a real server instead, set `RELAYFORGE_TEST_NATS_URL` (CI does).
- PostgreSQL tests need `RELAYFORGE_TEST_DATABASE_URL`. On Windows without Docker use a portable
  PostgreSQL 17 (a zip from EnterpriseDB), `initdb`, `pg_ctl start -o "-p 54329"`, then point the URL at
  it. Stop it when you are done.
- The race detector needs CGO and a C compiler on Windows (for example w64devkit's `gcc` on `PATH`):
  `$env:CGO_ENABLED = "1"; go test -race -count=1 ./...`.
- No test reaches the public internet: the egress client takes a fake resolver and tests use local TLS servers.

### Limitations of this phase

- Destination, subscription and delivery history APIs exist (Phase 4.3, section below); there is no
  delivery replay or manual retry endpoint.
- Destinations on private addresses are not supported (ADR 0023).
- Delivery is at-least-once with no ordering guarantee. Receivers must deduplicate on
  `X-RelayForge-Delivery-Id` (ADR 0022 and 0024).
- Secret rotation for destinations has a schema column but no operation yet.

## Destination, subscription and delivery history API (Phase 4.3)

All routes live under `/api/v1/organizations/{organizationId}` and use the session cookie. Every state
changing request also needs the `X-CSRF-Token` header and `Content-Type: application/json`. Bodies are strict:
unknown fields are rejected and bodies over the server limit return `413`. List responses use
`{"items": [...], "total": n, "limit": n, "offset": n}` with `limit` 1 to 100 (default 50) and `offset` >= 0.
Errors use `{"error": {"code": "...", "message": "..."}}`.

### Destinations

| Method and path | Purpose | Notes |
|---|---|---|
| `POST /destinations` | Create | Body `{"name", "url"}`. `201`. Returns `signingSecret` once (rotation is the only other response that does) |
| `GET /destinations` | List | Optional `status=ACTIVE`, `PAUSED` or `REVOKED` |
| `GET /destinations/{destinationId}` | Read | |
| `PATCH /destinations/{destinationId}` | Rename and/or change URL | Body `{"name"?, "url"?}`, at least one |
| `POST /destinations/{destinationId}/pause` | Pause | Idempotent. Paused destinations are not claimed by the worker |
| `POST /destinations/{destinationId}/activate` | Activate | Idempotent. `409` if revoked |
| `POST /destinations/{destinationId}/rotate-secret` | Rotate signing secret | Returns the new `signingSecret` once |
| `DELETE /destinations/{destinationId}` | Revoke | Permanent, keeps history, cancels pending deliveries. Idempotent |

A destination is `{id, organizationId, name, url, urlHasQuery, status, createdByUserId, createdAt, updatedAt, revokedAt?}`.
`url` is shown **without its query string**. Ciphertext, key version and secret version are never returned.
Names are 1 to 100 printable characters, unique per organization (case-insensitive) among non-revoked destinations.
URLs must be `https` on port 443, with no credentials or fragment, and must not be an IP literal in a blocked
range or a reserved name; the same rules apply again at connection time (ADR 0023).

### Subscriptions

| Method and path | Purpose | Notes |
|---|---|---|
| `POST /subscriptions` | Create | Body `{"endpointId", "destinationId"}`. `201`; `409 already_subscribed` for a duplicate active one |
| `GET /subscriptions` | List | Filters `endpointId`, `destinationId`, `status=ACTIVE` or `REVOKED` |
| `GET /subscriptions/{subscriptionId}` | Read | |
| `DELETE /subscriptions/{subscriptionId}` | Revoke | Keeps the record; `409 subscription_revoked` if already revoked |

A subscription is `{id, organizationId, endpointId, destinationId, destinationName, destinationStatus, status,
createdByUserId, createdAt, revokedAt?}`. Event eligibility follows ADR 0026: an event is delivered through a
subscription if it was accepted inside `[createdAt, revokedAt)`. Re-subscribing creates a new record.
Revoked endpoints and destinations cannot be subscribed (`409`); paused ones can.

### Delivery history (read-only, and one redrive operation)

| Method and path | Purpose |
|---|---|
| `GET /deliveries` | List, newest first |
| `GET /deliveries/{deliveryId}` | `{"delivery": {...}, "attempts": [...]}` |

List filters: `destinationId`, `eventId`, `status` (`PENDING`, `IN_FLIGHT`, `SUCCEEDED`, `FAILED`,
`CANCELLED`), `from` (inclusive) and `to` (exclusive) as RFC 3339 timestamps compared with the delivery's
creation time; `from` must be before `to`. A delivery has `id, eventId, destinationId, destinationName,
status, attemptCount, maxAttempts, nextAttemptAt?, lastHttpStatus?, lastErrorClass?, failureReason?, createdAt,
updatedAt, completedAt?`. An attempt has `attemptNumber, startedAt, finishedAt, durationMs, outcome, httpStatus?,
errorClass?, terminalReason?`.
Payloads, destination URLs, response bodies, raw error text and claim fields are never returned.

#### Why a delivery failed (ADR 30)

A `FAILED` delivery has a `failureReason`, and the attempt that ended it (the latest failure episode, since redrive, ADR 34) has the same value as `terminalReason`.
Both are omitted when they do not apply: the delivery has not failed, the attempt did not end a failure episode, or the
delivery failed before reasons were recorded (migration 00019 does not backfill).

| `failureReason` | Meaning | What to look at |
|---|---|---|
| `DESTINATION_REJECTED` | The destination answered with a final 4xx (not 408, 425 or 429, which retry). | The destination's handling of the request; `lastHttpStatus`. |
| `REDIRECT_REFUSED` | The destination answered with a 3xx. Redirects are never followed. | The destination URL. |
| `EGRESS_BLOCKED` | The egress guard refused the address or URL. | The destination URL and its DNS. |
| `RETRIES_EXHAUSTED` | The attempts ran out while results were still retryable. | The last attempt's `outcome`, `httpStatus` and `errorClass` (a 5xx or timeout points at the destination; `secret_unavailable`, `destination_inactive` or `lease_expired` point at the platform). |
| `INTERNAL_ERROR` | A platform fault, or a result nothing else describes. | Platform logs. |

Metrics: `relayforge_delivery_failed_total` counts every failure, and one counter per reason
(`relayforge_delivery_failed_destination_rejected_total`, `..._redirect_refused_total`, `..._egress_blocked_total`,
`..._retries_exhausted_total`, `..._internal_error_total`) splits it. The names are fixed (there are no labels), so
cardinality is constant. The "delivery attempt failed" and "expired lease recovered" log lines carry
`failure_reason` for failed deliveries.

**Deployment order for migration 00019.** The migration and the application binary depend on each other: the new
binary cannot finalize deliveries against the old schema (it writes columns that do not exist yet), and the old
binary cannot safely finalize deliveries against the new schema (the database rejects its transitions into
`FAILED`, because it records no reason). Deploy in this order:

1. Apply migration 00019 (`migrate up`).
2. Drain or promptly replace the old application workers.
3. Deploy the new application binary.

This is a rolling-deployment compatibility limitation, not a data-loss issue: a delivery whose finalization is
rejected stays `IN_FLIGHT`, its lease expires, and it is recovered normally once the new binary runs. Keep the
window between steps 1 and 3 short. See ADR 0030.

### Operational read models (ADR 31)

Two read-only, organization-scoped endpoints with the same session authentication and the `deliveries:read`
permission (OWNER, ADMIN, DEVELOPER, VIEWER). They are computed live from PostgreSQL and take no row locks.

| Method and path | Purpose |
|---|---|
| `GET /api/v1/organizations/{organizationID}/operations/queue` | Queue health snapshot: `pending, due, delayed, oldestDueAgeSeconds, inFlight, expiredLeases, generatedAt, capped` |
| `GET /api/v1/organizations/{organizationID}/operations/deliveries?from=&to=` | `{generatedAt, from, to, created: {total, byStatus}, completed: {succeeded, failed, cancelled, failureRate, byFailureReason}, capped}` |

- **Queue.** `pending` includes delayed deliveries. `due` is `PENDING` with `next_attempt_at <= generatedAt` and
  `delayed` is `PENDING` after it (counted independently). **Due does not mean claimable**: deliveries of a `PAUSED`
  destination are still due. `inFlight` includes expired leases; `expiredLeases` is `IN_FLIGHT` with
  `lease_expires_at <= generatedAt` and means "awaiting recovery", not "dead worker". `oldestDueAgeSeconds` is exact
  and `0` when nothing is due.
- **Deliveries.** `created` counts deliveries by `created_at` in `[from, to)` and their **current** status.
  `completed` counts deliveries by `completed_at` in the same window. The two cohorts are independent and do not have
  to match. `from` and `to` are optional RFC 3339 timestamps (offsets honored, output in UTC); `to` defaults to now
  and `from` to `to` minus 24 hours; the window must be positive and at most 31 days (`400 validation_failed`
  otherwise). Unknown query parameters are ignored.
- **Failure rate** is `failed / (succeeded + failed)`; cancelled deliveries are excluded and shown separately. It is
  `null` when the denominator is zero or when the succeeded or failed count is capped.
- **`byFailureReason`** always has six keys: the five reasons above and `UNRECORDED`, a response-only bucket for
  `FAILED` deliveries that failed before reasons were recorded (NULL `failure_reason`).
- **Count cap.** Every counter saturates at 100,000. A value of 100,000 means "at least 100,000" (a lower bound, never
  an exact count), and the top-level `capped` is `true` when at least one returned count reached it. Counters below
  the cap stay exact, and sums such as `pending = due + delayed` hold only while nothing is capped. The SQL `LIMIT`
  bounds the matching rows each counter returns; it does not bound the index entries or heap rows PostgreSQL
  examines, so a selective bucket (a rare status or failure reason) can scan a large part of the tenant's window.
  The five-second deadline is what bounds the request.
- **Window.** Bounds are normalized to UTC microseconds and must be in the years 0001 to 9999 (UTC); `0000-...` or an
  offset that leaves the range is `400 validation_failed`. `generatedAt` is the service clock at the start of the
  request, and a defaulted `to` is that instant; the newest edge of a window ending now can still change.
- **In-flight counters** read the global `deliveries_lease_idx` with an organization filter, so their cost can grow with
  the global in-flight and expired-lease population (accepted Stage 1 limitation, ADR 31).
- **Limits.** The database work has a five-second deadline (`503 read_model_timeout`; a client that disconnects is not
  a timeout), there is no per-organization rate limiter, and no payload, destination name or URL, secret or error text is returned.
- **Migration 00020** adds two partial indexes (`deliveries_org_pending_idx`, `deliveries_org_completed_idx`),
  built concurrently. They are expected to add about one index entry per delivery state change on the pending path
  and one per completion; that is an architectural estimate, and the write overhead was not benchmarked. The claim
  path keeps using `deliveries_due_idx`.

### Operational failure visibility (ADR 32)

Same session authentication, same `deliveries:read` permission (OWNER, ADMIN, DEVELOPER, VIEWER), same rules as the read
models above: organization-scoped, read-only, a non-member or malformed organization id gets `404` **before** any
parameter is validated, and nothing sensitive is returned (no payload, destination URL, secret, raw error or claim
data).

| Method and path | Purpose |
|---|---|
| `GET /api/v1/organizations/{organizationID}/operations/failures` | Current failures, newest completion first: `{generatedAt, from, to, items, nextCursor}` |
| `GET /api/v1/organizations/{organizationID}/operations/queue/age` | Overdue-age distribution of due deliveries: `{generatedAt, buckets, capped}` |
| `GET /api/v1/organizations/{organizationID}/deliveries?failureReason=` | The existing list, narrowed to one failure reason |
| `GET /api/v1/organizations/{organizationID}/operations/attempts` | Recorded attempts across deliveries, newest start first: `{generatedAt, from, to, items, nextCursor}` (ADR 33) |

- **Failure feed.** Optional `limit` (1 to 100, default 50), `cursor`, `failureReason`, `destinationId`, `from`, `to`.
  Order is `completed_at DESC, id DESC` with **keyset** pagination: pass the `nextCursor` of one page as the `cursor` of the
  next; `nextCursor` is `null` on the last page. There is no offset and no total. The cursor is opaque and carries a
  position only (never authorization): another tenant's cursor cannot reach your data, and a malformed or altered one is
  `400 validation_failed`. `from` and `to` bound `completed_at` as `[from, to)`; the default window is the **last seven
  days**, the longest is 31 days, and the bounds follow the delivery summary's normalization (UTC, microseconds, years
  0001 to 9999). Each item has `deliveryId, eventId, destinationId, destinationName, failureReason, attemptCount,
  lastHttpStatus, lastErrorClass, createdAt, completedAt`. `failureReason` is always present: a delivery that failed
  before reasons were recorded is `UNRECORDED` (the existing delivery list keeps omitting it). A delivery that is no
  longer `FAILED` is not a current failure and is not listed.
- **Feed consistency.** A page is one snapshot, but pages are eventually consistent: `completed_at` comes from the
  worker's clock, so a delivery that commits late can sort behind a cursor you already hold and a continuation will not
  show it. Poll the first page for the newest failures.
- **Reason filter.** `failureReason` is one of `DESTINATION_REJECTED`, `REDIRECT_REFUSED`, `EGRESS_BLOCKED`,
  `RETRIES_EXHAUSTED`, `INTERNAL_ERROR` or `UNRECORDED` (a `FAILED` delivery with no recorded reason). It implies
  `status=FAILED`: `status=FAILED` is accepted, any other `status` is `400`, and an unknown value is `400`. Ordering
  (`createdAt` descending), offset pagination, the response shape and the **exact** `total` are unchanged, and a request
  without `failureReason` is unchanged byte for byte. Only a filtered request runs under the five-second deadline. A
  filtered request is authorized before any of its parameters (`limit`, `offset`, `from`, `to`, `status`, the reason) is
  validated: a non-member always gets `404`, a member gets `400` for malformed input.
- **Queue age.** Overdue age is `generatedAt - next_attempt_at` of `PENDING` deliveries with `next_attempt_at <=
  generatedAt`: time overdue, **not** time since creation (a retry restarts it). Seven fixed half-open buckets, in
  seconds: `<1m` [0, 60), `1-5m` [60, 300), `5-15m` [300, 900), `15-60m` [900, 3600), `1-6h` [3600, 21600), `6-24h`
  [21600, 86400) and `>24h` [86400, no limit; `maxSeconds` is `null`). Future deliveries are excluded and a paused
  destination's due deliveries are counted. Each bucket saturates independently at 100,000 ("at least 100,000") and `capped`
  is `true` when any did; `generatedAt` is the service clock, the same instant for every bucket.
- **Limits.** The feed, queue age and a filtered list use the five-second deadline (`503 read_model_timeout`; a client
  that disconnects is not a timeout). There is no per-organization rate limiter and no concurrency limit on these reads.
- **Migration 00021** adds `deliveries_org_failed_idx` (partial on `status = 'FAILED'`, built concurrently), added
  because query-plan tests showed the feed reading the whole window without it. It is expected to add one index entry per
  delivery that ends `FAILED`; that is an architectural estimate, and the write overhead was **not benchmarked**. The
  claim path keeps using `deliveries_due_idx`.
- **Not yet available** (deferred): destination health and time-series metrics.

### Attempt activity feed (ADR 33)

`GET /api/v1/organizations/{organizationID}/operations/attempts` lists the recorded delivery attempts of the organization,
most recent start first. Same authentication, permission and `404`-before-validation rule as above; read-only, no
migration and no new index.

- **Parameters** (all optional): `limit` (1 to 100, default 50), `cursor`, `outcome` (`SUCCESS`, `HTTP_ERROR`, `REDIRECT`,
  `NETWORK_ERROR`, `TIMEOUT`, `BLOCKED_BY_POLICY` or `ABANDONED`, upper case), `deliveryId` (a UUID), `from` and `to`.
  There is **no `destinationId` filter** yet; an unknown parameter is ignored.
- **Window.** `[from, to)` on `started_at`. The default is the **last 24 hours** and the longest is 31 days; the bounds follow
  the same normalization as the other operational reads.
- **Order and pagination.** `started_at DESC, id DESC`, keyset only: pass `nextCursor` as `cursor` for the next page; it is
  `null` on the last page. The cursor is opaque, carries a position only and is not interchangeable with the failure feed's
  (`400`). No offset and no total.
- **Item fields** (stored, nothing derived): `attemptId, deliveryId, eventId, destinationId, destinationName,
  attemptNumber, startedAt, finishedAt, durationMs, outcome`, and, when set, `httpStatus, errorClass, terminalReason`.
  `ABANDONED` attempts are included: they mean a worker did not finish (lease expired, shutdown), not that the destination
  failed.
- **Consistency.** A page is one snapshot; across pages the feed is a live tail (a late-committing attempt can sort behind a
  cursor). Start again from the first page for the newest attempts. The five-second deadline applies (`503
  read_model_timeout`). A rare `outcome` filter can scan many index entries to fill a page; that is documented in ADR 33 and
  was not measured at production scale.

### Redrive a failed delivery (ADR 34)

`POST /api/v1/organizations/{organizationId}/deliveries/{deliveryId}/redrive` returns **one** `FAILED` delivery to
`PENDING`, due now, and raises its `maxAttempts` by five (capped at 50; a budget already at 50 is `409
retry_budget_exhausted`). It needs the session cookie, the CSRF header and the permission `deliveries:redrive` (OWNER,
ADMIN, DEVELOPER; not VIEWER). Body (strict JSON): `{"reason": "...", "expectedAttemptCount": 8}`. `reason` is required
(1 to 500 printable characters, recorded in the audit event only) and `expectedAttemptCount` is the `attemptCount` you saw
when you decided to redrive. Success is `200` with `{"delivery": {...}, "redrive": {"previousMaxAttempts",
"maxAttempts", "previousFailureReason"}}` (`previousFailureReason` is `null` for a delivery that failed before reasons
were recorded).

- The delivery keeps its id, event, destination, `attemptCount` and every attempt row; the next attempt is number
  `attemptCount + 1`. It is sent again by the normal worker, so a paused destination keeps it `PENDING` until it is
  activated, and a revoked destination refuses the redrive (`409 destination_revoked`).
- A repeated request is `409 delivery_not_failed` (there is no idempotency replay: read the delivery). A request that refers
  to an earlier failure of a delivery that has failed again is `409 delivery_changed`. A missing, empty or altered stored
  payload is `409 event_payload_unavailable`; a payload is never rebuilt.
- Retries of a redriven delivery use the normal attempt-number backoff, which is long for high attempt numbers (about 2 h,
  4 h, then 6 h between attempts 9 to 13). Receivers see the same delivery id again and must be idempotent.
- A redriven delivery leaves the failure feed and the failed list until it fails again; the attempt feed shows the attempts
  of every failure episode, and more than one attempt of a delivery can carry a `terminalReason`.
- Metric: `relayforge_delivery_redriven_total`. Audit action: `delivery.redriven`.
- Rollout order is migration 00022, then the delivery worker, then the API (ADR 34).

### Error codes

| Status | Code | When |
|---|---|---|
| 400 | `validation_failed`, `invalid_json` | Invalid input, unknown field, bad filter or date |
| 401 | `unauthenticated` | No valid session |
| 403 | `forbidden`, `csrf_invalid` | Missing permission, or missing CSRF token |
| 404 | `not_found` | Missing resource, malformed id, or a resource or organization of another tenant |
| 409 | `destination_revoked`, `destination_name_taken`, `already_subscribed`, `subscription_revoked`, `endpoint_revoked` | Lifecycle conflicts |
| 409 | `delivery_not_failed`, `delivery_changed`, `retry_budget_exhausted`, `event_payload_unavailable` | Redrive conflicts (ADR 34) |
| 413, 415 | `request_too_large`, `unsupported_media_type` | Body limits |
| 503 | `read_model_timeout` | An operational read (queue, summary, failure feed, queue age, or a delivery list filtered by `failureReason`) exceeded its five-second deadline; use a shorter window |
| 503 | `lock_timeout` | A redrive could not get its database lock within five seconds; nothing changed, retry (a `Retry-After` header is sent) |

### Permissions

OWNER and ADMIN can do everything. DEVELOPER can read, create, rename, re-target, pause and activate
destinations and manage subscriptions, but cannot rotate secrets or revoke destinations. VIEWER is read-only
(destinations, subscriptions, deliveries). Redriving a failed delivery needs `deliveries:redrive`: OWNER, ADMIN and DEVELOPER, not VIEWER (ADR 0034). See ADR 0027.

### Testing this API locally

The tests use the disposable PostgreSQL strategy described above; no Docker and no network are needed:

```powershell
$env:RELAYFORGE_TEST_DATABASE_URL = "postgres://USER:PASSWORD@127.0.0.1:54329/postgres?sslmode=disable"
go test -count=1 ./internal/app/ ./internal/httpapi/ ./internal/delivery/ ./internal/database/
```

## Frontend (web/)

The console is React, TypeScript and Vite. It uses no state library and no runtime dependency beyond React and
React Router; everything it shows comes from the real API.

**Running it.** Start the Go API, then `cd web; npm run dev` (port 5173). Vite forwards `/api` to
`http://[::1]:8080` (`web/vite.config.ts`). The IPv6 target is deliberate: on a machine where another server holds
`127.0.0.1:8080` (for example Apache), `localhost` can reach the wrong process. `changeOrigin` stays `false`
because `POST /api/v1/auth/csrf` compares `Origin` with `Host`. There is no CORS: the browser talks to one origin.

**Production.** The Go API does not serve static files. The intended setup is a reverse proxy that serves `web/dist`
and forwards `/api` to the API on the same origin, because the session cookie is scoped to `Path=/api`. The proxy
configuration is not part of this repository yet.

**Sessions and CSRF (ADR 8, ADR 10).** The session is an HttpOnly cookie that the browser sends itself. The CSRF
token is held in memory only (`web/src/api/client.ts`); it is never stored or logged.
- Login and register return the token. After a page reload there is none, so the first state-changing request
  fetches one with `POST /auth/csrf` (a header `X-Requested-With: XMLHttpRequest` is required).
- The server keeps one token hash per session, and rotating replaces it. Tabs of one browser share the cookie and
  therefore the token, so a rotation in one tab invalidates the token in the others.
- When a request is answered `403 csrf_invalid`, the client rotates once and sends that request once more. The
  backend checks the token before the handler runs, so the rejected request changed nothing and repeating it is
  safe. Concurrent failures in a tab share one rotation. A second failure is shown as an error, never retried.
- After a rotation, and when another tab signs in or out, the tab re-reads `GET /auth/me`. If the session now
  belongs to a different user, the tab clears its state and asks for sign-in. Tabs tell each other only
  "signed-in" or "signed-out" through `BroadcastChannel`, never a token or identity.
- The session has an absolute lifetime and is never extended: the UI warns shortly before it ends and then signs out.

**Permissions.** The API returns a role, not a permission list. `web/src/org/permissions.ts` is a client copy of
the matrix in `internal/tenancy/role.go`, used only to decide which controls to show. The backend authorizes every
request and a 403 always wins. `permissions.drift.test.ts` parses `role.go` and fails when the two differ.

**Checks.** `npm run format:check`, `lint`, `typecheck`, `test` and `build`. The tests stub `fetch`; they are not
end-to-end evidence.

## Operations pages (web/, Phase F1)

The operations pages read the operational read models (ADR 31 to 33) and show them without adding to them. All are
read-only, need `deliveries:read` and run under the API's five-second read deadline.

- **Queue** (`/o/:orgId/operations/queue`): `GET /operations/queue` and `GET /operations/queue/age`. Two independent
  reads, made on open, every 15 seconds while the tab is visible, and when the user presses Refresh (see
  **Scheduled refresh** below).
- **Counts are delivery counts.** A count of 100,000 or more is saturated and is displayed as "≥ 100,000" (a lower
  bound), decided from the count itself (`web/src/lib/format.ts`), not from the response-level `capped` flag.
- **Wording** (`web/src/features/operations/queueLabels.ts`) repeats what the backend counts and nothing stronger.
  The page never calls a queue "healthy" or "stuck": the API has no such notion.
- **Time** is shown in UTC and labeled as such.
- **Fetching** uses `useResource(scope, load)` (`web/src/lib/useResource.ts`): the scope is the organization id, a
  request is aborted when it changes or the page unmounts, and data of another scope is never shown.
- **Activity** (`/o/:orgId/operations/activity`): `GET /operations/deliveries`, `GET /operations/failures` and
  `GET /operations/attempts`. Three independent reads on open, each with its own loading and error state, scheduled as
  described under **Scheduled refresh**, and again when the user presses Refresh in that section. The summary is aggregate delivery counts (two cohorts: created
  by current status, completed by completion time), never individual deliveries.
- **Date ranges** are UTC, per section, applied with an explicit Apply (`web/src/lib/dateRange.ts`). With none
  applied, no `from` or `to` is sent: the server chooses its own default (24 h for the summary and attempts, seven
  days for failures) and the page reuses the window the server echoes for that query's continuation pages. The
  browser's clock is never used to compute a default. A range the user applied is written to the URL and is reused
  for every page and refresh.
- **URL state** (`web/src/lib/activityQuery.ts`): filters and explicit ranges only, with one prefix per section
  (`s`, `f`, `a`). A value that is not valid is ignored with a notice. A cursor is never stored in the URL.
- **Pagination** (`web/src/lib/useCursorFeed.ts`): the hook stores the opaque cursors the server issues and sends one
  only to continue the query that produced it. Previous returns to a held page without a request. A new filter or
  Refresh drops every held page and starts from the first. The backend cursor is a position only and is not bound to
  the filters, so this client-side rule is what keeps a cursor from being reused with other filters.
- **Destination filter**: failures only, server-side (`destinationId`). The selector loads on request, 100 per page
  by offset, merged by id. That is not a snapshot: destinations created or changed while paging can be missed or
  repeated. Attempts have no destination filter (ADR 33), and rows are never filtered in the browser.
- **Response checks** (`web/src/api/operationsParse.ts`) verify the fields the page reads. Unknown outcome or reason
  values are shown as text. Delivery IDs are copyable text: links to a delivery page come with the delivery detail
  phase.
- **Scheduled refresh** (Phase F1d; `web/src/lib/useAutoRefresh.ts`, `useLiveResource.ts`, `useNewActivity.ts`): the Queue
  snapshot, the queue age and the Activity summary reload themselves every 15 seconds, each on its own schedule. The
  first read is made on entry. The timer is chained: it is armed only after the previous request settles, so a resource
  never has two requests at once and a tick that falls while a request runs is dropped, never queued. Nothing is
  scheduled while the tab is hidden; when it becomes visible again each section refreshes at once (unless it is
  already loading). Requests are aborted on unmount and when the organization changes, and the Refresh button still
  works and replaces a running scheduled request. A scheduled reload over data already shown does not look busy, and a
  failed one keeps the data and shows a non-blocking notice; with nothing to show yet the error still blocks.
- **Feeds are never replaced by a schedule.** The failure and attempt feeds only check, on the same schedule, for
  newer entries: they read the newest entry of the same query (limit 1, no cursor, the applied filters and range, and
  no window when none is applied so the server picks its default). If it is not held on the first page and is not
  older than the newest held entry, a "New activity available" notice offers "Show newest", which is a normal Refresh
  back to page 1. The held pages, server cursors, filters, ranges and position are never reset by the check. A failed
  check says so and leaves the rows alone. Limitation: the check looks at the single newest entry, so it only says that
  something newer exists, not how much.
- **Show attempts** (`attemptWindowFor` in `web/src/lib/dateRange.ts`): the attempt window is anchored to the delivery,
  not to the failure feed's range. It runs from the minute of `createdAt` to the next whole minute after `completedAt`,
  which holds every attempt of the delivery (they all start between those two instants), including the episodes of a
  redriven delivery. The attempt feed spans at most 31 days, so a longer lifetime is clamped to the 31 days that end at
  the latest completion and the page says that older attempts are excluded. A truncated result is never presented as
  the complete history. The delivery detail page returns the recorded attempts for one delivery, without the attempt feed’s 31-day window limit. Because the API reads attempts before the delivery row, an attempt appended concurrently by a worker can appear after a later refresh.
- **Ignored link values** are reported once and dropped from the URL when the user applies a section or presses "Clear
  ignored filters" (`normalizedParams` in `web/src/lib/activityQuery.ts`).
- **Unverified compatibility**: the API sends microsecond timestamps and the page reads them with `new Date`. This is
  verified in Chromium only. Safari and Firefox have not been checked.

### Delivery detail (web/, Phase F1c)

- **Route** (`/o/:orgId/operations/deliveries/:deliveryId`) reads the existing `GET /api/v1/organizations/{organizationID}/deliveries/{deliveryID}` endpoint. The backend authorizes `deliveries:read` and scopes both the delivery and its attempts to the organization.
- **Displayed fields** are operational metadata only: delivery/event/destination identifiers and destination name, status, attempt budget, failure details, relevant timestamps, and stored attempt outcomes. The page never requests or displays event payloads, destination URLs, signing secrets, claim tokens, worker IDs, or leases.
- **Attempt history** is returned in ascending attempt-number order by the API and presented newest first. A worker append concurrent with the read may appear only after refresh. Episode boundaries are derived only from stored `terminalReason` markers; an unmarked group remains explicitly unclassified. `ABANDONED` is described as an attempt the worker did not finish, not as a destination failure.
- **Fetching** uses `useResource` scoped to organization and delivery ID. It fetches once on entry, aborts on unmount or scope change, hides data from the previous scope, and offers explicit refresh/retry. There is no polling. Phase F2 adds one mutation, the redrive (below).
- Delivery IDs in both Activity feeds link to the detail route. The existing **Show attempts** action remains available.

### Delivery redrive in the UI (web/, Phase F2)

- **Visibility**: `DeliveryDetailPage` shows **Redrive** only when `can('deliveries:redrive')` (a client copy of the role matrix; the backend still authorizes) and the delivery is `FAILED`. At `maxAttempts >= 50` it shows text instead. The destination status is deliberately not fetched; `409 destination_revoked` is the authority.
- **Files**: `src/api/deliveries.ts` (`redriveDelivery`, strict parse of `{delivery, redrive}`; an unreadable or inconsistent HTTP 200 becomes `ApiError(200, 'unreadable_success')`), `src/features/deliveries/useRedrive.ts` (state machine, 30 s timeout, duplicate-submit guard, one verification GET), `redriveOutcome.ts` (error and comparison messages, advisory budget), `RedriveDialog.tsx`, `RedriveBanner.tsx`, `src/lib/redriveReason.ts` (advisory reason checks), `src/ui/Dialog.tsx`.
- **No second POST**: only the client's existing `csrf_invalid` retry may repeat the request. An ambiguous outcome is resolved by reading the delivery (a GET, once automatically and again only through "Check again"); a new redrive needs a new dialog and a new reason. A browser's network stack can itself resend a POST after a reset keep-alive connection; the backend's `409 delivery_not_failed` is what makes that safe.
- **Advisory validation**: the reason checks (trimmed emptiness, 500 code points, C0 controls and DEL) and the `+5, cap 50` budget estimate give early feedback only; the server's answer wins.
- **Tests** use a stubbed fetch (`DeliveryDetailPage.redrive.test.tsx`, `deliveries.redrive.test.ts`, `redriveReason.test.ts`); they are not end-to-end evidence. Focus trapping and background inertness come from the native `<dialog>` and were checked in a real browser against a mock API, not in jsdom.

### Destination management in the UI (web/, Phase F3a)

- **Routes**: `/o/:orgId/destinations` (`DestinationsPage`) and `/o/:orgId/destinations/:destinationId` (`DestinationDetailPage`), linked from the sidebar group "Configuration". The control visibility follows `can()` for `destinations:read`, `create`, `update`, `pause`, `rotate_secret` and `revoke`; the backend authorizes every request and a 403 hides the controls.
- **API** (`src/api/destinations.ts`, `destinationTypes.ts`): F1's `listDestinations` (id, name, status only) is unchanged. `listDestinationRecords`, `getDestination` and the six mutations validate the answers: organization and destination ids, status, timestamps, `url` without a query or fragment, `urlHasQuery`, `revokedAt` only for REVOKED, and `signingSecret` present only in the create and rotate answers. A 2xx whose body cannot be trusted is `ApiError(200, 'unreadable_success')`.
- **One request per confirmation**: `useDestinationMutation` guards a second submit in the same tick, times out after 30 s and classifies the error. Rejections (400, 403, 404, 409, 503 `lock_timeout`) are known; a lost connection, a timeout, a 5xx and an unreadable 2xx are unknown, so the page reads the destination once and never repeats the request. Rotation is not idempotent, which is why it is never repeated.
- **Secrets**: `DestinationSecretDialog` holds the secret only in props and state; it is not dismissible, has a copy button without the secret in any label or toast, and requires an acknowledgment. Never put a secret in storage, the URL or a log.
- **URL changes**: the API hides the query string, so the change-URL field starts empty and the full new URL is sent.
- **Tests** use a stubbed fetch (`destinations.test.ts`, `DestinationsPage.test.tsx`, `DestinationDetailPage.test.tsx`); they are not end-to-end evidence.

### Subscription management in the UI (web/, Phase F3b)

- **Routes**: `/o/:orgId/subscriptions` (`SubscriptionsPage`) and `/o/:orgId/subscriptions/:subscriptionId` (`SubscriptionDetailPage`), linked from the Configuration sidebar group.
- **API** (`src/api/subscriptions.ts`, `subscriptionTypes.ts`, `ingestionEndpoints.ts`, `ingestionEndpointTypes.ts`): list, get, create and revoke subscriptions, and read-only list and get for ingestion endpoints. Answers are validated strictly (organization, ids, both statuses, `revokedAt` only for REVOKED and not before `createdAt`); a mutation answer that cannot be trusted is `unreadable_success`.
- **Two statuses**: a subscription's `status` (ACTIVE or REVOKED) describes only its own interval; `destinationStatus` is the destination's. They are separate columns and badges and are never derived from each other.
- **Endpoint names**: the subscription answer has none, so `useEndpointLookups` reads the distinct endpoints of the visible rows with `GET .../ingestion-endpoints/{id}` and falls back to a short id.
- **One request per confirmation**: `useSubscriptionMutation` (separate from `useDestinationMutation`) guards a second submit in the same tick, times out after 30 s and classifies the error. After an unknown result the pair (create) or the subscription (revoke) is read once and nothing is sent again.
- **Revoke is not idempotent** in the backend (`409 subscription_revoked` for a second revoke), so the UI reads the state instead of repeating it.
- **Tests** use a stubbed fetch (`subscriptions.test.ts`, `ingestionEndpoints.test.ts`, `SubscriptionsPage.test.tsx`, `SubscriptionDetailPage.test.tsx`); they are not end-to-end evidence.
