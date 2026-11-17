# 17. HMAC signature protocol

Date: 2026-11-17

## Status

Accepted

## Decision

Producers sign every request with the endpoint's signing secret.

| Header | Value |
|---|---|
| `X-RelayForge-Timestamp` | Unix time in seconds, ASCII digits only |
| `X-RelayForge-Signature` | 64 lowercase hexadecimal characters |
| `Idempotency-Key` | 1 to 255 printable ASCII characters |
| `Content-Type` | `application/json` |

```
signed_message = timestamp + "." + raw_request_body
signature      = lowercase_hex( HMAC-SHA256( key = UTF-8 bytes of the secret string, signed_message ) )
```

- The signature covers the **exact bytes** received. The server never parses and re-serializes the
  body before verifying or storing it, so whitespace and key order matter.
- The secret string (`rfs_...`) is used as the key as-is, as UTF-8 bytes, not base64-decoded.
- Verification uses `hmac.Equal` (constant time). A test inspects the code so a plain comparison
  cannot be introduced unnoticed.
- The signature must be exactly 64 lowercase hex characters. Uppercase, a `sha256=` prefix, base64
  and any other format are rejected as malformed. **There is no fallback format.** Unsigned requests
  are rejected.
- Signatures, secrets and bodies are never logged.

Error mapping: missing or malformed timestamp `400 invalid_timestamp`; outside the window
`401 timestamp_out_of_window`; missing or malformed signature `400 invalid_signature`; wrong
signature `401 signature_mismatch`; bad idempotency key `400 invalid_idempotency_key`; wrong content
type `415`; invalid JSON `400 invalid_json`; body over 1 MiB `413 request_too_large`.

Processing order: validate the public ID, apply the client-address rate limit, load the endpoint and
check status, read the body (1 MiB cap), check content type, JSON syntax, timestamp, signature
format, decrypt the secret, verify the HMAC, apply the per-endpoint rate limit, validate the
idempotency key, hash the payload, then one transaction (event + outbox) and `202`.

Clients: see the README for working PowerShell and curl examples.
