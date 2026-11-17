# 22. Outbound webhook signing

Date: 2026-11-17

## Status

Accepted

## Context

RelayForge signs the requests it sends to customer destinations so receivers can authenticate them. The
ingestion protocol (ADR 17) signs requests *to* RelayForge with `rfs_` secrets. Reusing it would let a
signature produced for one direction be replayed in the other.

## Decision

Each destination has its own secret (`rfd_` prefix, 32 random bytes), generated server-side and sealed with
the existing AES-256-GCM secretbox. The associated data is `relayforge:destination:v1:<destination id>`,
which differs from the ingestion label `relayforge:ingestion-endpoint:v1:`, so a ciphertext cannot be moved
between the two kinds of secret or between destinations.

Every delivery request carries:

| Header | Meaning |
|---|---|
| `X-RelayForge-Delivery-Id` | Stable across retries of one delivery. Receivers deduplicate on it. |
| `X-RelayForge-Event-Id` | The accepted event. |
| `X-RelayForge-Timestamp` | Unix seconds when this attempt was signed. |
| `X-RelayForge-Signature` | `v1=` + lowercase hex HMAC-SHA256. |

The MAC input is `v1.<timestamp>.<delivery id>.<event id>.<body>`. Binding the delivery and event ids
prevents a captured body from being replayed under another delivery. Receivers should reject timestamps
outside a window of their choice (five minutes is typical) and compare signatures in constant time;
`delivery.VerifyOutbound` is the reference implementation. The timestamp is fresh on every attempt, the
delivery id is not.

## Consequences

- A leaked destination secret affects only that destination.
- Rotating a secret needs a new `secret_version`; the schema already carries it.
- The signature covers the body, not the URL; the URL is the destination's identity.
