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
generic login failures with equalized timing, tenant isolation, RBAC, one-time expiring invitations
and append-only audit events (ADRs 0008 to 0014).

**Not implemented (follow-ups):**

- **Login rate limiting and account lockout.** Argon2id with a bounded number of concurrent hashes
  limits resource use, but nothing yet slows credential guessing. A PostgreSQL-backed counter per
  account and per network address is planned; an in-memory limiter was rejected because it would
  silently stop protecting a multi-instance deployment.
- Email verification, password reset and MFA.
- Breached-password screening.
- Automated invitation delivery (tokens are returned once to the inviting caller).

Treat the API as unsuitable for exposure to the public internet until login rate limiting exists.
