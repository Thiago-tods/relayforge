# 20. Transactional outbox

Date: 2026-11-17

## Status

Accepted

## Decision

- Accepting an event inserts the `webhook_events` row **and** its `outbox` row in one transaction.
  `202` is returned only after commit. If the outbox insert fails, the event is rolled back (tested by
  forcing the failure).
- `outbox` has one row per event (`UNIQUE (event_id)`), a composite foreign key that keeps the
  organization consistent, `published_at` (NULL means unpublished), `attempts` and `available_at`
  (default now). A **partial index** `(available_at, id) WHERE published_at IS NULL` serves a future
  publisher that polls for due, unpublished rows.
- The outbox `payload` is a small JSON envelope (`schema`, event ID, organization ID, endpoint ID,
  payload SHA-256), not the raw event payload. A publisher can load the event by ID. API responses
  never include outbox data.
- Duplicate (idempotent) requests create no second outbox row.
- **No publisher exists.** Nothing reads or updates the outbox yet, and there is no NATS integration.
  Rows accumulate unpublished until a later phase adds the publisher.

## Consequences

The database is the source of truth: an accepted event cannot be lost between persistence and
messaging. The publisher will provide at-least-once delivery to the broker, so consumers must be
idempotent.
