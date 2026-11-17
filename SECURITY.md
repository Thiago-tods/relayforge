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
