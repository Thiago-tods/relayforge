# 5. Migration strategy: embedded SQL migrations with goose

Date: 2026-11-17

## Status

Accepted

## Context

The schema must be versioned, reproducible and applicable without a separately installed CLI.
Several processes may start at the same time and must not apply the same migration twice.

## Decision

Use `github.com/pressly/goose/v3` as a library through its `Provider` API:

- Migrations are plain SQL files in `internal/database/migrations/`, named
  `NNNNN_description.sql`, embedded in the binary with `go:embed`.
- History is stored by goose in the `goose_db_version` table.
- The provider uses a PostgreSQL session-level advisory lock, so concurrent runners are serialized
  and each migration is applied once.
- Each SQL migration runs in a transaction by default, so a failing migration leaves no partial
  changes and stops the run.
- Migrations are applied explicitly with `go run ./cmd/migrate up`. They are not applied
  automatically at API startup, so deployments control when schema changes happen.
- Every migration has a `Down` section only when reversal is genuinely safe. A migration that
  cannot be reversed without data loss must say so in its header instead of providing a `Down`.

The first migration only creates the shared `relayforge_set_updated_at()` trigger function, so the
pipeline is exercised without inventing domain tables ahead of their phase.

## Alternatives considered

- golang-migrate: also viable; goose was chosen for its embedded `Provider`, session locker and
  clear partial-failure reporting.
- Atlas or Flyway: heavier tooling than needed.

## Consequences

Schema changes require a new numbered file; edited, already-applied files are not detected as
changed, so applied migrations must never be modified.

## Baseline migration review

Review of PR #5 asked whether `relayforge_set_updated_at()` belongs in Phase 1 or should arrive with
the first table that uses it.

Facts checked: migration `00001` exists only in one unmerged commit on
`feature/phase-1-database-foundation`; PR #5 is open; it has not reached `main`, and it has only been
applied to disposable CI and local test databases. Editing or removing it would therefore be safe.

Decision: **keep it, unchanged.**

- The migration pipeline needs at least one embedded migration to be usable: goose reports an error
  for an empty migration set, so `cmd/migrate` and the embedded-migration tests need a baseline.
- The alternative baselines are worse. A placeholder table would be exactly the empty schema this
  phase avoids, and an empty `SELECT 1` migration proves nothing.
- The function is tiny, has no data, depends on nothing, and has a lossless `Down`. Its cost if
  unused is one small function, which a later migration can drop.
- Phase 2 tables with an `updated_at` column will consume it, so it avoids creating the same
  function inside the first domain migration.

Rule going forward: once a migration is on `main`, it is immutable. Any change, including removing
this function, must be a new migration. If no table uses the function by the time the first domain
schema lands, the reviewer should require a new migration that drops it.
