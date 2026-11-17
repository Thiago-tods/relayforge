# Changelog

All notable changes to this project are documented in this file.
The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project
adheres to [Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added

- Phase 0 project foundation: Go backend skeleton (`api`, `outbox-publisher`, `delivery-worker`)
  with configuration loading, structured logging, HTTP server, graceful shutdown and health endpoints.
- React + TypeScript + Vite frontend shell with routing, ESLint, Prettier and Vitest.
- Docker Compose local infrastructure for PostgreSQL, Redis and NATS JetStream.
- GitHub Actions CI and Dependabot configuration.
- Project documentation.
