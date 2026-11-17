# 18. Timestamp and replay protection

Date: 2026-11-17

## Status

Accepted

## Decision

- The timestamp is part of the signed message, so it cannot be changed without invalidating the
  signature.
- It must be Unix seconds written as ASCII digits (at most 12). Signs, spaces, decimals, exponents,
  hex, non-ASCII digits and ISO dates are rejected as malformed. A missing header is rejected.
- It must lie within **plus or minus 5 minutes** of the server clock (boundary inclusive). Comparison
  uses whole seconds in `int64`; a test covers far-future values that would overflow `time.Duration`
  arithmetic and wrongly pass.
- Within the window, replays are handled by **idempotency** (ADR 0019): the same signed request sent
  twice returns the original event and creates nothing. Outside the window, a captured request is
  useless even though its signature is valid.
- Producers need clocks within the skew. The window is configured in code (`ingest.Config`) and is
  five minutes by default.

## Limitations

- The signed message is exactly `timestamp + "." + body` (ADR 0017). The `Idempotency-Key` header is
  **not signed**. An attacker who captures a valid request (which TLS is meant to prevent) can
  replay it unchanged for up to five minutes, which idempotency makes harmless, or replay it with a
  **different** idempotency key, which creates a second event with the same body. Always serve the
  API over TLS. Producers should put a unique event ID inside the body so consumers can deduplicate.
- Signing the idempotency key would close this gap but changes the documented message format; it is a
  candidate for a future protocol version.
