# Development notes

## Toolchain

See the prerequisites in the [README](../../README.md).

## Common tasks

| Task                  | Command                                                              |
| --------------------- | -------------------------------------------------------------------- |
| Format Go code        | `gofmt -w .`                                                         |
| Run Go tests          | `go test ./cmd/... ./internal/...`                                                      |
| Run frontend tests    | `cd web; npm test`                                                   |
| Format frontend       | `cd web; npm run format`                                             |
| Validate Compose file | `docker compose --env-file deploy/.env.example -f deploy/docker-compose.yml config` |
| Apply migrations      | `go run ./cmd/migrate up` (needs `RELAYFORGE_DATABASE_URL`)          |
| Migration status      | `go run ./cmd/migrate status`                                        |
| Integration tests     | set `RELAYFORGE_TEST_DATABASE_URL`, then `go test -count=1 ./cmd/... ./internal/...` |
| Stop infrastructure   | `docker compose --env-file deploy/.env -f deploy/docker-compose.yml down` |

Add `-v` to `down` only if you want to delete the persistent data volumes.

## Health endpoints

Every Go process serves `GET /healthz` and `GET /readyz` on its HTTP address.

## Writing migrations

1. Add `internal/database/migrations/NNNNN_description.sql` with the next sequential number.
2. Use `-- +goose Up` and, only when reversal is safe and lossless, `-- +goose Down`. Wrap statements
   containing `;` inside bodies (functions, triggers) in `-- +goose StatementBegin` / `StatementEnd`.
3. Never edit a migration that has been merged; add a new one instead.
4. Run `go run ./cmd/migrate up` twice locally: the second run must report no pending migrations.

## Integration tests

Integration tests use `internal/database/dbtest`, which creates a uniquely named database per test on
the server in `RELAYFORGE_TEST_DATABASE_URL` and drops it at cleanup. Use a disposable server.
`CI=true` or `RELAYFORGE_REQUIRE_INTEGRATION=1` turn a missing test database into a failure.

## Working on identity and tenancy

- Add a permission by extending the matrix in `internal/tenancy/role.go` and its table-driven test,
  then call `authorize` or `lockAndAuthorize` in `internal/app` before touching data.
- Operations that change memberships must start with `lockAndAuthorize` (row lock on the
  organization) and write their audit event in the same transaction.
- Every new tenant-owned table must be reachable only after `ActiveMembership` succeeds, and needs a
  cross-tenant test.
- Use `apptest.New(t)` for integration tests: it creates a disposable migrated database and a
  service with cheap Argon2 parameters and a controllable clock.
- Never log or return password hashes, session, CSRF or invitation tokens.

## Working on ingestion

- `apptest.Env` also provides `CreateEndpoint`, `Signed` (a correctly signed request using the fake
  clock) and `Send` (runs `Lookup` then `Accept` like the HTTP handler).
- Keep the request path ordered as in ADR 0017; do not parse or normalize the body before the signature
  is verified, and never log bodies, signatures or secrets (there is a test that scans the logs).
- Any new write on the accepted-event path must happen inside the same transaction as the event and
  outbox rows.
- Rate limits use `ratelimit.Hit` (count every request) or `Exceeded` + `Record` (count only failures).
- Generate a local encryption key with
  `[Convert]::ToBase64String([System.Security.Cryptography.RandomNumberGenerator]::GetBytes(32))` and set
  `RELAYFORGE_ENCRYPTION_KEY` in your shell. Never commit it.

## Working on delivery (Phase 4.2)

Processes (each is a separate binary and needs PostgreSQL; none needs Docker):

| Binary | Role | Extra requirements |
|---|---|---|
| `cmd/outbox-publisher` | Publishes outbox rows to JetStream, fans messages out into deliveries, reconciles | `RELAYFORGE_NATS_URL` |
| `cmd/delivery-worker` | Claims and sends deliveries, recovers expired leases | `RELAYFORGE_ENCRYPTION_KEY` |

Configuration (all optional unless noted; durations accept `30s` or plain seconds):

| Variable | Default | Meaning |
|---|---|---|
| `RELAYFORGE_NATS_URL` | none (required by the publisher) | May embed credentials; never logged |
| `RELAYFORGE_NATS_STREAM` / `RELAYFORGE_NATS_CONSUMER` | `RELAYFORGE_EVENTS` / `delivery-fanout` | Names |
| `RELAYFORGE_DELIVERY_CONCURRENCY` | 8 (1 to 64) | Maximum simultaneous requests |
| `RELAYFORGE_DELIVERY_BATCH_SIZE` | concurrency | Deliveries claimed per poll (not above concurrency) |
| `RELAYFORGE_DELIVERY_POLL_INTERVAL` | 1s | Claim polling |
| `RELAYFORGE_DELIVERY_RECOVERY_INTERVAL` | 30s | Expired lease reaper |
| `RELAYFORGE_DELIVERY_LEASE` | 2m | At least 30s and longer than the request timeout |
| `RELAYFORGE_DELIVERY_REQUEST_TIMEOUT` | 30s | Whole request, including connect and TLS |
| `RELAYFORGE_DELIVERY_SHUTDOWN_GRACE` | 20s | Wait for in-flight attempts on shutdown |

Publisher and reconciliation tuning (batch 50, lease 1m, reconcile every 1m with a 2m grace, 48h lookback,
500 per sweep) are code defaults in `internal/relay`; change them there until a real need appears.

Metrics (enable with `RELAYFORGE_METRICS_ENABLED=true`, unauthenticated, restrict at the network level)
have fixed names and no labels: `relayforge_delivery_*`, `relayforge_outbox_*`, `relayforge_fanout_*`,
`relayforge_reconcile*`. Logs carry an `error_class`, never URLs, payloads, headers or secrets.

### Running the tests

- Everything except the PostgreSQL suites runs with `go test ./...`. NATS tests start an **embedded**
  `nats-server` on a random loopback port with a temporary store: no Docker, no installed server.
- To use a real server instead, set `RELAYFORGE_TEST_NATS_URL` (CI does).
- PostgreSQL tests need `RELAYFORGE_TEST_DATABASE_URL`. On Windows without Docker use a portable
  PostgreSQL 17 (a zip from EnterpriseDB), `initdb`, `pg_ctl start -o "-p 54329"`, then point the URL at
  it. Stop it when you are done.
- The race detector needs CGO and a C compiler on Windows (for example w64devkit's `gcc` on `PATH`):
  `$env:CGO_ENABLED = "1"; go test -race -count=1 ./...`.
- No test reaches the public internet: the egress client takes a fake resolver and tests use local TLS servers.

### Limitations of this phase

- There is no HTTP API yet to create destinations or subscriptions. `delivery.Subscribe` and
  `delivery.RevokeSubscription` implement the lifecycle (ADR 0026); the API and its authorization come next.
- Destinations on private addresses are not supported (ADR 0023).
- Delivery is at-least-once with no ordering guarantee. Receivers must deduplicate on
  `X-RelayForge-Delivery-Id` (ADR 0022 and 0024).
- Events older than the reconciliation lookback whose broker message was lost are not repaired automatically.
- Secret rotation for destinations has a schema column but no operation yet.
