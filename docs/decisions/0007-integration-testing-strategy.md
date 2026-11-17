# 7. Integration testing strategy

Date: 2026-11-17

## Status

Accepted

## Context

Database behavior (pooling, migrations, advisory locks, readiness) cannot be verified with mocks.
Tests must be safe to run next to a developer's own data and must not be skipped silently in CI.

## Decision

- Integration tests run against a real PostgreSQL 17 server named by
  `RELAYFORGE_TEST_DATABASE_URL`.
- Each test creates a uniquely named database (`rf_test_<random>`) on that server and drops it at
  cleanup, so tests are isolated from each other and from existing databases. The role needs
  `CREATEDB`, and the server should be disposable.
- When the variable is unset, tests skip locally with a clear message. They fail instead of
  skipping when `CI=true` or `RELAYFORGE_REQUIRE_INTEGRATION=1`.
- CI provides PostgreSQL 17 as a GitHub Actions service container with test-only credentials and
  runs `go test -race`, so integration tests always execute there.
- No paid or hosted test infrastructure is used. Locally, tests run against the Docker Compose
  PostgreSQL service or any disposable PostgreSQL 17 instance.
- Migration failure behavior uses in-memory migration sets (`testing/fstest`) so failures can be
  exercised without adding broken files to the real migrations directory.

## Consequences

Integration tests are slower than unit tests but exercise real behavior, including concurrent
migration runs.
