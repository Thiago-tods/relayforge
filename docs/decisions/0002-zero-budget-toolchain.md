# 2. Zero-budget toolchain

Date: 2026-11-17

## Status

Accepted

## Context

The project must be built and operated with a zero-dollar budget.

## Decision

Use only free and open-source software: Go, React, Vite, PostgreSQL, Redis, NATS JetStream and
Docker Compose, plus GitHub Actions free tier. No cloud resources are provisioned and no paid APIs
or dependencies are introduced.

## Consequences

Local development relies on Docker Compose. Any future dependency must be free to use.
