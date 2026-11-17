-- Purpose: tenants and the memberships that grant users access to them.

-- +goose Up
CREATE TABLE organizations (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name       text        NOT NULL,
    slug       text        NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT organizations_slug_key UNIQUE (slug),
    CONSTRAINT organizations_name_length CHECK (char_length(name) BETWEEN 1 AND 100),
    -- Lowercase letters, digits and single hyphens; no leading, trailing or repeated hyphens.
    CONSTRAINT organizations_slug_format CHECK (
        char_length(slug) BETWEEN 3 AND 48 AND slug ~ '^[a-z0-9]+(-[a-z0-9]+)*$'
    )
);

CREATE TRIGGER organizations_set_updated_at
    BEFORE UPDATE ON organizations
    FOR EACH ROW EXECUTE FUNCTION relayforge_set_updated_at();

CREATE TABLE memberships (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id uuid        NOT NULL REFERENCES organizations (id),
    user_id         uuid        NOT NULL REFERENCES users (id),
    role            text        NOT NULL,
    status          text        NOT NULL DEFAULT 'ACTIVE',
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT memberships_organization_user_key UNIQUE (organization_id, user_id),
    CONSTRAINT memberships_role_valid CHECK (role IN ('OWNER', 'ADMIN', 'DEVELOPER', 'VIEWER')),
    CONSTRAINT memberships_status_valid CHECK (status IN ('ACTIVE', 'REMOVED'))
);

CREATE INDEX memberships_user_id_idx ON memberships (user_id);

-- At most one active OWNER per organization. Code paths that change roles also
-- lock the organization row, which keeps the count at exactly one.
CREATE UNIQUE INDEX memberships_one_active_owner_idx
    ON memberships (organization_id)
    WHERE role = 'OWNER' AND status = 'ACTIVE';

CREATE TRIGGER memberships_set_updated_at
    BEFORE UPDATE ON memberships
    FOR EACH ROW EXECUTE FUNCTION relayforge_set_updated_at();

-- +goose Down
-- Drops all tenant and membership data.
DROP TABLE memberships;
DROP TABLE organizations;
