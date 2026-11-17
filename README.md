# RelayForge

RelayForge is a webhook infrastructure platform that receives, persists, processes, delivers,
retries and monitors webhook events reliably.

> **Status:** Phase 4.2 (reliable webhook delivery engine: signed outbound delivery, retries, NATS JetStream relay). Destination and subscription management APIs are not implemented yet. Delivery is at-least-once.

## Architecture overview

RelayForge is a **modular monolith with independent workers**:

| Component                 | Purpose                                         | Default port |
| ------------------------- | ----------------------------------------------- | ------------ |
| `cmd/api`                 | HTTP API (`/api/v1`, health endpoints)          | 8080         |
| `cmd/outbox-publisher`    | Publishes outbox records to NATS JetStream      | 8081         |
| `cmd/delivery-worker`     | Delivers webhooks and handles retries           | 8082         |
| `cmd/migrate`             | Applies and reports database migrations (CLI)   | n/a          |
| `web/`                    | React + TypeScript + Vite console               | 5173         |

Infrastructure: PostgreSQL (source of truth), Redis (caching and coordination), NATS JetStream
(durable messaging). Every Go process exposes `GET /healthz` (liveness) and `GET /readyz` (readiness).

### Identity, tenancy and access control

- **Users** are global identities (`users`). One user can belong to many organizations.
- **Organizations** are independent tenants. Access exists only through an active **membership**
  carrying one of four roles: `OWNER`, `ADMIN`, `DEVELOPER`, `VIEWER`. Knowing an organization ID
  grants nothing; non-members get the same `404` as for a missing organization.
- **Authentication** is first-party email and password. Passwords are hashed with Argon2id
  (64 MiB, 3 iterations, 2 lanes; 12 to 128 characters). Sessions are opaque 256-bit random tokens;
  only their SHA-256 hash is stored. They last 12 hours in absolute terms and can be revoked
  immediately.
- **Cookies:** `relayforge_session` is `HttpOnly`, `SameSite=Lax`, `Path=/api`, expires with the
  session, and is `Secure` whenever `RELAYFORGE_COOKIE_SECURE` is true (the default in production,
  where disabling it is rejected at startup).
- **CSRF:** every state-changing authenticated request needs the `X-CSRF-Token` header (a per-session
  token whose hash is stored). Register and login return it; `POST /api/v1/auth/csrf` issues a new
  one.
- **Invitations:** an authorized member creates an invitation for an email and role and receives the
  raw token once; delivering it is the caller's job because no email service is used. It expires in
  7 days, can be revoked, works once, and only for the invited email.
- **Audit:** security-sensitive actions are recorded in an append-only table in the same transaction
  as the change.

RBAC matrix ("Limited" for ADMIN: only DEVELOPER and VIEWER members, only DEVELOPER and VIEWER roles):

| Operation | OWNER | ADMIN | DEVELOPER | VIEWER |
|---|---|---|---|---|
| View organization, list members | Yes | Yes | Yes | Yes |
| Update organization | Yes | Yes | No | No |
| Invite members | Yes | Limited | No | No |
| List and revoke invitations | Yes | Yes | No | No |
| Change roles, remove members | Yes | Limited | No | No |
| Transfer ownership | Yes | No | No | No |
| View audit events | Yes | Yes | No | No |

Each organization always has exactly one `OWNER`; ownership only changes through
`POST /api/v1/organizations/{id}/transfer-ownership`.

Not implemented yet: email verification, password reset, MFA, automated invitation email,
leaving an organization, and organization deletion (see [SECURITY.md](SECURITY.md)).

### Webhook ingestion

An **ingestion endpoint** is a RelayForge URL that receives signed events from an external producer.
It is not a delivery destination; outbound delivery, retries and the outbox publisher come later.

Accepting an event verifies the HMAC signature and timestamp, enforces idempotency, and writes the
event and its transactional outbox record in **one PostgreSQL transaction** before answering `202`.
Nothing publishes the outbox yet, so outbox rows stay unpublished for now.

**Create an endpoint** (needs a session and CSRF token, see below; OWNER, ADMIN and DEVELOPER may do it):

```powershell
curl.exe -s -b cookies.txt -H "X-CSRF-Token: <token>" -H "Content-Type: application/json" `
  -d '{"name":"Orders"}' "$base/organizations/<organization-id>/ingestion-endpoints"
```

The response contains `endpoint.publicId` (used in the URL) and `endpoint.signingSecret`. **The secret
is shown only in this response and when you rotate it.** It is stored encrypted with AES-256-GCM and
cannot be read back. Rotate with `POST .../ingestion-endpoints/{id}/rotate-secret` (OWNER and ADMIN);
the old secret stops working as soon as the rotation commits. `DELETE` revokes an endpoint
permanently; `POST .../pause` and `.../activate` are reversible.

**Request format.** `POST /api/v1/ingest/{publicId}` with a JSON body of at most 1 MiB and these headers:

| Header | Value |
|---|---|
| `Content-Type` | `application/json` |
| `X-RelayForge-Timestamp` | Unix time in seconds (must be within 5 minutes of the server clock) |
| `X-RelayForge-Signature` | lowercase hex of `HMAC-SHA256(secret, timestamp + "." + raw_body)` (64 characters) |
| `Idempotency-Key` | 1 to 255 printable ASCII characters, unique per logical event |

The key for the HMAC is the secret string itself (UTF-8). The signature covers the **exact bytes** you
send, so sign the final body, not a JSON object you serialize again later.

PowerShell (all values are fake; replace them):

```powershell
$base      = "http://127.0.0.1:8080/api/v1"
$publicId  = "ep_EXAMPLEexampleEXAMPLE12"      # endpoint.publicId
$secret    = "rfs_EXAMPLE-not-a-real-secret"   # endpoint.signingSecret
$body      = '{"event":"order.created","id":42}'
$timestamp = [string][DateTimeOffset]::UtcNow.ToUnixTimeSeconds()

$hmac      = [System.Security.Cryptography.HMACSHA256]::new([Text.Encoding]::UTF8.GetBytes($secret))
$bodyBytes = [Text.Encoding]::UTF8.GetBytes($body)
$message   = [Text.Encoding]::UTF8.GetBytes("$timestamp.") + $bodyBytes
$signature = ([BitConverter]::ToString($hmac.ComputeHash($message)) -replace '-', '').ToLowerInvariant()

Invoke-WebRequest -Method Post -Uri "$base/ingest/$publicId" -UseBasicParsing `
  -ContentType 'application/json' -Body $bodyBytes `
  -Headers @{ 'X-RelayForge-Timestamp' = $timestamp; 'X-RelayForge-Signature' = $signature; 'Idempotency-Key' = 'order-42' }
```

curl and OpenSSL (Linux, macOS, Git Bash):

```bash
BASE=http://127.0.0.1:8080/api/v1; PUBLIC_ID=ep_EXAMPLEexampleEXAMPLE12; SECRET=rfs_EXAMPLE-not-a-real-secret
BODY='{"event":"order.created","id":42}'; TS=$(date +%s)
SIG=$(printf '%s.%s' "$TS" "$BODY" | openssl dgst -sha256 -hmac "$SECRET" -hex | sed 's/^.* //')
curl -i -X POST "$BASE/ingest/$PUBLIC_ID" -H "Content-Type: application/json" \
  -H "X-RelayForge-Timestamp: $TS" -H "X-RelayForge-Signature: $SIG" -H "Idempotency-Key: order-42" \
  --data-binary "$BODY"
```

**Idempotency.** The key is scoped to the endpoint. Same key and same payload: `202` with the original
event ID and `"duplicate": true`, nothing new is stored. Same key and a different payload: `409
idempotency_conflict`. Different endpoints can reuse keys. Keys are kept as long as their events,
which are never deleted in this phase. Concurrent duplicates create exactly one event.

**Responses.**

| Status | Code | Meaning |
|---|---|---|
| 202 | | Accepted: `{"event":{"id":"...","status":"RECEIVED"},"duplicate":false}` |
| 400 | `invalid_timestamp`, `invalid_signature`, `invalid_idempotency_key`, `invalid_json` | Missing or malformed input |
| 401 | `signature_mismatch`, `timestamp_out_of_window` | Authentication failed |
| 404 | `not_found` | Unknown or revoked endpoint |
| 409 | `idempotency_conflict` | Key reused with a different payload |
| 413 | `request_too_large` | Body over 1 MiB |
| 415 | `unsupported_media_type` | Not `application/json` |
| 429 | `rate_limited` | Retry after the `Retry-After` seconds |
| 503 | `endpoint_paused` | The endpoint is paused; retry later |

**Encryption key.** The API requires `RELAYFORGE_ENCRYPTION_KEY`, the base64 of exactly 32 random
bytes, and refuses to start without it. Generate one and keep it outside the repository:

```powershell
[Convert]::ToBase64String([System.Security.Cryptography.RandomNumberGenerator]::GetBytes(32))
```

Back the key up **separately from database backups**. If it is lost, no stored signing secret can be
decrypted and every endpoint needs a new secret. See [ADR 0016](docs/decisions/0016-signing-secret-encryption.md).

**Reading events.** `GET /api/v1/organizations/{id}/events` lists event metadata (filters `endpointId`,
`status`, `from`, `to` as RFC 3339; pagination). Payloads are returned only by
`GET .../events/{eventId}`, for OWNER, ADMIN and DEVELOPER; VIEWER sees metadata only.

**Rate limiting.** Limits are counted in PostgreSQL, so they hold across API instances: ingestion
300 requests per minute per client address and 600 per endpoint, and failed logins 10 per account and
30 per address per 15 minutes. Login attempts reserve both budgets atomically before password
verification; invalid credentials keep the reservation, while successful logins release it in the
session transaction. This prevents concurrent requests from racing through a stale pre-check. See
[ADR 0021](docs/decisions/0021-rate-limiting.md) for limits, settings and caveats (notably: behind a
proxy the client address is the proxy).

**Metrics.** Set `RELAYFORGE_METRICS_ENABLED=true` to serve Prometheus-format counters and latency
histograms at `GET /metrics`. The endpoint is unauthenticated and off by default: expose it only on a
trusted network.

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
| `RELAYFORGE_SESSION_LIFETIME`    | `12h`         | Absolute session lifetime (max 30d)  |
| `RELAYFORGE_COOKIE_SECURE`       | `true` in production, else `false` | `Secure` attribute of the session cookie |
| `RELAYFORGE_ENCRYPTION_KEY`      | none (required by `api`) | Base64 of 32 bytes; encrypts signing secrets |
| `RELAYFORGE_INGEST_RATE_PER_ENDPOINT` | `600` | Authenticated ingestion requests per endpoint per minute |
| `RELAYFORGE_INGEST_RATE_PER_IP`  | `300`         | Ingestion requests per client address per minute |
| `RELAYFORGE_LOGIN_FAILURES_PER_ACCOUNT` | `10`   | Failed logins per account per 15 minutes |
| `RELAYFORGE_LOGIN_FAILURES_PER_IP` | `30`        | Failed logins per client address per 15 minutes |
| `RELAYFORGE_METRICS_ENABLED`     | `false`       | Serve `GET /metrics` |

Run the frontend:

```powershell
cd web
npm run dev
```

### Using the API locally

Start PostgreSQL, apply migrations and run the API (see above), then, in PowerShell (use `curl.exe`):

```powershell
$base = "http://127.0.0.1:8080/api/v1"
# Register: creates the user, a first organization and a session cookie (kept in cookies.txt).
curl.exe -s -c cookies.txt -H "Content-Type: application/json" `
  -d '{"email":"you@example.com","password":"a long local password","displayName":"You","organizationName":"My Org"}' `
  "$base/auth/register"
```

The response contains `session.csrfToken`. Send it on every state-changing request:

```powershell
curl.exe -s -b cookies.txt -H "X-CSRF-Token: <token>" -H "Content-Type: application/json" `
  -d '{"email":"teammate@example.com","role":"DEVELOPER"}' `
  "$base/organizations/<organization-id>/invitations"
```

The invitation response includes `token` once; give it to the teammate, who registers, then calls
`POST $base/invitations/accept` with `{"token":"..."}`. Other endpoints: `GET /auth/me`,
`POST /auth/login`, `POST /auth/logout`, `GET/POST /organizations`,
`GET/PATCH /organizations/{id}`, `GET /organizations/{id}/members`,
`PATCH|DELETE /organizations/{id}/members/{userId}`, `GET /organizations/{id}/invitations`,
`DELETE /organizations/{id}/invitations/{invitationId}`, `GET /organizations/{id}/audit-events`.
Collections accept `?limit=` (1 to 100, default 50) and `?offset=`. Errors look like
`{"error":{"code":"forbidden","message":"..."}}`.

Never commit cookie files or tokens: keep `cookies.txt` outside the working tree.

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
