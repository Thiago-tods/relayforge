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
