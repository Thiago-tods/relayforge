# 34. Delivery redrive: returning one FAILED delivery to the pipeline

Date: 2026-11-21

## Status

Accepted. Phase 4.4.6. Builds on ADR 12 (RBAC), ADR 14 (audit), ADR 24 (delivery lifecycle), ADR 30 (terminal failure
reasons) and ADR 32 (failure visibility), whose "redrive readiness" points this implements. Adds **migration 00022**, one
permission, one route, one worker change and one metric.

## Context

A delivery that ends `FAILED` is terminal and immutable (ADR 24). Operators had no way to send it again after fixing the
receiver, short of editing the database. A redrive must be a deliberate, audited, tenant-scoped action that reuses the
existing delivery pipeline (claim, lease, signing, egress policy, retry, recovery) and never bypasses it.

## Decision

### What a redrive is

`POST /api/v1/organizations/{organizationID}/deliveries/{deliveryID}/redrive` returns **one** `FAILED` delivery to
`PENDING`, immediately due, and raises its attempt budget. There is no bulk redrive. The delivery keeps its identity
(delivery id, event id, destination), its `attempt_count` and every attempt row. The next claim records attempt
`attempt_count + 1`, so attempt numbers keep growing without gaps or reuse. The request is sent again through the normal
worker, so everything about a send (signature with a fresh timestamp, egress and redirect policy, retries, lease
recovery, the pause of a destination) is unchanged.

Request body (strict JSON, unknown fields are `400`, at most 64 KiB):

```json
{ "reason": "the receiver deployed the fix", "expectedAttemptCount": 8 }
```

- `reason` is required: trimmed, 1 to 500 characters, valid UTF-8, no control characters. It is recorded in the audit event
  and nowhere else; it is never written to application logs.
- `expectedAttemptCount` is required (1 to 50). It is the `attemptCount` of the failure the operator looked at.

Response `200` (the transition is synchronous; the send is asynchronous):

```json
{ "delivery": { "id": "...", "status": "PENDING", "attemptCount": 8, "maxAttempts": 13, "nextAttemptAt": "...", "...": "..." },
  "redrive": { "previousMaxAttempts": 8, "maxAttempts": 13, "previousFailureReason": "RETRIES_EXHAUSTED" } }
```

`previousFailureReason` is `null` for a legacy delivery that failed before reasons were recorded; such deliveries can be
redriven. The delivery has no `failureReason` and no `completedAt` any more.

### Budget

An accepted redrive sets `max_attempts = min(old max_attempts + 5, 50)`. It raises the **total** budget by five; it does
not promise exactly five remaining attempts. A delivery that exhausted its budget (8 of 8) gets exactly five more (9 to
13); a delivery that failed permanently at attempt 1 of 8 ends up with 13 and may use attempts 2 to 13. A budget that is
already 50 is refused with `409 retry_budget_exhausted`, even when attempts remain. From the default of 8 the budget
walks 13, 18, 23, 28, 33, 38, 43, 48, 50 and then stops: nine redrives.

### Eligibility and errors

Inside one transaction, with the delivery row locked, in this order:

| Check | Result |
|---|---|
| Not a member, malformed organization or delivery id, delivery of another organization | `404 not_found` |
| Member without `deliveries:redrive` (VIEWER) | `403 forbidden` |
| Invalid body | `400 validation_failed` / `invalid_json` (only after authorization) |
| Status is not `FAILED` (pending, in flight, succeeded, cancelled, or already redriven) | `409 delivery_not_failed` |
| `attemptCount` differs from `expectedAttemptCount` | `409 delivery_changed` |
| Destination is `REVOKED` (a `PAUSED` destination is allowed) | `409 destination_revoked` |
| `max_attempts` is already 50 | `409 retry_budget_exhausted` |
| Stored payload missing, empty or not matching its SHA-256 | `409 event_payload_unavailable` |
| A database lock was not obtained within five seconds | `503 lock_timeout` with `Retry-After` |

CSRF is enforced like every mutating route (`403 csrf_invalid`). The new permission `deliveries:redrive` belongs to
OWNER, ADMIN and DEVELOPER; VIEWER stays read-only.

**Stale, repeated and later requests.** `(delivery, attemptCount)` identifies a failure episode: a redriven delivery must
be claimed (which increments the count) before it can fail again, so a new failure always has a higher count. A repeated
request after success finds the delivery `PENDING` or `IN_FLIGHT` and gets `delivery_not_failed`; there is no idempotency
replay, so the client reads the delivery instead. A late duplicate that arrives after the delivery failed *again* gets
`delivery_changed` and is never executed as a second redrive. A deliberate redrive of the second failure episode sends
the new `attemptCount` and a new reason, and adds five to the current budget. After a commit whose response was lost the
client cannot tell whether it applied: it reads the delivery (or the audit event, which is the source of truth) before
trying again.

### Payload retention and integrity

A redrive must never send a payload that is not the one that was ingested, and must never reconstruct one.
Findings (from the migrations and the code; no production database was queried):

- `deliveries (event_id, organization_id)` has a foreign key to `webhook_events (id, organization_id)` with no cascade, so
  an event cannot be deleted while a delivery references it.
- `webhook_events.payload` is `bytea NOT NULL` with a 1 B to 1 MiB check, stored exactly as received, next to a 32-byte
  `payload_sha256`. Nothing in the application updates or deletes events, and there is no retention, TTL or archival job
  (the JetStream 24 hour limit applies to the stream only).
- The outbox stores a small envelope, not the payload, and is never used to rebuild one.
- Exceptions: unlike deliveries, destinations and attempts, `webhook_events` has no trigger that blocks UPDATE or DELETE,
  so protection is the foreign key plus convention; a database administrator can alter a payload with direct SQL; and a
  future retention feature would break the assumption.

So the redrive **checks, inside the transaction and inside PostgreSQL**, that the event exists in the organization, that
`octet_length(payload) > 0` and that `sha256(payload) = payload_sha256`. The payload never reaches the application. On any
failure the redrive returns `409 event_payload_unavailable` and writes nothing. Nothing is reconstructed or substituted.

### Database: migration 00022

No table, column or index changes. The migration replaces `relayforge_enforce_delivery_transition()` and its `Down`
restores the 00012 function **exactly** (a test compares the two). A `FAILED -> PENDING` update is allowed only when all of
these hold:

- the transaction-local setting `relayforge.redrive` is `'on'`;
- `OLD.max_attempts < 50` and `NEW.max_attempts = LEAST(OLD.max_attempts + 5, 50)`;
- `attempt_count` is unchanged (it is never reset);
- `failure_reason` and `completed_at` are cleared and the claim fields stay empty.

Every other rule of 00012 is unchanged: `SUCCEEDED` and `CANCELLED` stay immutable; `FAILED` stays immutable except for
this transition; the identity columns never change and `max_attempts` changes only in a redrive; `attempt_count` moves only
on a claim. The existing constraints still apply to the new row (`PENDING` needs `attempt_count < max_attempts`, the claim
fields exist only while `IN_FLIGHT`, `completed_at` only for terminal statuses, `failure_reason` only while `FAILED`). The
attempt history is not touched: `delivery_attempts` stays append-only and the 00014 trigger still requires
`attempt_number = attempt_count` with no gap. `last_http_status` and `last_error_class` keep describing the last attempt.

**The flag is not a security boundary.** `relayforge.redrive` is an ordinary custom setting that any database session can
set for its own transaction. It prevents application code from changing a terminal delivery by accident; it does not
stop someone who can already run SQL as the application's role, and it cannot stop a database owner or superuser, who can
disable triggers (the 00014 header states the same limit). Authorization, eligibility and audit are enforced by the
service, not by the trigger. Even with the flag set by hand, the arithmetic, the unchanged attempt count and the cleared
reason are still required, and direct database modification remains outside the guarantee.

### Worker: the row's `max_attempts` is authoritative (W1)

`policyFor` used to lower the policy's limit to the row's `max_attempts` only when the row's value was smaller. The worker
policy is the hard-coded default (8 attempts), so raising a row to 13 granted nothing: attempt 9 would already have been
treated as exhausted. `policyFor` now uses the row's positive `max_attempts` as the limit. Both decision sites go through
it (claim finalization and lease recovery), so they behave the same. Ordinary deliveries are unchanged: fan-out never sets
`max_attempts`, so every ordinary row has the column default of 8, the same as the default policy. The policy's own
`MaxAttempts` is only the fallback for a row without a positive value, which the schema forbids. A later configurable
global limit would therefore no longer cap rows that carry their own value. The global ceiling of the policy is not raised.

**Retry intervals stay long.** A redriven delivery uses the ordinary attempt-number backoff (30 s doubling, capped at six
hours); there is no separate redrive policy. The first redriven attempt runs immediately, but a delivery redriven from
8 to 13 waits about 2.1 h, 4.3 h and then 6 h between its next attempts, so a full set of five extra attempts spans
roughly 18 hours. Operators should expect that.

### Concurrency and locking

One transaction does everything, in the lock order **organization (shared) -> destination (shared) -> delivery**, the same
order destination revoke uses (organization, destination, delivery rows), so the two cannot deadlock:

1. `SET LOCAL lock_timeout = '5s'` (transaction-local, never a global setting; `55P03` becomes `503 lock_timeout`).
2. `SELECT id FROM organizations ... FOR SHARE`, then the membership is read and the permission checked.
3. The delivery's `destination_id` is read without a lock (the column never changes), then the destination row is
   locked `FOR SHARE`.
4. The delivery row is locked `FOR NO KEY UPDATE` and its state is checked; the payload is checked.
5. The flag is set, one `UPDATE` moves the delivery, the flag is switched off, and the audit event is written. The
   transaction commits (READ COMMITTED) or rolls back as a whole.

**Why a shared organization lock.** Every membership change, destination mutation and revoke takes the organization row
`FOR UPDATE` first, which is what makes `lockAndAuthorize` safe: the role is read after any concurrent change has
committed. A shared lock conflicts with that exclusive lock, so the same guarantee holds (a role change or removal
committed earlier is visible; a later one cannot commit until the redrive ends), and a revoke cannot interleave with any
redrive: revoke first gives `409 destination_revoked`; redrive first lets revoke cancel the redriven `PENDING` delivery in
its own transaction. Unlike the exclusive lock, shared locks do not conflict with each other, so redrives of unrelated
deliveries in one organization run in parallel. (Ingestion does not reference the organization row directly: it locks the
ingestion endpoint row, so it was never blocked by this lock either way.)

Rejected alternatives: the exclusive organization lock (correct, but serializes every redrive of an organization); no
organization lock with a lock on the caller's membership row (it deadlocks: the audit insert takes a key-share lock on the
organization row, which a membership change holds exclusively while it waits for the membership row); advisory locks (no
membership guarantee). The shared helper (`tenancy.LockOrganizationShared`, `lockSharedAndAuthorize`) is used **only** by
the redrive flow, and a test fails if any other code uses it.

Concurrent redrives of the same delivery: the second waits on the delivery row (after the same shared locks, which do not
conflict), re-reads the committed state and gets `409 delivery_not_failed`; exactly one wins, +5 is applied once and one
audit event is written. Workers never touch `FAILED` rows (claim needs `PENDING`, recovery needs `IN_FLIGHT`), so a redrive
does not contend with them; after the commit the row is claimable at once. A paused destination keeps the delivery
`PENDING` and unclaimed until it is activated, because the claim requires an `ACTIVE` destination. PostgreSQL does not
guarantee fairness between a waiting exclusive lock and later shared requests, so a revoke or member change could in theory
wait behind a continuous stream of overlapping redrives; each lasts milliseconds and is human-triggered, and a test shows a
revoke completing while redrives run. `lock_timeout` bounds only the redrive's own waits.

### Audit and metrics

The same transaction writes `delivery.redriven` (resource type `delivery`) with: the operator `reason`, `previousStatus`,
`previousFailureReason` (`null` for a legacy delivery), `previousMaxAttempts`, `maxAttempts`, `attemptCount`,
`destinationId` and `destinationStatus`. Never a payload, a URL or a secret. The `audit_events` table has no list of
actions, so no migration is needed. Because the event is written in the transaction, the status change, the budget
increase and the audit event commit or roll back together; a test makes the audit insert fail and shows the delivery
untouched.

One new counter, `relayforge_delivery_redriven_total`, is incremented after a successful commit. Counters are best effort:
a crash between the commit and the increment loses a count, the audit event does not.

### Read models

A redriven delivery is no longer `FAILED`, so it leaves the failure feed (ADR 32) and the failed delivery list, and
re-enters them if it fails again (with a new `failureReason` and `completedAt`). The attempt feed (ADR 33) shows every
attempt of both failure episodes. Because a delivery can fail more than once, **several attempts of one delivery can carry
a `terminalReason`**, one per failure episode: `terminalReason` marks the attempt that ended an episode, and the delivery's
`failureReason` always describes the latest one (ADR 30 and ADR 24 say so too).

## Receivers

Delivery is at-least-once (ADR 24), and a redrive deliberately sends a delivery again. The request carries the same
`X-RelayForge-Delivery-Id` and event id, a higher attempt number and a fresh signature timestamp. Receivers must be
idempotent on the delivery id.

## Deployment and rollback

Order: **migration 00022, then the delivery worker (W1), then the API route.** The migration is a permissive superset of the
old function and is safe with binaries that predate it. If the API were deployed before the workers, a redriven delivery
would be sent once more and then fail early with `RETRIES_EXHAUSTED` (safe, but it wastes the redrive). Rolling back the
route disables the feature. `goose down` restores the 00012 function and is safe with redriven rows (history is kept; they
keep their raised budget and only lose the ability to be redriven again). Rolling back only the worker caps retries of
rows above 8 at 8 again, which fails safe.

## Not in this phase

Bulk redrive, redriving `CANCELLED` or `SUCCEEDED` deliveries, a separate redrive backoff, an idempotency key, a
per-organization redrive rate limit, a retention policy for events, and a measurement of the trigger function's overhead.
The new branch of the trigger only runs when the old status is `FAILED`, which is rare, but the hot path (every delivery
update runs the function) was **not benchmarked**; that is an architectural estimate, not a measured result.

## Consequences

- Operators can recover from a receiver outage without database access, and every redrive is attributable.
- The redrive is only as safe as the invariants enforced by constraints; the flag is a guard against mistakes, not
  against someone with database access.
- Free-text operator reasons are stored in the audit table and are visible to anyone with `audit:view`; they must not hold
  secrets, which is the operator's responsibility.
- A destination that keeps failing can be redriven at most nine times per delivery.
