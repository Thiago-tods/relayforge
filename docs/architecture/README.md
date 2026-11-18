# Architecture

RelayForge is a modular monolith with independent workers.

## Processes

- **api** serves the HTTP API and will own request validation, authentication and event ingestion.
- **outbox-publisher** reads the transactional outbox in PostgreSQL and publishes to NATS JetStream.
- **delivery-worker** consumes events from JetStream, delivers webhooks and schedules retries.

All processes share the `internal/` packages and are built from one Go module.

## Package layout

| Path                     | Responsibility                                               |
| ------------------------ | ------------------------------------------------------------ |
| `internal/platform`      | Configuration, HTTP server, error handling, process runtime  |
| `internal/observability` | Structured logging (metrics and tracing later)               |
| `internal/database`      | pgx connection pool, health check, embedded goose migrations |
| `internal/identity`      | Email normalization, Argon2id passwords, opaque tokens, users and sessions |
| `internal/tenancy`       | Organizations, memberships, invitations, roles and the permission policy |
| `internal/audit`         | Append-only audit events |
| `internal/app`           | Use cases and transaction boundaries; the authorization gate |
| `internal/httpapi`       | `/api/v1` handlers, cookie authentication, CSRF, error rendering |
| `internal/ingest`        | Ingestion endpoints, HMAC protocol, idempotent event + outbox persistence |
| `internal/secretbox`     | AES-256-GCM encryption of secrets at rest |
| `internal/ratelimit`     | Shared fixed-window counters in PostgreSQL |
| `internal/database/dbtest` | Test helper creating isolated databases for integration tests |
| `cmd/migrate`            | CLI to apply (`up`) and report (`status`) migrations         |

## Data stores

- **PostgreSQL**: durable source of truth, including the outbox.
- **Redis**: caching and short-lived coordination.
- **NATS JetStream**: durable at-least-once messaging between processes.

The detailed Technical Design Document is maintained outside this repository; decisions that
affect the code are recorded in [docs/decisions](../decisions/).

## Database layer

- One `database.Pool` (pgxpool) per process, opened and closed by `internal/platform/runtime`.
  Only processes that set `RequireDatabase` connect; currently `api`.
- `/readyz` reports a `postgres` check; `/healthz` is independent of PostgreSQL.
- Schema is defined by embedded SQL migrations tracked in `goose_db_version`, applied explicitly with
  `cmd/migrate`, never implicitly at API startup.
- See ADRs 0004 to 0007 for the rationale.

## Identity and tenancy layer

Request flow for an organization-scoped call: `httpapi` authenticates the cookie and checks CSRF,
`app` loads the caller's active membership in the requested organization (`404` if absent), checks the
permission from `tenancy` (`403` if missing), then runs the operation in a transaction together with
its audit event. Repositories in `identity`, `tenancy` and `audit` take a `database.Querier`, so the
same code runs inside or outside a transaction.

Tables: `users`, `sessions`, `organizations`, `memberships`, `organization_invitations`,
`audit_events` (migrations 00002 to 00006). Secrets are stored only as SHA-256 hashes (session, CSRF,
invitation tokens) or an Argon2id hash (passwords). Invariants enforced by the database: unique
normalized email, one active OWNER per organization, valid roles and statuses, one pending invitation
per organization and email, append-only audit events.

See ADRs 0008 to 0014.

## Webhook ingestion layer

```
producer --HTTPS--> POST /api/v1/ingest/{publicId}
   Lookup:  validate id, per-address rate limit, load endpoint, check status      (no secret needed)
   read body (<= 1 MiB) -> content type -> JSON syntax -> timestamp -> signature format
   decrypt secret (AES-256-GCM) -> verify HMAC -> per-endpoint rate limit -> idempotency key
   BEGIN
     SELECT ... FROM ingestion_endpoints WHERE id = $1 FOR SHARE   -- status and secret version re-checked
     INSERT INTO webhook_events ... ON CONFLICT (endpoint, key) DO NOTHING
     INSERT INTO outbox ...                                         -- only for a new event
   COMMIT  ->  202
```

- Rotation, pausing and revocation take `FOR UPDATE` on the endpoint row, so they wait for in-flight
  ingestion and ingestion that starts afterwards sees the new state. A request verified under the old
  secret that has not committed when the rotation commits is rejected (`signature_mismatch`); one
  that committed first was valid when accepted.
- The outbox is written but nothing reads it yet. A future publisher polls
  `outbox WHERE published_at IS NULL AND available_at <= now()` (partial index) and publishes to NATS.
- Tables (migrations 00007 to 00010): `ingestion_endpoints`, `webhook_events`, `outbox`,
  `rate_limit_counters`. Composite foreign keys keep events and outbox rows in the endpoint's
  organization.

See ADRs 0015 to 0021.

## Delivery attempt recording protocol (Phase 4)

Migration 00014 enforces this order in the database; the delivery worker must follow it.

1. **Claim** (commits on its own): `UPDATE deliveries SET status = 'IN_FLIGHT', attempt_count = attempt_count + 1, ...`
   from `PENDING`. A claim increments `attempt_count` by exactly one and sets the lease and claim token.
2. **Send** the HTTP request outside any transaction.
3. **Record and move in one transaction, in this order:**
   `INSERT INTO delivery_attempts` (number equal to `attempt_count`), then
   `UPDATE deliveries ... WHERE id = $1 AND status = 'IN_FLIGHT' AND claim_token = $2`.
   If the update affects no row the claim is stale: roll back, which also discards the attempt.
   A reaper handling an expired lease does the same, recording an `ABANDONED` attempt first.

The attempt trigger locks the delivery row (`FOR NO KEY UPDATE`) before checking that the delivery is
`IN_FLIGHT`, that the number equals `attempt_count`, and that the previous attempt exists (no gaps, no
backfill). Writers therefore always lock the delivery row first and the attempt second, which rules out
lock cycles, and a stale worker waits for a concurrent claim or completion and is then rejected.
Recording after the delivery has left `IN_FLIGHT` in the same transaction is rejected, so the order is enforced.

`delivery_attempts` rejects UPDATE, DELETE and TRUNCATE. `destinations` and `deliveries` reject DELETE
and TRUNCATE (revoke or cancel instead). These are triggers, so they apply to every role, including one
with full table privileges. Disabling them needs table ownership or superuser rights, so the
application's runtime role must not own these tables. Migration rollback uses `DROP TABLE` and is not blocked.

## Destinations, subscriptions and delivery history (Phase 4.3)

`internal/delivery` owns the persistence of destinations (`destination.go`), subscriptions (`subscription.go`,
`subscription_read.go`) and the read model of deliveries and attempts (`history.go`). `internal/app`
(`destinations.go`, `subscriptions.go`, `deliveries.go`) adds authorization, validation, row locking and audit,
and `internal/httpapi/delivery_api.go` maps them to routes. The request flow is the same as for ingestion
endpoints: the handler authenticates and checks CSRF, the service locks the organization, authorizes through
`tenancy`, changes the row and writes the audit event in one transaction. Delivery history has no write path.
Migration 00017 adds database guards for destinations (a revoked destination is immutable) and an index for
subscription listing. See ADR 0027 for the rules and the permission matrix.
