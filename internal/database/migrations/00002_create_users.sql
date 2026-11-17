-- Purpose: global user identities. Organization access is modeled by memberships,
-- never by a role on the user.

-- +goose Up
CREATE TABLE users (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    email            text        NOT NULL,
    normalized_email text        NOT NULL,
    password_hash    text        NOT NULL,
    display_name     text        NOT NULL,
    status           text        NOT NULL DEFAULT 'ACTIVE',
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT users_normalized_email_key UNIQUE (normalized_email),
    -- The application lowercases before inserting; this makes uniqueness
    -- case-insensitive even if a writer forgets to.
    CONSTRAINT users_normalized_email_lowercase CHECK (normalized_email = lower(normalized_email)),
    CONSTRAINT users_normalized_email_length CHECK (char_length(normalized_email) BETWEEN 3 AND 254),
    CONSTRAINT users_display_name_length CHECK (char_length(display_name) BETWEEN 1 AND 100),
    CONSTRAINT users_status_valid CHECK (status IN ('ACTIVE', 'DISABLED'))
);

CREATE TRIGGER users_set_updated_at
    BEFORE UPDATE ON users
    FOR EACH ROW EXECUTE FUNCTION relayforge_set_updated_at();

-- +goose Down
-- Reversible only while no later migration references users; drops all user data.
DROP TABLE users;
