# Changelog

All notable changes to this project are documented in this file.
The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project
adheres to [Semantic Versioning](https://semver.org/).

## [Unreleased]

### Changed (Phase 4.4.3: reconciliation marker)

- Reconciliation no longer scans a 48-hour window of events. Migration 00018 adds `outbox.fanned_out_at` and a
  partial index on unmarked rows; `FanOut` and `Reconcile` set the marker in the same transaction that creates
  the deliveries, and `Reconcile` works only from unmarked rows. A lost fan-out is now repaired however old it
  is, including after JetStream expired its message.
- `delivery.Reconcile` no longer takes a lookback; it takes a `pgx.Tx` and `ReconcileParams` and returns a
  `ReconcileResult`. `relay.ConsumerConfig` drops `Lookback` and gains `LiveWindow`, per-lane batch budgets and
  `ReconcileMaxRowFailures`.
- Reconciliation has two lanes with independent budgets: live (younger than 24 hours) and history (older,
  unbounded), so a large historical backlog cannot delay the repair of a recent loss.
- A row that fails with an integrity or data error (SQLSTATE class 22 or 23) no longer blocks the rest of the
  batch: healthy rows are reconciled, the failing row stays unmarked, is reported safely and is retried every
  sweep. Transient errors still fail the whole sweep.
- Historical outbox rows start unmarked and are verified in bounded batches (no bulk update in the migration).
- New metrics: `relayforge_reconciled_events_total`, `relayforge_reconcile_row_failures_total` and the gauges
  `relayforge_reconcile_oldest_live_unmarked_age_seconds` and
  `relayforge_reconcile_oldest_history_unmarked_age_seconds`.
- Tests for the event/outbox invariant, and a guard that keeps `INSERT INTO webhook_events` and
  `INSERT INTO outbox` inside `internal/ingest/store.go`. ADR 29.

### Added (Phase 4.4: operational reliability and observability, first stages)

- `X-Request-Id` on every response and one access log line per request (route pattern, status, duration),
  with the same id on failed-request logs. Database error details (which can repeat an e-mail address or
  idempotency key) no longer reach the log.
- Outbox, event and organization identifiers on relay log lines.
- Gauges `relayforge_delivery_oldest_due_age_seconds` and `relayforge_outbox_oldest_unpublished_age_seconds`,
  and `relayforge_db_pool_*` connection pool gauges.
- Readiness now covers the delivery worker loops, the outbox publisher, the fan-out consumer and the NATS
  connection, not only PostgreSQL.
- ADR 28.

### Changed (Phase 4.4)

- The delivery worker no longer sleeps a whole `PollInterval` after a full batch while all slots are busy;
  it waits for a slot instead. Throughput was previously capped near `Concurrency` deliveries per poll interval.
- A request interrupted by shutdown is recorded immediately as an `ABANDONED` attempt
  (`shutdown_interrupted`) and is due again in 5 s, instead of waiting for the lease (2 min) and the next
  recovery sweep. The attempt still counts toward `max_attempts`.
- `RELAYFORGE_DELIVERY_SHUTDOWN_GRACE` defaults to 10 s (was 20 s) and is reduced at start-up so that the
  grace plus the 5 s result write fits in `RELAYFORGE_SHUTDOWN_TIMEOUT`.
- `QueueCounts` names the status in each subquery so it uses the partial indexes and no longer scans the
  whole, ever-growing `deliveries` table every 30 s.
- One metrics registry and one `GET /metrics` route per process, so roles can be combined in one process.

### Added (Phase 4.3: destination and subscription management API)

- Destination management: create (with a signing secret shown once), list, read, rename, change URL, pause,
  activate, rotate the secret and revoke (`DELETE`, permanent). URLs pass the SSRF-safe validation and are
  returned without their query string.
- Subscription management: create, list, read and revoke (`DELETE`), on top of `delivery.Subscribe` and
  `delivery.RevokeSubscription`; ADR 0026 event-time semantics are unchanged.
- Read-only delivery history: filtered, paginated list and a detail view with the recorded attempts.
- New permissions in the RBAC matrix (destinations, subscriptions, deliveries) and audit events for every mutation.
- Migration 00017: a revoked destination is immutable at the database level, destination identity and
  `secret_version` guards, and an organization index for subscription listing.
- ADR 0027.

### Added (Phase 4.2: reliable webhook delivery engine)

- `internal/egress`: outbound HTTP client with DNS pinning, SSRF address blocking, no proxies, no redirects,
  bounded timeouts and responses, and sanitized errors (ADR 0023).
- `internal/delivery`: outbound `v1=` HMAC signing with per-destination `rfd_` secrets (ADR 0022); claim,
  execute, finalize worker with fencing tokens, leases, `ABANDONED` recovery, graceful shutdown and
  bounded concurrency; retry classification with `Retry-After` handling (ADR 0024).
- `internal/messaging` and `internal/relay`: NATS JetStream stream and durable consumer, transactional
  outbox publisher (publish acknowledgment before `published_at`), idempotent fan-out and PostgreSQL
  reconciliation (ADR 0025). `cmd/outbox-publisher` now runs them.
- Migration 00015: `subscriptions` (tenant-safe composite foreign keys, revoked, never deleted).
- Migration 00016: subscription lifecycle. One ACTIVE subscription per endpoint/destination pair (partial
  unique index), re-subscription after revocation, immutable revoked history, valid intervals.
- Fan-out and reconciliation now use the event-time rule "active at acceptance" (ADR 0026), so revoking a
  subscription no longer drops events accepted while it was active.
- `delivery.Subscribe` and `delivery.RevokeSubscription`.
- Fixed NATS tests failing on a shared server ("subjects overlap"): the stream subject is now configurable.
- Label-free delivery, outbox, fan-out and reconciliation metrics; gauges in the metrics registry.
- Dependencies: `nats.go` (client) and `nats-server` (embedded server for tests only) with their transitive modules.
- CI starts a disposable NATS JetStream container.
- Delivery is at-least-once. Exactly-once is not claimed.

### Added (Phase 3: webhook ingestion and transactional outbox)

- Ingestion endpoints (`ingestion_endpoints`) with random public identifiers and AES-256-GCM encrypted
  signing secrets (`RELAYFORGE_ENCRYPTION_KEY`, required by the API), shown once on creation or rotation.
- `POST /api/v1/ingest/{publicId}`: HMAC-SHA256 signatures, strict timestamp window, 1 MiB limit,
  PostgreSQL-enforced idempotency, and an atomic event + outbox commit before `202`.
- Management API for endpoints (create, list, get, rename, pause, activate, rotate secret, revoke) and
  organization-scoped event listing and retrieval, with new RBAC permissions.
- Shared PostgreSQL rate limiting for ingestion and brute-force protection for login.
- Prometheus-format metrics at `GET /metrics` (opt-in) and structured logging without secrets.
- Migrations 00007 to 00010 and ADRs 0015 to 0021.

### Changed

- Login failures are now counted and limited (`429 rate_limited` with `Retry-After`).
- `apperror.Error` supports `RetryAfter`; the HTTP server limits header size.

### Added (Phase 2: core domain)

- Users, sessions, organizations, memberships, invitations and audit events (migrations 00002 to 00006).
- First-party authentication: Argon2id password hashing, opaque revocable sessions with a 12 hour
  absolute lifetime, hardened cookies and synchronizer-token CSRF protection.
- `/api/v1` endpoints for authentication, organizations, members, invitations, ownership transfer
  and audit events, with strict tenant isolation and pagination.
- Centralized RBAC policy with the roles OWNER, ADMIN, DEVELOPER and VIEWER; exactly one owner per
  organization, protected by a unique index and transactional ownership transfer.
- `RELAYFORGE_SESSION_LIFETIME` and `RELAYFORGE_COOKIE_SECURE` settings.
- ADRs 0008 to 0014.

### Not included

Email verification, password reset, MFA, automated invitation email, login rate limiting,
leaving an organization, and organization deletion.

### Added (Phase 1: database foundation)

- PostgreSQL configuration (`RELAYFORGE_DATABASE_URL`, `RELAYFORGE_DB_*`) with validation.
- `internal/database`: pgx v5 connection pool with lifecycle management and credential-safe errors.
- PostgreSQL readiness check in `/readyz` for processes that require the database (currently `api`).
- Embedded SQL migrations (goose) with advisory-lock protection, and the `cmd/migrate` tool
  (`up`, `status`). Initial migration adds the `relayforge_set_updated_at()` trigger function.
- Real PostgreSQL integration tests and a PostgreSQL 17 service in CI.
- ADRs 0003 to 0007.

### Review fixes

- `Pool.SQLDB()` now returns `(*sql.DB, error)` and is mutex-protected against `Close`; it returns
  `ErrClosed` after closure.
- `dbtest` validates the test server URL and only drops databases it created.
- `runtime` and `cmd/migrate` gained injectable entry points and automated tests.

### Changed

- Go module path is now `github.com/Thiago-tods/relayforge` (see ADR 0003).
- `runtime.Run` takes an `Options` argument.

### Added

- Phase 0 project foundation: Go backend skeleton (`api`, `outbox-publisher`, `delivery-worker`)
  with configuration loading, structured logging, HTTP server, graceful shutdown and health endpoints.
- React + TypeScript + Vite frontend shell with routing, ESLint, Prettier and Vitest.
- Docker Compose local infrastructure for PostgreSQL, Redis and NATS JetStream.
- GitHub Actions CI and Dependabot configuration.
- Project documentation.
