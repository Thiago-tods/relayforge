# Security Policy

## Reporting a vulnerability

Please do not open a public issue for security problems. Report them privately using GitHub's
"Report a vulnerability" feature (private security advisories) on this repository. Include a
description, reproduction steps and the affected version or commit.

We aim to acknowledge reports within 5 business days.

## Handling secrets

- Never commit secrets, credentials, tokens or private keys.
- Local credentials live in `deploy/.env`, which is git-ignored. Use `deploy/.env.example` as a template.
- Report any accidentally committed secret immediately so it can be rotated.

## Supported versions

RelayForge is pre-release. Only the `main` branch receives security fixes.

## Authentication hardening status

Implemented: Argon2id password hashing, opaque revocable sessions, hardened cookies, CSRF tokens,
generic login failures with equalized timing, atomic PostgreSQL-backed failed-login reservations per
account and client address, tenant isolation, RBAC, one-time expiring invitations and append-only
audit events (ADRs 0008 to 0014 and 0021).

**Not implemented (follow-ups):**

- Email verification, password reset and MFA.
- Breached-password screening.
- Automated invitation delivery (tokens are returned once to the inviting caller).


## Frontend dependency audit policy

CI fails on high-severity advisories in production frontend dependencies (`npm audit --omit=dev`).
Development-only tooling is audited in an informational step that does not block the build, because
advisories there can be published before a patched release exists (for example `picomatch`
GHSA-fc5q-39xc-vjpf on 2026-11-17, no fixed version at the time). Re-enable blocking on all
dependencies once a patched release is available.

## Webhook ingestion security (Phase 3)

Implemented: HMAC-SHA256 request signing with constant-time verification, strict timestamp window
(5 minutes), PostgreSQL-enforced idempotency, signing secrets encrypted with AES-256-GCM and bound to
their endpoint, secrets shown once, 1 MiB body limit, shared rate limits in PostgreSQL for ingestion
and for failed logins (ADR 0021), and an atomic event + outbox commit (ADRs 0015 to 0021).

**Known limitations (read before exposing the API publicly):**

- **Public production readiness is not claimed.** Several controls below are missing.
- **Client address behind proxies.** Per-address limits use the TCP peer address and ignore
  `X-Forwarded-For`. Behind a reverse proxy all clients share one address, so those limits become
  global. A trusted-proxy setting is not implemented.
- **No TLS in the application.** Terminate TLS in front of it. Without TLS, secrets, signatures and
  session cookies cross the network in clear text.
- **The idempotency key is not signed** (ADR 0018): a captured request can be replayed with a new key
  inside the five-minute window.
- **The encryption key** is a single static key (ADR 0016). If lost, all signing secrets are
  unrecoverable; if leaked together with a database copy, all secrets are exposed. Key rotation
  tooling is not implemented.
- **Account lockout** is possible for anyone who knows an email address (failed-login counters).
- **Fixed rate-limit windows** allow bursts of up to twice the limit across a window boundary.
- **No distributed denial-of-service protection**, no web application firewall.
- **`/metrics` is unauthenticated** and disabled by default; keep it on a private network.
- **Events are never deleted** and the outbox is never drained: there is no retention policy or
  publisher yet.
- Email verification, password reset, MFA and automated invitation delivery remain unimplemented.

## Outbound delivery security (Phase 4.2)

- Destination URLs are untrusted. Every send re-validates the URL, resolves the host once, rejects the request
  if **any** answer is a non-public address, and connects to the validated IP (DNS pinning). Proxies and
  redirects are never followed (ADR 0023).
- Destination secrets are encrypted at rest (AES-256-GCM, domain-separated associated data) and used only to
  sign. Requests carry a timestamped HMAC over delivery id, event id and body (ADR 0022).
- Logs and metrics never contain destination URLs, payloads, headers, secrets or response bodies.
- Delivery is at-least-once: receivers must deduplicate on `X-RelayForge-Delivery-Id`.
- Run workers with a network policy that blocks private ranges as a second layer; the application check
  is not a substitute for it.
