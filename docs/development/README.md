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

Publisher and reconciliation tuning (batch 50, lease 1m, reconcile every 1m with a 2m grace, 48h lookback,
500 per sweep) are code defaults in `internal/relay`; change them there until a real need appears.

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
- Events older than the reconciliation lookback whose broker message was lost are not repaired automatically.
- Secret rotation for destinations has a schema column but no operation yet.

## Destination, subscription and delivery history API (Phase 4.3)

All routes live under `/api/v1/organizations/{organizationId}` and use the session cookie. Every state
changing request also needs the `X-CSRF-Token` header and `Content-Type: application/json`. Bodies are strict:
unknown fields are rejected and bodies over the server limit return `413`. List responses use
`{"items": [...], "total": n, "limit": n, "offset": n}` with `limit` 1 to 100 (default 20) and `offset` >= 0.
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

### Delivery history (read-only)

| Method and path | Purpose |
|---|---|
| `GET /deliveries` | List, newest first |
| `GET /deliveries/{deliveryId}` | `{"delivery": {...}, "attempts": [...]}` |

List filters: `destinationId`, `eventId`, `status` (`PENDING`, `IN_FLIGHT`, `SUCCEEDED`, `FAILED`,
`CANCELLED`), `from` (inclusive) and `to` (exclusive) as RFC 3339 timestamps compared with the delivery's
creation time; `from` must be before `to`. A delivery has `id, eventId, destinationId, destinationName,
status, attemptCount, maxAttempts, nextAttemptAt?, lastHttpStatus?, lastErrorClass?, createdAt, updatedAt,
completedAt?`. An attempt has `attemptNumber, startedAt, finishedAt, durationMs, outcome, httpStatus?, errorClass?`.
Payloads, destination URLs, response bodies, raw error text and claim fields are never returned.

### Error codes

| Status | Code | When |
|---|---|---|
| 400 | `validation_failed`, `invalid_json` | Invalid input, unknown field, bad filter or date |
| 401 | `unauthenticated` | No valid session |
| 403 | `forbidden`, `csrf_invalid` | Missing permission, or missing CSRF token |
| 404 | `not_found` | Missing resource, malformed id, or a resource or organization of another tenant |
| 409 | `destination_revoked`, `destination_name_taken`, `already_subscribed`, `subscription_revoked`, `endpoint_revoked` | Lifecycle conflicts |
| 413, 415 | `request_too_large`, `unsupported_media_type` | Body limits |

### Permissions

OWNER and ADMIN can do everything. DEVELOPER can read, create, rename, re-target, pause and activate
destinations and manage subscriptions, but cannot rotate secrets or revoke destinations. VIEWER is read-only
(destinations, subscriptions, deliveries). See ADR 0027.

### Testing this API locally

The tests use the disposable PostgreSQL strategy described above; no Docker and no network are needed:

```powershell
$env:RELAYFORGE_TEST_DATABASE_URL = "postgres://USER:PASSWORD@127.0.0.1:54329/postgres?sslmode=disable"
go test -count=1 ./internal/app/ ./internal/httpapi/ ./internal/delivery/ ./internal/database/
```
