# 8. Authentication and session strategy

Date: 2026-11-17

## Status

Accepted

## Context

Phase 2 introduces user identity. The project has a zero-dollar budget, so no hosted identity
provider or email service can be used, and authentication must work fully locally.

## Decision

- Users are **global identities** (`users`). A user can belong to many organizations. Access to an
  organization is granted only by a membership (ADR 0011), never by a role on the user and never by
  knowing an organization ID.
- First-party email and password authentication.
- **Opaque server-side sessions**, not JWTs. A session is a 256-bit random token from `crypto/rand`,
  encoded as unpadded base64url (43 characters). Only its SHA-256 hash is stored in `sessions`.
  Opaque sessions can be revoked immediately; a stateless token cannot, and this application has no
  need for stateless verification across services.
- **Absolute lifetime of 12 hours** (`RELAYFORGE_SESSION_LIFETIME`, capped at 30 days). There is no
  sliding expiration: activity never extends a session. `last_seen_at` is informational and is
  updated at most every 5 minutes to limit writes.
- Revocation sets `revoked_at`; the very next request fails. Logout revokes only the current session.
- The token travels in a cookie named `relayforge_session` with `HttpOnly`, `SameSite=Lax`,
  `Path=/api`, an explicit `Expires` and `Max-Age`, and `Secure` whenever
  `RELAYFORGE_COOKIE_SECURE` is true. It defaults to true in production, and a production
  configuration with `false` is rejected at startup.
- Authentication rejects, with an identical `401 unauthenticated`: missing, malformed, unknown,
  revoked and expired tokens, and disabled users.
- Login failures (unknown email, wrong password, malformed email, disabled account, oversized
  password) return the same `401 invalid_credentials` response. For unknown accounts the server
  verifies the password against a dummy Argon2id hash so response time does not reveal whether the
  account exists.
- Registration returns `409 email_taken` for an existing email. That reveals existence, which is the
  usual trade-off for sign-up forms; email verification is out of scope for this phase.

## Not implemented (deferred)

Email verification, password reset, MFA, session
listing, "log out everywhere" and password change. None of them is claimed to exist.

Login throttling was added in Phase 3 (ADR 0021).

## Consequences

Every authenticated request costs one indexed database lookup. Raw tokens exist only in the client
cookie and in the one response that creates them; they are never logged, audited or stored.
