# RelayForge

RelayForge is a webhook infrastructure platform that receives, persists, processes, delivers,
retries and monitors webhook events reliably.

> **Status:** Phase 0 (project foundation). No business features are implemented yet.

## Architecture overview

RelayForge is a **modular monolith with independent workers**:

| Component                 | Purpose                                         | Default port |
| ------------------------- | ----------------------------------------------- | ------------ |
| `cmd/api`                 | HTTP API                                        | 8080         |
| `cmd/outbox-publisher`    | Publishes outbox records to NATS JetStream      | 8081         |
| `cmd/delivery-worker`     | Delivers webhooks and handles retries           | 8082         |
| `web/`                    | React + TypeScript + Vite console               | 5173         |

Infrastructure: PostgreSQL (source of truth), Redis (caching and coordination), NATS JetStream
(durable messaging). Every Go process exposes `GET /healthz` (liveness) and `GET /readyz` (readiness).

See [docs/architecture](docs/architecture/README.md) and [docs/decisions](docs/decisions/).

## Prerequisites

- [Go](https://go.dev/dl/) 1.27 or newer
- [Node.js](https://nodejs.org/) 22 or newer (with npm)
- [Git](https://git-scm.com/)
- [Docker Desktop](https://www.docker.com/products/docker-desktop/) with Docker Compose v2 (for local infrastructure)

All tooling is free and open source.

## Installation

```powershell
git clone <repository-url> RelayForge
cd RelayForge
go mod download
cd web
npm ci
cd ..
```

## Local development

Start local infrastructure (PostgreSQL, Redis, NATS JetStream):

```powershell
Copy-Item deploy/.env.example deploy/.env   # then edit the passwords
docker compose --env-file deploy/.env -f deploy/docker-compose.yml up -d
docker compose --env-file deploy/.env -f deploy/docker-compose.yml ps
```

Infrastructure ports are bound to `127.0.0.1` only. `deploy/.env` is git-ignored.

Run the backend processes (each in its own terminal):

```powershell
go run ./cmd/api
go run ./cmd/outbox-publisher
go run ./cmd/delivery-worker
```

Configuration is read from environment variables:

| Variable                         | Default       | Description                          |
| -------------------------------- | ------------- | ------------------------------------ |
| `RELAYFORGE_ENV`                 | `development` | `development`, `test`, `production`  |
| `RELAYFORGE_LOG_LEVEL`           | `info`        | `debug`, `info`, `warn`, `error`     |
| `RELAYFORGE_LOG_FORMAT`          | `json`        | `json` or `text`                     |
| `RELAYFORGE_HTTP_ADDR`           | per process   | HTTP listen address                  |
| `RELAYFORGE_SHUTDOWN_TIMEOUT`    | `15s`         | Graceful shutdown limit              |
| `RELAYFORGE_READ_HEADER_TIMEOUT` | `5s`          | HTTP header read timeout             |

Run the frontend:

```powershell
cd web
npm run dev
```

## Quality checks

```powershell
gofmt -l .
go vet ./cmd/... ./internal/...
go test ./cmd/... ./internal/...
cd web
npm run format:check; npm run lint; npm run typecheck; npm test; npm run build
```

## Documentation

- [Contributing](CONTRIBUTING.md)
- [Security policy](SECURITY.md)
- [Changelog](CHANGELOG.md)
- [Development notes](docs/development/README.md)

## License

[MIT](LICENSE)
