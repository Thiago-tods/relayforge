# 10. CSRF protection

Date: 2026-11-17

## Status

Accepted

## Context

Sessions travel in a cookie, so browsers attach them to cross-site requests. `SameSite=Lax` blocks
most cross-site POSTs but is defence in depth, not the control.

## Decision

A synchronizer token per session:

- A random 256-bit CSRF token is created with the session. Only its SHA-256 hash is stored in
  `sessions.csrf_token_hash`.
- Registration and login return the raw token once in `session.csrfToken`.
- Every authenticated request other than GET, HEAD and OPTIONS must send it in `X-CSRF-Token`.
  The server hashes it and compares in constant time. Missing, malformed, wrong and other-session
  tokens return `403 csrf_invalid`. A request without a valid session returns `401` first.
- GET, HEAD and OPTIONS never change state in this API.
- Register and login are unauthenticated and need no token (login CSRF is accepted: it can only
  sign a victim into the attacker's own account, and the session cookie is not readable cross-site).
- `POST /api/v1/auth/logout` is idempotent: with no valid session it answers `204` without a token.

**Recovering the token after a page reload.** The raw token is not stored, so `GET /auth/session`
cannot return it, and a GET must not mutate state. `POST /api/v1/auth/csrf` issues a fresh token and
invalidates the previous one. It is the only authenticated POST exempt from the header check, so it
requires another defence: the custom header `X-Requested-With: XMLHttpRequest`, which a cross-site
form or simple request cannot set without a CORS preflight (no CORS is enabled), plus rejection of
any `Origin` that does not match the host. A cross-site attacker could at most rotate the token, not
read it.

## Consequences

Single-page clients keep the token in memory and call `POST /auth/csrf` on start-up. Rotating the
token invalidates it in other tabs of the same session.
