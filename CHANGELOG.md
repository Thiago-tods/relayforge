# Changelog

All notable changes to this project are documented in this file.
The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project
adheres to [Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added (Phase 1: database foundation)

- PostgreSQL configuration (`RELAYFORGE_DATABASE_URL`, `RELAYFORGE_DB_*`) with validation.
- `internal/database`: pgx v5 connection pool with lifecycle management and credential-safe errors.
- PostgreSQL readiness check in `/readyz` for processes that require the database (currently `api`).
- Embedded SQL migrations (goose) with advisory-lock protection, and the `cmd/migrate` tool
  (`up`, `status`). Initial migration adds the `relayforge_set_updated_at()` trigger function.
- Real PostgreSQL integration tests and a PostgreSQL 17 service in CI.
- ADRs 0003 to 0007.

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
