# Changelog

All notable changes to this project are documented in this file.
The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project
adheres to [Semantic Versioning](https://semver.org/).

## [Unreleased]

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
