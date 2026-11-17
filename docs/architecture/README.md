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
