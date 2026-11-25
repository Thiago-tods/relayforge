# Changelog

All notable changes to this project are documented in this file.
The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project
adheres to [Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added (Phase F3a: destination management in the web UI)

- `web/`: the Destinations pages at `/o/:orgId/destinations` (list) and `/o/:orgId/destinations/:destinationId` (detail),
  under a new "Configuration" sidebar group (shown with `destinations:read`). "Subscriptions" in that group is a
  non-link "coming soon" label: the page belongs to a later phase.
- List: name, URL without its query, status, created and updated, filter All, Active, Paused or Revoked (in the URL),
  the existing offset pagination with its exact total, and loading, empty, error and success states. Switching
  organization never shows another organization's rows.
- Create (name up to 100 characters, URL; the backend validates both and applies the SSRF policy), rename, change URL,
  pause, activate, rotate the signing secret and revoke, each behind its own dialog and the permission of ADR 27
  (OWNER and ADMIN everything; DEVELOPER create, update, pause and activate; VIEWER read only). A 403 hides the
  controls. The change-URL field starts empty because the stored query string is never shown; revoke needs an
  acknowledgment and nothing is sent before a dialog is confirmed.
- A signing secret (create and rotate) is shown once in a dialog that Escape cannot close: it needs an acknowledgment
  and has a copy button. It exists only in component state: never in storage, the URL, a toast or an accessible name,
  and it is discarded when the dialog closes or the organization changes.
- A mutation is sent once. A lost connection, a 30 second timeout, a 5xx or an unreadable 2xx (`unreadable_success`)
  is an unknown result: the page says so, reads the destination (or the list) once and never repeats the request. A lost
  create or rotate answer means a lost secret, and the page says to rotate.
- `web/src/api/destinations.ts` keeps F1's `listDestinations` unchanged and adds `listDestinationRecords`,
  `getDestination`, `createDestination`, `updateDestination`, `pauseDestination`, `activateDestination`,
  `rotateDestinationSecret` and `revokeDestination` with strict response validation.
- Frontend only: no backend change, no migration, no new dependency.

### Added (Phase F2: delivery redrive in the web UI)

- `web/`: the delivery detail page offers **Redrive** for a `FAILED` delivery to users with `deliveries:redrive` (OWNER,
  ADMIN, DEVELOPER). A VIEWER gets no control and no dialog. A delivery that is not `FAILED` gets none, and a delivery whose
  budget is already 50 shows an explanation instead. The destination status is not requested: `409 destination_revoked`
  is the authoritative rejection, and a paused destination is only described in the dialog, never detected.
- A native `<dialog>` (`showModal()`) asks for a required single-line reason and explains at-least-once delivery, the
  advisory budget change (8 to 13, capped at 50), asynchronous processing and the audit log. The attempt count it sends
  is the one shown when the dialog opened. The reason and budget checks are advisory: Confirm is never disabled by them
  and the server's answer (including `validation_failed`) is shown inline. The server's `redrive` budget figures
  replace the estimate after success.
- One confirmation sends one POST with a 30 second client timeout. The POST is never repeated by the UI (the client's
  existing single retry after `csrf_invalid` is unchanged). A lost connection, a timeout, a generic or proxy 5xx, or an
  HTTP 200 with an unreadable or inconsistent body (`unreadable_success`) is an ambiguous outcome: the UI says the
  redrive may or may not have been applied, reads the delivery once, compares it with the snapshot taken when the dialog
  opened, and never claims that this request caused a change. If that read fails it shows "State unverified" with a "Check
  again" action (a GET). Redrive stays hidden until a check succeeds. Leaving the page or switching organization stops
  the work and starts no check.
- Known rejections (`delivery_not_failed`, `delivery_changed`, `destination_revoked`, `retry_budget_exhausted`,
  `event_payload_unavailable`, `lock_timeout`, 403, 404) have their own messages. After success the page shows a banner and
  reads the delivery once.
- `web/src/ui/Dialog.tsx` is the first dialog primitive. jsdom has no `showModal`, so `web/src/test/setup.ts` adds a
  minimal stand-in; focus trapping and inertness are verified in a real browser, not in the unit tests.
- Frontend only: no backend change, no migration, no new dependency, no worker, retry, lease or recovery change.

### Added (Phase F1b: delivery activity and feeds)

- `web/`: the Activity page at `/o/:orgId/operations/activity` (sidebar entry "Activity" in the Operations group).
  It reads `GET /operations/deliveries` (aggregate delivery counts), `GET /operations/failures` and
  `GET /operations/attempts` (cursor-paged feeds) once on open and again on Refresh (Phase F1d later added scheduled refresh).
- Each section has its own UTC date range and filters with an explicit Apply. Failures: reason and destination.
  Attempts: outcome and delivery. Applied filters and explicit ranges live in the URL; cursors never do.
- With no range applied the server picks its own default window: the browser sends no range, shows the window the
  server echoes, and reuses it for the continuation pages of that query. Refresh asks for a fresh window.
- Previous and Next use a stack of the cursors the server issued (never decoded or built in the browser). Changing
  a filter or pressing Refresh starts again from the first page.
- The failure feed has a destination selector that loads destinations on request, 100 at a time. Filtering is done
  by the server (`destinationId`). The attempt feed has no destination filter because the backend has none.
  Merging pages by id avoids duplicates but does not make the enumeration a snapshot.
- Light response-shape validation of the new endpoints: a malformed body is an error state, not a crash. The
  destination list keeps only id, name and status and never the URL.
- "Show attempts" on a failure row filters the attempts to that delivery over the delivery's own lifetime: from the
  minute it was created to the next whole minute after its latest failure, never the date range of the failure feed.
  The attempt feed spans at most 31 days. A delivery older than that (for example one redriven weeks after its first
  failure) gets the last 31 days and a visible warning that older attempts are excluded. The attempt section also says
  it shows only the attempts that started inside its window. Delivery detail links remain a later phase.
- Values of a link that were not valid are ignored with a notice, and are dropped from the URL when the user applies
  any section or presses "Clear ignored filters".
- A failure rate above zero is never shown as "0.0%" and one below total is never shown as "100.0%". The failure-reason
  meanings are text in the page, not only a tooltip, and the pager live region exists before rows arrive.
- Known limitation: timestamps from the API carry microseconds and are parsed with `new Date`. That is verified in
  Chromium only. Safari and Firefox are not verified (the formatting helpers from F1a have the same dependency).
- No backend change, no migration, no new dependency. Delivery detail links and redrive are a later phase.

### Added (Phase F1d: scheduled refresh of the operations pages)

- `web/`: the Queue and Activity pages refresh on their own every 15 seconds while the browser tab is visible. The first
  read is on entry, a hidden tab schedules nothing and refreshes at once when it is visible again, and Refresh still
  works. The queue snapshot, the queue age and the Activity summary each have their own schedule; a resource never has
  two requests at once and missed ticks are dropped, not queued. Requests are cancelled on unmount and on an
  organization change.
- A failed scheduled refresh keeps the data shown and says so in a notice that does not block anything.
- The Activity feeds are not replaced by a schedule: they check for a newer entry and offer "Show newest" in a "New
  activity available" notice. The page, filters, date range, cursors and position are never reset automatically.
- Rolling windows stay server-authoritative: no date is sent unless the user applied a range.
- Frontend only: no backend change, no migration, no new dependency, no worker, retry, lease or recovery change.

### Added (Phase F1c: delivery detail)

- `web/`: the delivery detail page at `/o/:orgId/operations/deliveries/:deliveryId`, using the existing read-only delivery
  history endpoint. It shows delivery metadata and the bounded attempt history returned by the API, newest first, with
  failure episodes separated only by stored terminal markers. Delivery IDs in the failure and attempt feeds link to it.
- No redrive controls, no backend change, no migration and no new dependency.

### Added (Phase F1a: operations queue page)

- `web/`: the queue page at `/o/:orgId/operations/queue` (sidebar group "Operations", one entry "Queue"). It reads
  `GET /operations/queue` and `GET /operations/queue/age` once on open and again on Refresh (Phase F1d later added scheduled refresh).
- Delivery counts (pending, due, delayed, in flight, expired leases) and the oldest due age use the backend's own
  definitions. A count that reaches 100,000 is shown as "≥ 100,000", decided per count and never as an exact value.
  The overdue-age distribution is shown exactly as returned: seven fixed buckets, no percentiles.
- Shared pieces for the next phases: operations API module, UTC formatting helpers and a `useResource` fetch hook
  (abort on unmount or organization change, nothing of another organization is ever shown).
- No backend change and no new dependency.

### Added (Phase F0: frontend foundation)

- `web/`: application shell (sidebar that becomes a drawer on narrow screens, top bar with organization switcher,
  theme switch and sign-out), light and dark themes, toasts, and sign-in, registration and organization pages that
  use the real API. A same-origin API client with a Vite development proxy to `http://[::1]:8080`.
- CSRF handling per ADR 10: in-memory token, lazy fetch after a reload, one rotation and one retry on `csrf_invalid`,
  single-flight rotation, an identity re-check after rotation and cross-tab sign-in and sign-out signals.
- A client copy of the role-to-permission matrix with a test that fails when it differs from `internal/tenancy/role.go`.
- No backend change, no new dependency. Operations, deliveries, destinations, events and access management are later phases.

### Added (Phase 4.4.6: delivery redrive)

- `POST /api/v1/organizations/{organizationID}/deliveries/{deliveryID}/redrive` (ADR 34): returns one `FAILED` delivery to
  `PENDING`, due immediately, and raises its `max_attempts` by five, capped at 50. Body `{"reason", "expectedAttemptCount"}`
  (both required; the reason is recorded in the audit event only). `200` with the delivery and the previous and new budget.
  `attempt_count` is never reset and attempt history is untouched, so attempt numbers keep growing. Errors: `404`, `403`,
  `400`, and `409` `delivery_not_failed`, `delivery_changed`, `destination_revoked`, `retry_budget_exhausted`,
  `event_payload_unavailable`, plus `503 lock_timeout` (a transaction-local five-second `lock_timeout`).
- New permission `deliveries:redrive` for OWNER, ADMIN and DEVELOPER (not VIEWER). Audit action `delivery.redriven` written
  in the same transaction. Metric `relayforge_delivery_redriven_total`.
- A redrive never reconstructs or substitutes a payload: inside the transaction PostgreSQL checks that the event exists,
  is non-empty and still hashes to its recorded SHA-256, otherwise `409 event_payload_unavailable`.
- Migration 00022 replaces `relayforge_enforce_delivery_transition()`: a `FAILED` delivery may return to `PENDING` only
  with the transaction-local flag `relayforge.redrive`, exactly +5 budget (cap 50), unchanged `attempt_count` and the
  reason and completion time cleared. `SUCCEEDED` and `CANCELLED` stay immutable. The flag is an application guard, not a
  security boundary. `Down` restores the 00012 function exactly. Deploy: migration, then workers, then API.
- Behavior change in the delivery worker: the row's `max_attempts` is now authoritative for the attempt limit (claim
  finalization and lease recovery), so a raised budget grants real attempts. Ordinary deliveries still have the default
  of 8, so nothing changes for them. Retry delays are still the attempt-number backoff (long for attempts 9 to 13).
- Locking: a shared organization lock (new `tenancy.LockOrganizationShared`, used only by redrive), then the destination
  `FOR SHARE`, then the delivery row `FOR NO KEY UPDATE`; the same order as destination revoke. Redrives of different
  deliveries do not queue behind each other, and membership changes and revokes cannot interleave with one.

### Added (Phase 4.4.7b: attempt activity feed)

- `GET /api/v1/organizations/{organizationID}/operations/attempts` (ADR 33): the organization's recorded delivery attempts
  across deliveries, most recent start first, with keyset pagination (`limit` 1 to 100, default 50, and an opaque `cursor`
  that carries a position only and is refused by the failure feed). Filters: `outcome` (one of the seven attempt outcomes,
  `ABANDONED` included), `deliveryId`, `from`, `to` (default window 24 hours, at most 31 days, same normalization as the
  other operational reads). There is no `destinationId` filter yet. Items are stored fields only (no derived flag) and
  carry operational metadata only: no payload, response body, destination URL, secret, raw error or claim data. The feed
  is a live tail: a late-committing attempt can land behind a cursor.
- Same authorization as the other operational reads (`deliveries:read`, non-member 404 before validation), the same
  five-second deadline (`503 read_model_timeout`) and no new permission, write route, cache or bulkhead. No migration and
  no new index: the existing `delivery_attempts_history_idx` serves the window, order and cursor. Plans on 200,000
  synthetic attempts used it in index order with no sequential scan; a rare `outcome` filter was not measured at scale.

### Added (Phase 4.4.5b: operational failure visibility)

- `GET /api/v1/organizations/{organizationID}/operations/failures` (ADR 32): the organization's current failures, most
  recent completion first, with keyset pagination (`limit` 1 to 100, default 50, and an opaque `cursor` that carries a
  position only). Filters: `failureReason`, `destinationId`, `from`, `to` (default window seven days, at most 31 days,
  same normalization as the delivery summary). A legacy failure with no recorded reason is shown as `UNRECORDED`. A
  delivery that is no longer `FAILED` (a future redrive; implemented later, see ADR 34) is not listed. Operational metadata only: no payload, destination
  URL, secret, raw error or claim data. The feed is eventually consistent across pages (late commits can land behind a
  cursor); start again from the first page for the newest failures.
- `GET /api/v1/organizations/{organizationID}/operations/queue/age`: how long due deliveries have been overdue
  (`generatedAt - next_attempt_at`, not age since creation) in seven fixed buckets (`<1m`, `1-5m`, `5-15m`, `15-60m`,
  `1-6h`, `6-24h`, `>24h`). One statement and one clock instant; each bucket saturates on its own at 100,000 and
  `capped` says so. A paused destination's due deliveries are counted.
- `GET .../deliveries?failureReason=`: filter the delivery list by one of the five failure reasons or `UNRECORDED`. A
  reason implies `status=FAILED` and any other `status` is `400`. Without the parameter the list is unchanged (same
  SQL, same bytes, no deadline). The exact `total` is kept; a filtered request runs under the five-second deadline and is
  authorized before its pagination, timestamps, status and reason are validated (a non-member gets `404`, never `400`).
- Migration 00021 adds `deliveries_org_failed_idx` (partial on `status = 'FAILED'`, built concurrently, idempotent,
  reversible). It was added because query-plan tests at 300,000 rows showed the feed reading the whole window without it
  (a sequential scan over 31 days). Its write overhead was **not benchmarked**; the expectation of one extra entry per
  delivery that ends `FAILED` is an architectural estimate, not a measured result.
- Same authorization as the other operational reads (`deliveries:read`, non-member 404 before validation), the same
  five-second deadline (`503 read_model_timeout`) and no new permission, write route, cache or bulkhead.

### Added (Phase 4.4.5: operational read models)

- `GET /api/v1/organizations/{organizationID}/operations/queue` and `.../operations/deliveries?from=&to=` (ADR 31): organization-scoped, read-only
  operational views computed live from PostgreSQL, with the existing `deliveries:read` permission (no new role,
  permission or write path). The queue view reports pending, due, delayed, the exact age of the oldest due
  delivery, in-flight and expired leases. The delivery view reports a created cohort (by `created_at`, with current
  status) and a completed cohort (by `completed_at`: succeeded, failed, cancelled, failure rate and failure reasons)
  over a half-open window of at most 31 days (default: the last 24 hours).
- Every counter saturates at 100,000 and the response says so with `capped`: a value of 100,000 means "at least
  100,000", and `capped` means at least one counter reached it. The cap bounds the matching rows each count returns,
  not the rows PostgreSQL examines (a rare bucket can examine much of a tenant's window); the deadline bounds the time.
  A failure rate over a capped count is `null`. `UNRECORDED` is a response-only failure-reason bucket for deliveries
  that failed before reasons were recorded. A due delivery of a paused destination still counts as due.
- Migration 00020 adds `deliveries_org_pending_idx` and `deliveries_org_completed_idx` (partial, built
  concurrently, idempotent, reversible). The claim path keeps using `deliveries_due_idx`.
- The database work of an operational read has a five-second deadline (`503 read_model_timeout`); a client that
  disconnects is not reported as a timeout. Window bounds are normalized to UTC microseconds and must fall in years
  0001 to 9999 (UTC), otherwise `400 validation_failed`.
- `delivery.ClaimDueStatement` is the SQL text of `ClaimDue`, moved into a named constant so a test can plan the real
  statement (no behavior change).

### Fixed

- The development README documented a default page size of 20; the API's default `limit` is 50.

### Added (Phase 4.4.4: terminal failure reasons)

- A delivery that ends as `FAILED` now records why, in a closed taxonomy: `DESTINATION_REJECTED` (final 4xx),
  `REDIRECT_REFUSED` (3xx), `EGRESS_BLOCKED` (egress policy), `RETRIES_EXHAUSTED` (attempt budget spent) and
  `INTERNAL_ERROR` (platform fault, and the fallback). ADR 30.
- Migration 00019 adds `deliveries.failure_reason` and `delivery_attempts.terminal_reason`, with `CHECK`
  constraints: only a `FAILED` delivery has a reason, only a non-successful attempt has a terminal reason, and a
  delivery that fails from now on must have one (`NOT VALID`, so existing `FAILED` rows stay NULL; nothing is
  backfilled). Attempt history stays append-only and terminal deliveries stay immutable.
- `RetryPolicy.Decide` returns `Decision.FailureReason`; `Finish` writes both reasons in the same transaction as the
  status change, under the claim-token fence, and rejects a decision whose status and reason disagree.
- `GET /deliveries` and `GET /deliveries/{id}` return `failureReason`, and the detail response returns
  `terminalReason` on the attempt that ended the delivery. Both are omitted when absent. No route, permission or
  role changed.
- New counters `relayforge_delivery_failed_<reason>_total`, one per reason with fixed names (no labels);
  `relayforge_delivery_failed_total` is unchanged. The failure log events carry `failure_reason`.

### Changed (Phase 4.4.3: reconciliation marker)

- Reconciliation no longer scans a 48-hour window of events. Migration 00018 adds `outbox.fanned_out_at` and a
  partial index on unmarked rows; `FanOut` and `Reconcile` set the marker in the same transaction that creates
  the deliveries, and `Reconcile` works only from unmarked rows. A lost fan-out is now repaired however old it
  is, including after JetStream expired its message.
- `delivery.Reconcile` no longer takes a lookback; it takes a `pgx.Tx` and `ReconcileParams` and returns a
  `ReconcileResult`. `relay.ConsumerConfig` drops `Lookback` and gains `LiveWindow`, per-lane batch budgets and
  `ReconcileMaxRowFailures`.
- Reconciliation has two lanes with independent budgets: live (younger than 24 hours) and history (older,
  unbounded), so a large historical backlog cannot delay the repair of a recent loss.
- A row that fails with an integrity or data error (SQLSTATE class 22 or 23) no longer blocks the rest of the
  batch: healthy rows are reconciled, the failing row stays unmarked, is reported safely and is retried every
  sweep. Transient errors still fail the whole sweep.
- Historical outbox rows start unmarked and are verified in bounded batches (no bulk update in the migration).
- New metrics: `relayforge_reconciled_events_total`, `relayforge_reconcile_row_failures_total` and the gauges
  `relayforge_reconcile_oldest_live_unmarked_age_seconds` and
  `relayforge_reconcile_oldest_history_unmarked_age_seconds`.
- Tests for the event/outbox invariant, and a guard that keeps `INSERT INTO webhook_events` and
  `INSERT INTO outbox` inside `internal/ingest/store.go`. ADR 29.

### Added (Phase 4.4: operational reliability and observability, first stages)

- `X-Request-Id` on every response and one access log line per request (route pattern, status, duration),
  with the same id on failed-request logs. Database error details (which can repeat an e-mail address or
  idempotency key) no longer reach the log.
- Outbox, event and organization identifiers on relay log lines.
- Gauges `relayforge_delivery_oldest_due_age_seconds` and `relayforge_outbox_oldest_unpublished_age_seconds`,
  and `relayforge_db_pool_*` connection pool gauges.
- Readiness now covers the delivery worker loops, the outbox publisher, the fan-out consumer and the NATS
  connection, not only PostgreSQL.
- ADR 28.

### Changed (Phase 4.4)

- The delivery worker no longer sleeps a whole `PollInterval` after a full batch while all slots are busy;
  it waits for a slot instead. Throughput was previously capped near `Concurrency` deliveries per poll interval.
- A request interrupted by shutdown is recorded immediately as an `ABANDONED` attempt
  (`shutdown_interrupted`) and is due again in 5 s, instead of waiting for the lease (2 min) and the next
  recovery sweep. The attempt still counts toward `max_attempts`.
- `RELAYFORGE_DELIVERY_SHUTDOWN_GRACE` defaults to 10 s (was 20 s) and is reduced at start-up so that the
  grace plus the 5 s result write fits in `RELAYFORGE_SHUTDOWN_TIMEOUT`.
- `QueueCounts` names the status in each subquery so it uses the partial indexes and no longer scans the
  whole, ever-growing `deliveries` table every 30 s.
- One metrics registry and one `GET /metrics` route per process, so roles can be combined in one process.

### Added (Phase 4.3: destination and subscription management API)

- Destination management: create (with a signing secret shown once), list, read, rename, change URL, pause,
  activate, rotate the secret and revoke (`DELETE`, permanent). URLs pass the SSRF-safe validation and are
  returned without their query string.
- Subscription management: create, list, read and revoke (`DELETE`), on top of `delivery.Subscribe` and
  `delivery.RevokeSubscription`; ADR 0026 event-time semantics are unchanged.
- Read-only delivery history: filtered, paginated list and a detail view with the recorded attempts.
- New permissions in the RBAC matrix (destinations, subscriptions, deliveries) and audit events for every mutation.
- Migration 00017: a revoked destination is immutable at the database level, destination identity and
  `secret_version` guards, and an organization index for subscription listing.
- ADR 0027.

### Added (Phase 4.2: reliable webhook delivery engine)

- `internal/egress`: outbound HTTP client with DNS pinning, SSRF address blocking, no proxies, no redirects,
  bounded timeouts and responses, and sanitized errors (ADR 0023).
- `internal/delivery`: outbound `v1=` HMAC signing with per-destination `rfd_` secrets (ADR 0022); claim,
  execute, finalize worker with fencing tokens, leases, `ABANDONED` recovery, graceful shutdown and
  bounded concurrency; retry classification with `Retry-After` handling (ADR 0024).
- `internal/messaging` and `internal/relay`: NATS JetStream stream and durable consumer, transactional
  outbox publisher (publish acknowledgment before `published_at`), idempotent fan-out and PostgreSQL
  reconciliation (ADR 0025). `cmd/outbox-publisher` now runs them.
- Migration 00015: `subscriptions` (tenant-safe composite foreign keys, revoked, never deleted).
- Migration 00016: subscription lifecycle. One ACTIVE subscription per endpoint/destination pair (partial
  unique index), re-subscription after revocation, immutable revoked history, valid intervals.
- Fan-out and reconciliation now use the event-time rule "active at acceptance" (ADR 0026), so revoking a
  subscription no longer drops events accepted while it was active.
- `delivery.Subscribe` and `delivery.RevokeSubscription`.
- Fixed NATS tests failing on a shared server ("subjects overlap"): the stream subject is now configurable.
- Label-free delivery, outbox, fan-out and reconciliation metrics; gauges in the metrics registry.
- Dependencies: `nats.go` (client) and `nats-server` (embedded server for tests only) with their transitive modules.
- CI starts a disposable NATS JetStream container.
- Delivery is at-least-once. Exactly-once is not claimed.

### Added (Phase 3: webhook ingestion and transactional outbox)

- Ingestion endpoints (`ingestion_endpoints`) with random public identifiers and AES-256-GCM encrypted
  signing secrets (`RELAYFORGE_ENCRYPTION_KEY`, required by the API), shown once on creation or rotation.
- `POST /api/v1/ingest/{publicId}`: HMAC-SHA256 signatures, strict timestamp window, 1 MiB limit,
  PostgreSQL-enforced idempotency, and an atomic event + outbox commit before `202`.
- Management API for endpoints (create, list, get, rename, pause, activate, rotate secret, revoke) and
  organization-scoped event listing and retrieval, with new RBAC permissions.
- Shared PostgreSQL rate limiting for ingestion and brute-force protection for login.
- Prometheus-format metrics at `GET /metrics` (opt-in) and structured logging without secrets.
- Migrations 00007 to 00010 and ADRs 0015 to 0021.

### Changed

- Login failures are now counted and limited (`429 rate_limited` with `Retry-After`).
- `apperror.Error` supports `RetryAfter`; the HTTP server limits header size.

### Added (Phase 2: core domain)

- Users, sessions, organizations, memberships, invitations and audit events (migrations 00002 to 00006).
- First-party authentication: Argon2id password hashing, opaque revocable sessions with a 12 hour
  absolute lifetime, hardened cookies and synchronizer-token CSRF protection.
- `/api/v1` endpoints for authentication, organizations, members, invitations, ownership transfer
  and audit events, with strict tenant isolation and pagination.
- Centralized RBAC policy with the roles OWNER, ADMIN, DEVELOPER and VIEWER; exactly one owner per
  organization, protected by a unique index and transactional ownership transfer.
- `RELAYFORGE_SESSION_LIFETIME` and `RELAYFORGE_COOKIE_SECURE` settings.
- ADRs 0008 to 0014.

### Not included

Email verification, password reset, MFA, automated invitation email, login rate limiting,
leaving an organization, and organization deletion.

### Added (Phase 1: database foundation)

- PostgreSQL configuration (`RELAYFORGE_DATABASE_URL`, `RELAYFORGE_DB_*`) with validation.
- `internal/database`: pgx v5 connection pool with lifecycle management and credential-safe errors.
- PostgreSQL readiness check in `/readyz` for processes that require the database (currently `api`).
- Embedded SQL migrations (goose) with advisory-lock protection, and the `cmd/migrate` tool
  (`up`, `status`). Initial migration adds the `relayforge_set_updated_at()` trigger function.
- Real PostgreSQL integration tests and a PostgreSQL 17 service in CI.
- ADRs 0003 to 0007.

### Review fixes

- `Pool.SQLDB()` now returns `(*sql.DB, error)` and is mutex-protected against `Close`; it returns
  `ErrClosed` after closure.
- `dbtest` validates the test server URL and only drops databases it created.
- `runtime` and `cmd/migrate` gained injectable entry points and automated tests.

### Changed

- Go module path is now `github.com/Thiago-tods/relayforge` (see ADR 0003).
- `runtime.Run` takes an `Options` argument.

### Added

- Phase 0 project foundation: Go backend skeleton (`api`, `outbox-publisher`, `delivery-worker`)
  with configuration loading, structured logging, HTTP server, graceful shutdown and health endpoints.
- React + TypeScript + Vite frontend shell with routing, ESLint, Prettier and Vitest.
- Docker Compose local infrastructure for PostgreSQL, Redis and NATS JetStream.
- GitHub Actions CI and Dependabot configuration.
- Project documentation.
