# 25. Outbox relay over NATS JetStream, with reconciliation

Date: 2026-11-17

## Status

Accepted

## Context

ADR 20 made event acceptance and the outbox row one transaction. Something must now turn outbox rows into
deliveries without coupling ingestion latency to delivery. NATS JetStream gives durable, replayable
messaging; PostgreSQL remains the source of truth.

## Decision

**Publisher** (`internal/relay.Publisher`). A short transaction claims due unpublished outbox rows with
`FOR UPDATE SKIP LOCKED` and pushes `available_at` forward by a lease (no schema change needed), then commits.
Each row is published with `Nats-Msg-Id = outbox.id` and the publisher waits for the JetStream
acknowledgment. Only then is `published_at` set. A failed publish leaves the row unpublished with
exponential backoff (capped). If the process dies after the acknowledgment, the row is republished after
its lease; JetStream drops the duplicate inside its duplicate window and the consumer is idempotent anyway.

**Message.** Identifiers only (`outbox_id`, `organization_id`, `event_id`); never the payload.

**Stream** `RELAYFORGE_EVENTS`: file storage, limits retention with `MaxAge` 24 h, duplicate window 2 h,
one replica, delete and purge denied.

**Consumer** `delivery-fanout`: durable pull, explicit acknowledgments, `MaxDeliver` 10, a back-off list
whose first entry (30 s) is the effective time a handler has before redelivery, fetch batches of 16,
`MaxAckPending` 64. Handling a message runs `delivery.FanOut` in one transaction and acknowledges **after
commit**. A failure sends a negative acknowledgment. A malformed message is terminated, because retrying
cannot fix it. The organization is part of the fan-out query, so a message naming the wrong tenant creates
nothing.

**Fan-out** inserts one `PENDING` delivery per subscription that was active when the event was accepted,
with `ON CONFLICT (event_id, destination_id) DO NOTHING`. The exact event-time rule, re-subscription and
the separation from destination status are defined in ADR 26. Subscriptions are revoked, never deleted.

**Reconciliation.** JetStream may lose, expire or give up on a message, and a publisher may fail
permanently. Every minute the consumer process runs `delivery.Reconcile`, which applies the same event-time rule and
creates the missing deliveries for events received between 48 h and 2 min ago (limit 500 per sweep). The grace period leaves
recent events to the normal path. It is idempotent and safe to run from several processes.

## Guarantees

- Every accepted event is eventually fanned out as long as PostgreSQL survives and a relay process runs
  within the lookback window. The broker lowers latency; it is not a durability dependency.
- Messages may be duplicated at the broker, consumer and receiver level. Nothing is exactly-once.
- Ordering is not guaranteed.

## Consequences

- An event older than the lookback whose message and fan-out were both lost is not repaired automatically.
- NATS needs a persistent volume for the file store; without it the reconciler is the only safety net.
- Tests use an embedded `nats-server`; CI also starts a real JetStream container. A server rejects streams
  with overlapping subjects, so every test stream has its own subject (`Config.Subject`).
