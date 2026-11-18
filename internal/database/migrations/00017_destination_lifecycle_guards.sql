-- Purpose: database-level guards for the destination management API, and an index
-- for listing an organization's subscriptions.
--
-- Migration 00011 stores destinations but nothing in the database stops a REVOKED
-- destination from being activated again, or its identity from being rewritten.
-- The application enforces both; this trigger makes the rule hold for every writer.
--   * A REVOKED destination is immutable evidence: no update of any kind.
--   * Identity and creation columns never change.
--   * The secret version only moves with the secret: a changed ciphertext or key
--     version requires secret_version + 1 exactly, and secret_version never changes alone.

-- +goose Up
-- +goose StatementBegin
CREATE FUNCTION relayforge_enforce_destination_lifecycle() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.status = 'REVOKED' THEN
        RAISE EXCEPTION 'revoked destination % is immutable', OLD.id USING ERRCODE = 'check_violation';
    END IF;
    IF NEW.id IS DISTINCT FROM OLD.id
       OR NEW.organization_id IS DISTINCT FROM OLD.organization_id
       OR NEW.created_by_user_id IS DISTINCT FROM OLD.created_by_user_id
       OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'destination % identity columns are immutable', OLD.id USING ERRCODE = 'check_violation';
    END IF;
    IF NEW.secret_version < OLD.secret_version THEN
        RAISE EXCEPTION 'destination % secret version cannot decrease', OLD.id USING ERRCODE = 'check_violation';
    END IF;
    -- The secret material and its version move together: a rotation replaces the
    -- ciphertext and/or key version and increments secret_version by exactly one;
    -- the version never moves without a real change of the stored secret.
    IF NEW.signing_secret_ciphertext IS DISTINCT FROM OLD.signing_secret_ciphertext
       OR NEW.encryption_key_version IS DISTINCT FROM OLD.encryption_key_version THEN
        IF NEW.secret_version <> OLD.secret_version + 1 THEN
            RAISE EXCEPTION 'destination % secret rotation must increment secret_version by exactly one', OLD.id
                USING ERRCODE = 'check_violation';
        END IF;
    ELSIF NEW.secret_version <> OLD.secret_version THEN
        RAISE EXCEPTION 'destination % secret_version cannot change without a new secret or key version', OLD.id
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- Fires before destinations_set_updated_at (triggers run alphabetically).
CREATE TRIGGER destinations_lifecycle
    BEFORE UPDATE ON destinations
    FOR EACH ROW EXECUTE FUNCTION relayforge_enforce_destination_lifecycle();

-- Organization-scoped subscription listing, newest first.
CREATE INDEX subscriptions_organization_idx
    ON subscriptions (organization_id, created_at DESC, id DESC);

-- +goose Down
DROP INDEX subscriptions_organization_idx;
DROP TRIGGER destinations_lifecycle ON destinations;
DROP FUNCTION relayforge_enforce_destination_lifecycle();
