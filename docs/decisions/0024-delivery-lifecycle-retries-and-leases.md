# 24. Delivery lifecycle, retries, leases and fencing

Date: 2026-11-17

## Status

Accepted

## Context

Delivery is at-least-once. A worker can die after the destination processed a request and before the
result is stored, and a slow worker can finish after its lease was recovered. The database schema
(migrations 00011 to 00014) provides the states, the attempt records and the triggers; this ADR fixes how
the worker uses them.

## Decision

**Lifecycle.** `PENDING -> IN_FLIGHT` (claim) `-> SUCCEEDED | PENDING (retry or expired lease) | FAILED`;
`PENDING -> CANCELLED` (destination revoked). Terminal rows never change (Go state machine and database trigger), with one controlled exception: a `FAILED` delivery can be redriven back to `PENDING` by an operator (ADR 34, migration 00022), which raises its `max_attempts` and keeps its attempt history; `SUCCEEDED` and `CANCELLED` stay immutable.

**Claim.** One short transaction selects due `PENDING` deliveries of non-paused, non-revoked destinations
with `FOR UPDATE SKIP LOCKED`, sets `IN_FLIGHT`, a new random `claim_token` and `lease_expires_at`, increments
`attempt_count`, and commits. Concurrent workers never receive the same row.

**Execute outside any transaction.** Secrets are decrypted, the request is signed and sent with no database
transaction open, so a slow destination never holds a connection or a lock.

**Finalize in one transaction.** Insert the attempt first, then the fenced update
`UPDATE ... WHERE status = 'IN_FLIGHT' AND claim_token = $token`. Zero rows means the lease was lost: the
transaction is rolled back, the result is discarded and `stale_finalizations` is counted. The attempt
trigger independently requires `IN_FLIGHT`, a matching attempt number and no gaps, so a worker bug cannot
corrupt history.

**Recovery.** A reaper finds `IN_FLIGHT` rows whose lease expired (`FOR UPDATE SKIP LOCKED`, one savepoint
per delivery), records an `ABANDONED` attempt (never a gap) and returns the row to `PENDING`, or to `FAILED`
when attempts are exhausted. The lease must exceed the request timeout (configuration enforces it).

**Retry policy.** Base 30 s, factor 2, cap 6 h, +/-20 % jitter, at most 8 attempts, bounded further by each
delivery's own `max_attempts`. `Retry-After` (seconds or HTTP-date) is honoured and clamped to the policy's
bounds. Nothing retries forever.

| Outcome | Result |
|---|---|
| 2xx | Succeeded |
| 408, 425, 429, 5xx | Retry |
| other 4xx, 3xx, 1xx | Failed, no retry |
| DNS error, connect error, timeout | Retry |
| Blocked address, invalid URL | Failed, no retry |
| TLS error | Retry (customers often fix certificates within hours) |
| Context cancelled by shutdown | Recorded at once as `ABANDONED` (`shutdown_interrupted`), due again in 5 s (ADR 28); if that write fails, the lease expires and the reaper records `ABANDONED` |

**Shutdown.** The worker stops claiming, waits up to `RELAYFORGE_DELIVERY_SHUTDOWN_GRACE` for in-flight
attempts, then cancels them. The pool is closed only after the worker has returned. ADR 28 changes what
happens to a cancelled request: it is recorded immediately instead of being left to the lease.

## Consequences

- Receivers see duplicates after crashes or lost responses. `X-RelayForge-Delivery-Id` is stable for that purpose.
- A crash costs at most one lease period of delay for the affected deliveries.
- This is **not** exactly-once delivery and the documentation must never claim it.
