# 27. Destination and subscription management API

Date: 2026-11-18

## Status

Accepted. Builds on ADR 22 (signing), ADR 23 (egress), ADR 24 (delivery lifecycle) and ADR 26 (subscriptions).

## Context

Phase 4.2 delivered the engine but left destinations and subscriptions as database rows. Phase 4.3 exposes
them, and the delivery history, over the existing `/api/v1` API. Several rules had to be decided because the
earlier ADRs only covered the engine.

## Decision

**Same shape as ingestion endpoints.** Handlers call `app.Service` methods; each mutation runs in one
transaction that takes the organization lock (`lockAndAuthorize`), locks the resource row, applies the change
and writes the audit event. Permissions are centralized in `internal/tenancy`:

| Permission | OWNER | ADMIN | DEVELOPER | VIEWER |
|---|---|---|---|---|
| destinations read | yes | yes | yes | yes |
| destinations create, update (name, URL), pause and activate | yes | yes | yes | no |
| destinations rotate secret, revoke | yes | yes | no | no |
| subscriptions read | yes | yes | yes | yes |
| subscriptions create and revoke | yes | yes | yes | no |
| deliveries read (metadata and attempts) | yes | yes | yes | yes |

Rotation and revocation stay with OWNER and ADMIN for the same reason as for ingestion endpoints: rotation
breaks every receiver still verifying the old secret, and revocation is permanent.

**Authorization comes before input validation** for create and update, so a caller without permission learns
nothing about validation rules. (The ingestion endpoint use cases validate first; they are unchanged.)

**Destination URLs are shown without their query string.** A webhook URL often carries a secret token in its
query. API responses and audit metadata show the URL without the query and add `urlHasQuery`. The full URL
exists only in the database and in memory of the delivery worker. Validation errors carry a reason code and
never echo the submitted URL.

**Lifecycle.** ACTIVE, PAUSED, REVOKED. Pause and activate are idempotent. REVOKED is permanent and total: the
application returns `409 destination_revoked` for any change, and migration 00017 makes the database refuse
any update of a revoked row, any change of identity columns, and any `secret_version` change that does not
travel with the secret: a changed ciphertext or key version requires `secret_version` + 1 exactly, and the
version never changes alone, never decreases and never jumps. Revoking
twice is a no-op. Revocation cancels the destination's PENDING deliveries in the same transaction; IN_FLIGHT
ones are left to the worker, which re-checks the destination before sending.

**URL change.** Allowed while not revoked, with the same SSRF-safe validation as creation. Pending deliveries
use the new URL on their next attempt; recorded attempts are history and never change.

**Secret rotation.** A new `rfd_` secret is sealed with the destination-specific associated data, the key
version is stored with it and `secret_version` is incremented, all under the row lock. The raw secret is
returned once. The next attempt signs with the new secret; a request already in flight may carry the old one.

**Subscriptions.** The API calls `delivery.Subscribe` and `delivery.RevokeSubscription` and adds checks taken
from the existing lifecycles, not new rules: a REVOKED endpoint or destination cannot be subscribed (permanent),
PAUSED ones can (reversible, and a paused destination keeps its pending deliveries). Revoking a destination does
not revoke its subscriptions: they become inert because fan-out ignores destinations revoked before an event was
accepted (ADR 26), and keeping them preserves the audit trail. ADR 26 semantics are unchanged.

A subscription's `status` describes only its own temporal interval `[created_at, revoked_at)`: ACTIVE means it
has not been revoked, not that its destination can receive deliveries. A subscription can therefore be ACTIVE
while its destination is REVOKED (or PAUSED); the read model exposes `destinationStatus` next to `status` so
clients can tell the two apart. The system never revokes subscriptions automatically, and the subscription
can still be revoked explicitly.

**Delivery history is read-only.** Only GET routes exist. The read model exposes operational metadata and the
attempts of one delivery (number, times, duration, outcome, HTTP status, error class). It never exposes the
payload, the destination URL, response bodies, raw error text, or claim fields (token, worker, lease). Attempts
are read before the delivery row, so a response never lists more attempts than `attemptCount`.

**Tenant isolation.** Every query includes the organization id, and a resource of another organization yields
the same `404 not_found` as a missing one, including when it is named in a request body (subscription creation).

## Consequences

- Subscriptions to a revoked destination remain listed as ACTIVE until revoked explicitly. The listing shows
  the destination's status.
- Destination names are unique per organization, case-insensitively, among non-revoked destinations (00011).
- There is no delivery replay or manual retry endpoint; those need their own design.
- Migration 00017 adds only a trigger and an organization index for subscription listing; no table changes.
