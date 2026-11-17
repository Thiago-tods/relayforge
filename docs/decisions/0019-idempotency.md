# 19. Idempotency

Date: 2026-11-17

## Status

Accepted

## Decision

- Scope: an `Idempotency-Key` is unique per **ingestion endpoint**. Different endpoints may reuse a
  key. Keys are case-sensitive.
- Enforcement: PostgreSQL. `webhook_events` has `UNIQUE (ingestion_endpoint_id, idempotency_key)` and
  the insert is `INSERT ... ON CONFLICT DO NOTHING`. When two transactions race on the same key, the
  second waits for the first to commit or roll back, then inserts nothing. There is no application
  pre-check that could race.
- Outcomes:
  - new key: event and outbox record created, `202`, `duplicate: false`;
  - same key and same payload SHA-256: nothing created, `202` with the original event ID and
    `duplicate: true`;
  - same key and a different payload hash: nothing created, `409 idempotency_conflict`.
- A request that fails (including a rollback because the outbox insert failed) reserves nothing, so
  the producer can retry with the same key.
- Concurrency tests assert exactly one event and one outbox record under 24 simultaneous duplicates,
  and exactly one winner when different payloads contend for one key.

## Retention

Idempotency keys live as long as their event rows, and events are **never deleted** in this phase, so
the guarantee currently has no time limit. A future retention job may delete old events, but must not
delete a key before the documented window (at least 30 days is the proposed minimum) has passed;
until that policy exists, nothing prunes events.

## Consequences

Producers should generate a stable key per logical event (for example the event ID in their own
system) and reuse it on every retry.
