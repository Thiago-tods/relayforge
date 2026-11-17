# 15. Ingestion endpoint model

Date: 2026-11-17

## Status

Accepted

## Context

RelayForge receives events from external producers and later delivers them to destinations. Phase 3
covers only the first half. The two ideas are easy to confuse, so they are named apart.

## Decision

- An **ingestion endpoint** is a RelayForge URL (`POST /api/v1/ingest/{publicId}`) that receives
  events from an external producer. It is **not** a delivery destination. Outbound delivery,
  retries, NATS publishing and a publisher process are out of scope for this phase.
- Each endpoint belongs to one organization (foreign key) and has a status: `ACTIVE`, `PAUSED` or
  `REVOKED`. Only `ACTIVE` endpoints accept events. Pausing is reversible. **Revocation is permanent**
  (`DELETE` means revoke): the record, its events and its audit history are kept, and a revoked
  endpoint cannot be reactivated, paused, renamed or rotated.
- The URL contains a **public identifier**, `ep_` plus 128 random bits from `crypto/rand` in
  unpadded base64url (25 characters). It never exposes the internal UUID. A check constraint pins
  the format and a unique constraint prevents collisions.
- Tenant ownership is a relational fact: `webhook_events` references
  `(ingestion_endpoint_id, organization_id)` of `ingestion_endpoints`, and `outbox` references
  `(event_id, organization_id)` of `webhook_events`, so an event can never claim another
  organization's endpoint.
- Unknown and revoked endpoints both answer `404 not_found` to unauthenticated callers. A paused
  endpoint answers `503 endpoint_paused`, so producers retry later.
- Management is organization-scoped and goes through the Phase 2 gate (session, CSRF, membership,
  permission). RBAC:

| Operation | OWNER | ADMIN | DEVELOPER | VIEWER |
|---|---|---|---|---|
| Read endpoints, list event metadata | Yes | Yes | Yes | Yes |
| Create, rename, pause, activate | Yes | Yes | Yes | No |
| Rotate secret, revoke | Yes | Yes | No | No |
| Read event payloads | Yes | Yes | Yes | No |

Rotation is limited to OWNER and ADMIN because it breaks every producer using the old secret.
Revocation is limited for the same reason and because it cannot be undone. Event listings never
contain payloads; the payload is returned only by the single-event endpoint, for roles that may read
payloads.

## Audit policy

Endpoint mutations are audited in the same transaction as the change: `ingestion_endpoint.created`,
`.updated`, `.paused`, `.activated`, `.revoked` and `.secret_rotated`. Metadata holds only names,
status transitions and the secret version, never secrets, ciphertext or signatures.

**Accepted events are not audited individually.** The `webhook_events` row (with its timestamp,
endpoint, idempotency key and payload hash) is already the durable, immutable record of the
acceptance, and it is written in the same transaction as the outbox row. A second audit row per
event would double the write volume of the hottest path, grow the audit table without bound at
ingestion rates, and bury administrative history that the audit view exists to show. The action
name `webhook_event.accepted` is therefore not emitted. Revisit if events become deletable.

## Consequences

Producers need the public identifier and the signing secret, nothing else. Delivery destinations
will be a separate entity in a later phase.
