# 26. Subscription lifecycle and event-time eligibility

Date: 2026-11-17

## Status

Accepted. Refines ADR 25 (fan-out) and replaces the uniqueness rule of migration 00015.

## Context

Review of the first fan-out implementation found two defects:

1. Migration 00015 made `(ingestion_endpoint_id, destination_id)` unconditionally unique, so a pair could
   never be subscribed again after a revocation.
2. Fan-out and reconciliation selected subscriptions by their **current** `status = 'ACTIVE'`. An event
   accepted while a subscription was active silently lost its delivery if the subscription was revoked
   before the fan-out message was processed (or before the reconciliation sweep ran), which breaks the
   promise that PostgreSQL-accepted events are delivered.

## Decision

**A subscription record is one lifecycle interval** `[created_at, revoked_at)`; `revoked_at IS NULL` means
it is still active. No new columns are needed: `created_at` is the start and `revoked_at` the end.

**Event-time rule.** A subscription applies to an event when the event was accepted inside its interval:

```
s.created_at <= e.received_at AND (s.revoked_at IS NULL OR s.revoked_at > e.received_at)
```

Start inclusive, end exclusive, so two back-to-back intervals (revoke and re-subscribe at the same
instant) cover an instant exactly once. The current `status` is not consulted. `FanOut` and `Reconcile`
share one SQL fragment (`activeAtAcceptance`), so the normal path and the safety net cannot disagree.
Both timestamps are `timestamptz` set by `now()`, which is the transaction start time.

| Situation | Delivery |
|---|---|
| Revoked after acceptance, before fan-out or reconciliation | Created |
| Created after acceptance | Not created |
| Revoked before acceptance, or created and revoked before acceptance | Not created |
| Active at acceptance, later revoked and re-subscribed | Created (by the old interval) |
| Accepted in the gap between two intervals | Not created |

**Re-subscription** creates a new record. A revoked record is never reactivated or rewritten.

**Uniqueness.** At most one ACTIVE subscription per pair, enforced by the partial unique index
`subscriptions_one_active_per_pair_idx ... WHERE status = 'ACTIVE'`. Concurrent `Subscribe` calls: exactly
one succeeds, the rest get `ErrAlreadySubscribed`. `RevokeSubscription` locks the row (`FOR UPDATE`); a
concurrent `Subscribe` either sees the active row (and fails) or waits for the revocation to commit (and
succeeds), both valid, never a duplicate.

**Database guarantees (migration 00016).** `revoked_at >= created_at`; a trigger rejects any update of a
REVOKED record (even a no-op), and of the identity or `created_at` of an active one; records are never
deleted or truncated (00015); composite foreign keys keep both sides of a subscription in one organization.

**Destination eligibility is a separate concern.** Subscription revocation affects *future* events only.
A destination's own status decides whether a request is sent:

- REVOKED before the event was accepted: no delivery is created.
- REVOKED after acceptance: the delivery is created and later set to CANCELLED by the worker's
  maintenance pass, so the decision is auditable. A revoked destination is never claimed or sent to.
- PAUSED: the delivery is created and stays PENDING. It is not claimed until the destination is
  activated; nothing is cancelled or lost.

## Guarantees and limits

- Every event accepted while a subscription was active is eventually fanned out as long as the
  reconciliation lookback (48 h) has not passed. After that, nothing repairs it automatically.
- Boundaries use transaction **start** times, not commit times. If a subscription transaction starts
  before an event transaction but commits after it, the event-time comparison still uses the earlier
  transaction-start timestamp. The reverse ordering can likewise differ from commit order. The discrepancy
  can be as large as the duration of the relevant transaction(s); it is not inherently limited to milliseconds.
  This is the chosen timestamp model, and the same rule is used by fan-out and reconciliation.
- Clock steps on the database server can reorder timestamps; the database clock is assumed monotonic.
- Delivery remains at-least-once. This ADR does not change that, and nothing here is exactly-once.
