# 4. PostgreSQL access library: pgx v5 with pgxpool

Date: 2026-11-17

## Status

Accepted

## Context

RelayForge uses PostgreSQL as its source of truth and needs a maintained, performant Go driver with
connection pooling, context support and access to PostgreSQL-specific features.

## Decision

Use `github.com/jackc/pgx/v5` with `pgxpool`. Application code will use pgx natively. The
`database/sql` interface is exposed only through `pgx/v5/stdlib` (`Pool.SQLDB`) for libraries that
require it, currently the migration runner. Both handles share one underlying pool.

No ORM and no extra query abstraction is introduced. Repositories added in later phases will use
pgx directly.

## Alternatives considered

- `lib/pq`: in maintenance mode, no native pooling.
- `database/sql` only: loses pgx-specific types, batching and `COPY`.
- GORM or sqlc-generated layers: not needed to establish the foundation and can be decided when
  the domain model exists.

## Consequences

pgx is free and open source (MIT). Drivers are pinned in `go.mod` and checked by `govulncheck` and
Dependabot.
