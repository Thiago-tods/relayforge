# 16. Signing secret encryption at rest

Date: 2026-11-17

## Status

Accepted

## Context

HMAC verification needs the plaintext secret, so it cannot be stored as a one-way hash. It must be
reversible but protected if the database is read.

## Decision

- Secrets are `rfs_` plus 256 random bits (`crypto/rand`, unpadded base64url, 47 characters).
- Stored encrypted with **AES-256-GCM** (`internal/secretbox`). Each encryption draws a fresh random
  96-bit nonce stored in front of the ciphertext; the 128-bit tag authenticates it. There is no
  unauthenticated mode.
- **Additional authenticated data** is `relayforge:ingestion-endpoint:v1:<endpoint id>`, binding a
  ciphertext to its row. A ciphertext copied to another endpoint does not decrypt (tested).
- `encryption_key_version` is stored with the ciphertext. Version 1 is the only version; opening a
  ciphertext under another version fails.
- The key comes from `RELAYFORGE_ENCRYPTION_KEY`: base64 of exactly 32 bytes. The API refuses to
  start without a valid key (exit code 2), in every environment, including production. There is no
  default, no hardcoded key and no key generated at startup. Errors never echo the key. Tests inject
  a fixed key that protects nothing.
- The raw secret is returned exactly once, in the response that creates or rotates it. It is never
  logged, audited, listed or returned again. API representations omit the ciphertext and versions.
- Decryption failures at ingestion time are server errors (500, logged without secrets), not client
  errors.

## Backup and key loss

- **Back up the key separately from the database.** A database backup alone cannot yield secrets, and
  a key alone is useless without the data.
- **If the key is lost, every stored signing secret is unrecoverable.** Producers must receive new
  secrets by rotating each endpoint, which requires restoring service with a new key first (create a
  new key, then rotate every endpoint; existing ciphertexts will fail to decrypt until rotated).
- If the key leaks, treat all secrets as exposed: deploy a new key and rotate every endpoint.

## Planned key rotation (not implemented)

Introduce a keyring mapping version to key. New and rotated secrets use the newest version; existing
rows keep decrypting with their stored version. A background job re-encrypts old rows
(`decrypt with v(n)`, `encrypt with v(n+1)`, update `encryption_key_version`) and the old key is
retired when no row references it. The stored version column and the AAD are already in place for it.

## Consequences

Anyone who can read both the database and the key can read secrets, so they must be protected
separately (for example key in a secret manager, database in managed storage).
