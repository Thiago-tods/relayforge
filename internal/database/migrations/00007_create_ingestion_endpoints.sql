-- Purpose: ingestion endpoints are RelayForge URLs that receive signed events from
-- an external producer. They are not delivery destinations. The signing secret is
-- stored only as AES-256-GCM ciphertext.

-- +goose Up
CREATE TABLE ingestion_endpoints (
    id                        uuid PRIMARY KEY,
    organization_id           uuid        NOT NULL REFERENCES organizations (id),
    -- Random public identifier used in the ingestion URL; never the internal id.
    public_id                 text        NOT NULL,
    name                      text        NOT NULL,
    status                    text        NOT NULL DEFAULT 'ACTIVE',
    signing_secret_ciphertext bytea       NOT NULL,
    encryption_key_version    smallint    NOT NULL,
    -- Incremented on every rotation. Ingestion re-checks it inside its transaction.
    secret_version            bigint      NOT NULL DEFAULT 1,
    created_by_user_id        uuid        NOT NULL REFERENCES users (id),
    created_at                timestamptz NOT NULL DEFAULT now(),
    updated_at                timestamptz NOT NULL DEFAULT now(),
    revoked_at                timestamptz,
    CONSTRAINT ingestion_endpoints_public_id_key UNIQUE (public_id),
    -- Lets child tables reference (id, organization_id) so tenant ownership is a relational fact.
    CONSTRAINT ingestion_endpoints_id_organization_key UNIQUE (id, organization_id),
    CONSTRAINT ingestion_endpoints_public_id_format CHECK (public_id ~ '^ep_[A-Za-z0-9_-]{22}$'),
    CONSTRAINT ingestion_endpoints_name_length CHECK (char_length(name) BETWEEN 1 AND 100),
    CONSTRAINT ingestion_endpoints_status_valid CHECK (status IN ('ACTIVE', 'PAUSED', 'REVOKED')),
    CONSTRAINT ingestion_endpoints_revoked_consistent CHECK ((status = 'REVOKED') = (revoked_at IS NOT NULL)),
    -- 12-byte nonce + at least the 16-byte GCM tag.
    CONSTRAINT ingestion_endpoints_ciphertext_length CHECK (octet_length(signing_secret_ciphertext) >= 28),
    CONSTRAINT ingestion_endpoints_key_version_positive CHECK (encryption_key_version > 0),
    CONSTRAINT ingestion_endpoints_secret_version_positive CHECK (secret_version > 0)
);

CREATE INDEX ingestion_endpoints_organization_created_idx
    ON ingestion_endpoints (organization_id, created_at DESC, id DESC);

CREATE TRIGGER ingestion_endpoints_set_updated_at
    BEFORE UPDATE ON ingestion_endpoints
    FOR EACH ROW EXECUTE FUNCTION relayforge_set_updated_at();

-- +goose Down
-- Drops all endpoints and, through the dependent migrations, must run after them.
DROP TABLE ingestion_endpoints;
