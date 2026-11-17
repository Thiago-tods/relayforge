# 6. Connection pool ownership and lifecycle

Date: 2026-11-17

## Status

Accepted

## Context

Several processes share the same code, but not all of them use PostgreSQL yet. Unmanaged pools
created in individual packages would waste connections and complicate shutdown.

## Decision

- `database.Open` creates the only `Pool` of a process. The process entry point
  (`internal/platform/runtime`) owns it: it opens the pool at startup, passes it to the components
  that need it, and closes it after the HTTP server has drained.
- Opening validates the configuration, connects and pings within `RELAYFORGE_DB_CONNECT_TIMEOUT`.
  A process that requires PostgreSQL refuses to start if it is unreachable.
- Processes opt in with `runtime.Options{RequireDatabase: true}`. Currently only `api` does. The
  `outbox-publisher` and `delivery-worker` stay disconnected until they have database work, so they
  do not hold idle connections. They will opt in when that work lands.
- `/readyz` includes a `postgres` check (bounded to 2 seconds) when the database is required.
  `/healthz` never touches PostgreSQL, so a database outage makes the process unready but not
  restarted.
- Errors and logs never contain the connection URL or password. HTTP responses report only
  `ok` or `unavailable`.

## Consequences

Components receive the shared `*database.Pool` instead of opening their own. The `cmd/migrate`
tool opens its own short-lived pool because it is a separate process.
