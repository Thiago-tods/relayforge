# 21. Rate limiting

Date: 2026-11-17

## Status

Accepted

## Context

The Phase 2 security notes required login brute-force protection before any public exposure, and the
public ingestion endpoint needs resource limits. In-memory limiters protect nothing once the API runs
as several instances, so they were rejected.

## Decision

Fixed-window counters in PostgreSQL (`rate_limit_counters`, `internal/ratelimit`). One atomic upsert
(`INSERT ... ON CONFLICT DO UPDATE SET count = count + 1 RETURNING count`) counts a request, so
concurrent requests on any instance are counted exactly (a test checks 40 concurrent hits against a
limit of 25). Buckets are `name:sha256(identifier)`; no address or email is stored in the clear.

| Limit | Default | Window | Notes |
|---|---|---|---|
| Ingestion per client address | 300 requests | 1 minute | Applied before authentication |
| Ingestion per endpoint | 600 requests | 1 minute | Applied **after** the signature is verified |
| Failed logins per account | 10 | 15 minutes | Keyed on the hashed normalized email, whether or not the account exists |
| Failed logins per client address | 30 | 15 minutes | |

- Two ingestion buckets on purpose: unsigned traffic can only exhaust its own address's budget and
  can never starve the legitimate producer's per-endpoint budget (tested).
- Over the limit: `429 rate_limited` with `Retry-After`. Login limits are checked **before** the
  password, so a correct password during lockout is also refused.
- Settings: `RELAYFORGE_INGEST_RATE_PER_ENDPOINT`, `RELAYFORGE_INGEST_RATE_PER_IP`,
  `RELAYFORGE_LOGIN_FAILURES_PER_ACCOUNT`, `RELAYFORGE_LOGIN_FAILURES_PER_IP`.
- One goroutine, owned by the runtime and stopped before the pool closes, deletes counters older
  than 24 hours every hour. No unbounded queues or per-request goroutines exist.
- Other protections on ingestion: 1 MiB body cap, 64 KiB header cap, 15 second request timeout, bounded
  identifier and header lengths, duplicated headers rejected.
- Failure mode: if the counter store errors, the request fails (500), not open.

## Trade-offs and limitations

- Fixed windows allow up to twice the limit across a boundary.
- Every limited request costs one small database write.
- **The client address is the TCP peer.** `X-Forwarded-For` is ignored because it is client
  controlled. Behind a reverse proxy every client appears as the proxy, so per-address limits become
  global; a trusted-proxy setting is future work. Do not put the API behind a shared proxy without it.
- Account lockout lets an attacker who knows an email block that account for the rest of the window.
  This is the usual trade-off for brute-force protection; a future CAPTCHA-free mitigation could
  weight the address bucket more heavily.
- There is no distributed DoS protection. Run the API behind a network-level filter for that.

## Consequences

The Phase 2 limitation "no login rate limiting" is resolved for credential guessing. Public
production readiness is still not claimed: see SECURITY.md.
