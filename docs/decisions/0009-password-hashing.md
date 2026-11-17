# 9. Password policy and hashing

Date: 2026-11-17

## Status

Accepted

## Decision

**Hashing:** Argon2id from `golang.org/x/crypto/argon2` (RFC 9106), encoded in the PHC string format
(`$argon2id$v=19$m=65536,t=3,p=2$<salt>$<hash>`).

| Parameter | Value |
|---|---|
| Memory | 64 MiB |
| Iterations | 3 |
| Parallelism | 2 |
| Salt | 16 random bytes per password (`crypto/rand`) |
| Output | 32 bytes |

This exceeds the OWASP minimum (19 MiB, t=2, p=1) and costs roughly 100 ms per hash on a current
laptop core, which is acceptable for a login and affordable on a zero-budget host. Verification
reads the parameters stored in each hash, so raising the defaults later does not invalidate existing
hashes. Comparison uses `crypto/subtle.ConstantTimeCompare`. Stored hashes are bounded (at most
1 GiB memory and 20 iterations) so a corrupted row cannot trigger an enormous allocation.

Because every hash allocates 64 MiB, at most four hashes run concurrently; further requests wait and
respect their context deadline. This bounds memory use under load without an external rate limiter.

**Policy:**

- 12 to 128 characters, counted as Unicode characters (not bytes).
- Must not consist only of whitespace; must be valid UTF-8; must not contain NUL.
- Never trimmed, normalized or truncated.
- Login accepts passwords up to 1024 bytes before hashing, to bound unauthenticated work.
- Passwords and hashes are never logged, audited, serialized (`User.PasswordHash` has `json:"-"`) or
  returned by the API.

Composition rules (digits, symbols) and breached-password checks are not implemented; length is the
primary control, in line with NIST SP 800-63B.

## Consequences

Tests use deliberately cheap parameters (`apptest.FastArgon2Params`). A test asserts that
`DefaultArgon2Params` never drops below the documented minimum.
