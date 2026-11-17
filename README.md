# RelayForge

RelayForge is a webhook infrastructure platform that receives, persists, processes, delivers,
retries and monitors webhook events reliably.

> **Status:** Phase 1 (database foundation). No business features are implemented yet.

## Architecture overview

RelayForge is a **modular monolith with independent workers**:

| Component                 | Purpose                                         | Default port |
| ------------------------- | ----------------------------------------------- | ------------ |
| `cmd/api`                 | HTTP API                                        | 8080         |
| `cmd/outbox-publisher`    | Publishes outbox records to NATS JetStream      | 8081         |
| `cmd/delivery-worker`     | Delivers webhooks and handles retries           | 8082         |
| `cmd/migrate`             | Applies and reports database migrations (CLI)   | n/a          |
| `web/`                    | React + TypeScript + Vite console               | 5173         |

Infrastructure: PostgreSQL (source of truth), Redis (caching and coordination), NATS JetStream
(durable messaging). Every Go process exposes `GET /healthz` (liveness) and `GET /readyz` (readiness).

### PostgreSQL

PostgreSQL 17 is the source of truth. Go code uses `pgx` v5 with a single `pgxpool` per process
(`internal/database`), and schema changes are versioned SQL files embedded in the binary and applied
with [goose](https://github.com/pressly/goose) through `cmd/migrate`. Only `api` connects today; its
`/readyz` includes a `postgres` check that returns 503 when the database is unreachable, while
`/healthz` never depends on it. The workers connect once they have database work.

See [docs/architecture](docs/architecture/README.md) and [docs/decisions](docs/decisions/).

## Prerequisites

- [Go](https://go.dev/dl/) 1.27 or newer
- PostgreSQL 17 (provided by Docker Compose, or any disposable local instance)
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

### Database configuration and migrations

Copy the application settings from `deploy/.env.example` into your shell. In PowerShell:

```powershell
$env:RELAYFORGE_DATABASE_URL = "postgres://relayforge:<your-password>@127.0.0.1:5432/relayforge?sslmode=disable"
go run ./cmd/migrate up       # apply pending migrations (safe to repeat)
go run ./cmd/migrate status   # list migrations and whether they are applied
```

On macOS or Linux use `export RELAYFORGE_DATABASE_URL=...`. Never commit real credentials.

| Variable                            | Default | Description                                   |
| ----------------------------------- | ------- | --------------------------------------------- |
| `RELAYFORGE_DATABASE_URL`           | none    | PostgreSQL URL (required by `api` and `migrate`) |
| `RELAYFORGE_DB_MAX_CONNS`           | `10`    | Maximum pooled connections                    |
| `RELAYFORGE_DB_MIN_CONNS`           | `0`     | Connections kept open (must not exceed max)   |
| `RELAYFORGE_DB_MAX_CONN_LIFETIME`   | `1h`    | Maximum connection age                        |
| `RELAYFORGE_DB_MAX_CONN_IDLE_TIME`  | `30m`   | Maximum idle time                             |
| `RELAYFORGE_DB_HEALTH_CHECK_PERIOD` | `1m`    | Background idle-connection check interval     |
| `RELAYFORGE_DB_CONNECT_TIMEOUT`     | `10s`   | Bound for the initial connect and ping        |

Durations accept Go syntax (`30s`) or plain seconds (`30`).

Run the backend processes (each in its own terminal). `api` requires `RELAYFORGE_DATABASE_URL`:

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
go test ./cmd/... ./internal/...      # unit tests; integration tests skip without a test database
cd web
npm run format:check; npm run lint; npm run typecheck; npm test; npm run build
```

## Integration tests

Integration tests need a **disposable** PostgreSQL 17 server and a role with `CREATEDB`. Each test
creates its own uniquely named database and drops it afterwards, so no existing database is touched.
With the Compose PostgreSQL running:

```powershell
$env:RELAYFORGE_TEST_DATABASE_URL = "postgres://relayforge:<your-password>@127.0.0.1:5432/postgres?sslmode=disable"
go test -count=1 ./cmd/... ./internal/...
```

Without the variable the tests skip locally. They fail instead of skipping when `CI=true` or
`RELAYFORGE_REQUIRE_INTEGRATION=1`. CI runs them against a PostgreSQL 17 service container.

## Documentation

- [Contributing](CONTRIBUTING.md)
- [Security policy](SECURITY.md)
- [Changelog](CHANGELOG.md)
- [Development notes](docs/development/README.md)

## License

[MIT](LICENSE)
