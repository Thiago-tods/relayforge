-- Purpose: one-time, expiring, revocable invitations. Only the SHA-256 hash of
-- the invitation token is stored.

-- +goose Up
CREATE TABLE organization_invitations (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id    uuid        NOT NULL REFERENCES organizations (id),
    invited_email      text        NOT NULL,
    invited_by_user_id uuid        NOT NULL REFERENCES users (id),
    token_hash         bytea       NOT NULL,
    role               text        NOT NULL,
    expires_at         timestamptz NOT NULL,
    accepted_at        timestamptz,
    revoked_at         timestamptz,
    created_at         timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT organization_invitations_token_hash_key UNIQUE (token_hash),
    CONSTRAINT organization_invitations_token_hash_length CHECK (octet_length(token_hash) = 32),
    -- Ownership is never granted by invitation; use ownership transfer.
    CONSTRAINT organization_invitations_role_valid CHECK (role IN ('ADMIN', 'DEVELOPER', 'VIEWER')),
    CONSTRAINT organization_invitations_email_lowercase CHECK (invited_email = lower(invited_email)),
    CONSTRAINT organization_invitations_expiry_after_creation CHECK (expires_at > created_at),
    CONSTRAINT organization_invitations_single_outcome CHECK (accepted_at IS NULL OR revoked_at IS NULL)
);

CREATE INDEX organization_invitations_organization_id_idx
    ON organization_invitations (organization_id);

-- At most one pending invitation per organization and email.
CREATE UNIQUE INDEX organization_invitations_one_pending_idx
    ON organization_invitations (organization_id, invited_email)
    WHERE accepted_at IS NULL AND revoked_at IS NULL;

-- +goose Down
DROP TABLE organization_invitations;
