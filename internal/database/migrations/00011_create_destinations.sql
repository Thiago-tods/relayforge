-- Purpose: delivery destinations. A destination is an external URL that
-- RelayForge sends events to. It belongs to one organization. Its signing
-- secret (used to sign outbound requests) is stored only as AES-256-GCM
-- ciphertext. Destinations are never deleted; they are revoked, which keeps the
-- delivery history that references them.

-- +goose Up
CREATE TABLE destinations (
    id                        uuid PRIMARY KEY,
    organization_id           uuid        NOT NULL REFERENCES organizations (id),
    name                      text        NOT NULL,
    -- The normalized URL produced by the application's validator. The database
    -- re-checks its shape; SSRF protection is enforced by the application at
    -- creation time and again at connection time.
    url                       text        NOT NULL,
    status                    text        NOT NULL DEFAULT 'ACTIVE',
    signing_secret_ciphertext bytea       NOT NULL,
    encryption_key_version    smallint    NOT NULL,
    -- Incremented on every rotation.
    secret_version            bigint      NOT NULL DEFAULT 1,
    created_by_user_id        uuid        NOT NULL REFERENCES users (id),
    created_at                timestamptz NOT NULL DEFAULT now(),
    updated_at                timestamptz NOT NULL DEFAULT now(),
    revoked_at                timestamptz,
    -- Lets deliveries reference (id, organization_id) so tenant ownership is a relational fact.
    CONSTRAINT destinations_id_organization_key UNIQUE (id, organization_id),
    CONSTRAINT destinations_name_length CHECK (char_length(name) BETWEEN 1 AND 100),
    CONSTRAINT destinations_status_valid CHECK (status IN ('ACTIVE', 'PAUSED', 'REVOKED')),
    CONSTRAINT destinations_revoked_consistent CHECK ((status = 'REVOKED') = (revoked_at IS NOT NULL)),
    CONSTRAINT destinations_url_length CHECK (char_length(url) BETWEEN 12 AND 2048),
    -- Printable ASCII only, and no backslash (chr(92)).
    CONSTRAINT destinations_url_charset CHECK (url !~ '[^\x21-\x7e]' AND position(chr(92) in url) = 0),
    -- https, a lowercase ASCII host (no user info, no brackets, no trailing dot),
    -- an optional port, an optional path and query, and no fragment.
    CONSTRAINT destinations_url_shape CHECK (
        url ~ '^https://[a-z0-9]([a-z0-9.-]*[a-z0-9])?(:[0-9]{1,5})?([/?][^#]*)?$'
    ),
    -- 12-byte nonce + at least the 16-byte GCM tag.
    CONSTRAINT destinations_ciphertext_length CHECK (octet_length(signing_secret_ciphertext) >= 28),
    CONSTRAINT destinations_key_version_positive CHECK (encryption_key_version > 0),
    CONSTRAINT destinations_secret_version_positive CHECK (secret_version > 0)
);

CREATE INDEX destinations_organization_idx
    ON destinations (organization_id, created_at DESC, id DESC);

-- A name identifies a destination within its organization until it is revoked.
CREATE UNIQUE INDEX destinations_active_name_key
    ON destinations (organization_id, lower(name))
    WHERE status <> 'REVOKED';

CREATE TRIGGER destinations_set_updated_at
    BEFORE UPDATE ON destinations
    FOR EACH ROW EXECUTE FUNCTION relayforge_set_updated_at();

-- +goose Down
-- Must run after 00012 and 00013 have been rolled back (goose does this in
-- order). Drops every destination and its encrypted secret.
DROP TABLE destinations;
