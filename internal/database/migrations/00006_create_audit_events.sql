-- Purpose: append-only record of security-sensitive operations. Application code
-- only inserts; the trigger below rejects UPDATE and DELETE.

-- +goose Up
CREATE TABLE audit_events (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id uuid REFERENCES organizations (id),
    actor_user_id   uuid REFERENCES users (id),
    action          text        NOT NULL,
    resource_type   text        NOT NULL,
    resource_id     uuid,
    metadata        jsonb       NOT NULL DEFAULT '{}'::jsonb,
    created_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT audit_events_action_length CHECK (char_length(action) BETWEEN 1 AND 100),
    CONSTRAINT audit_events_metadata_object CHECK (jsonb_typeof(metadata) = 'object')
);

CREATE INDEX audit_events_organization_created_idx
    ON audit_events (organization_id, created_at DESC, id DESC);

-- +goose StatementBegin
CREATE FUNCTION relayforge_reject_audit_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'audit_events is append-only' USING ERRCODE = 'integrity_constraint_violation';
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER audit_events_append_only
    BEFORE UPDATE OR DELETE ON audit_events
    FOR EACH ROW EXECUTE FUNCTION relayforge_reject_audit_change();

-- +goose Down
-- Drops the audit history. Only acceptable before the table is used in production.
DROP TABLE audit_events;
DROP FUNCTION relayforge_reject_audit_change();
