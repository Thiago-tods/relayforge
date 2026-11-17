-- Purpose: make the subscription lifecycle correct and auditable.
--
-- Migration 00015 made (ingestion_endpoint_id, destination_id) unconditionally
-- unique, so a pair could never be subscribed again after a revocation. This
-- migration lets each pair have at most one ACTIVE subscription at a time while
-- keeping every revoked subscription as immutable historical evidence.
--
-- A subscription record is one lifecycle interval:
--     [created_at, revoked_at)      revoked_at IS NULL means "still active"
-- It applies to an event if created_at <= event.received_at and
-- (revoked_at IS NULL OR revoked_at > event.received_at). Fan-out and
-- reconciliation use exactly this rule, so revoking a subscription later never
-- removes an event that was accepted while it was active.
-- Re-subscribing creates a NEW record (a new interval); it never reactivates an old one.

-- +goose Up
ALTER TABLE subscriptions DROP CONSTRAINT subscriptions_endpoint_destination_key;

CREATE UNIQUE INDEX subscriptions_one_active_per_pair_idx
    ON subscriptions (ingestion_endpoint_id, destination_id) WHERE status = 'ACTIVE';

-- A lifecycle interval cannot end before it starts.
ALTER TABLE subscriptions
    ADD CONSTRAINT subscriptions_interval_valid CHECK (revoked_at IS NULL OR revoked_at >= created_at);

-- Event-time lookups: the subscriptions of an endpoint whose interval covers a timestamp.
CREATE INDEX subscriptions_endpoint_interval_idx
    ON subscriptions (ingestion_endpoint_id, created_at, revoked_at);

-- +goose StatementBegin
CREATE FUNCTION relayforge_enforce_subscription_lifecycle() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    -- A revoked subscription is historical evidence: nothing about it may change,
    -- not even a no-op update.
    IF OLD.status = 'REVOKED' THEN
        RAISE EXCEPTION 'revoked subscription % is immutable', OLD.id USING ERRCODE = 'check_violation';
    END IF;
    -- Identity and the start of the interval never change.
    IF NEW.id IS DISTINCT FROM OLD.id
       OR NEW.organization_id IS DISTINCT FROM OLD.organization_id
       OR NEW.ingestion_endpoint_id IS DISTINCT FROM OLD.ingestion_endpoint_id
       OR NEW.destination_id IS DISTINCT FROM OLD.destination_id
       OR NEW.created_by_user_id IS DISTINCT FROM OLD.created_by_user_id
       OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'subscription % identity and start time are immutable', OLD.id USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- Named so that it fires before subscriptions_set_updated_at (triggers run alphabetically).
CREATE TRIGGER subscriptions_lifecycle
    BEFORE UPDATE ON subscriptions
    FOR EACH ROW EXECUTE FUNCTION relayforge_enforce_subscription_lifecycle();

-- +goose Down
-- Restores the unconditional uniqueness of 00015. It cannot succeed once a pair has
-- been subscribed more than once, because history is never deleted; the migration
-- then fails with a clear message instead of discarding evidence.
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM subscriptions GROUP BY ingestion_endpoint_id, destination_id HAVING count(*) > 1) THEN
        RAISE EXCEPTION 'cannot roll back: an endpoint/destination pair has several subscription records (history is never deleted)';
    END IF;
END;
$$;
-- +goose StatementEnd

DROP TRIGGER subscriptions_lifecycle ON subscriptions;
DROP FUNCTION relayforge_enforce_subscription_lifecycle();
DROP INDEX subscriptions_endpoint_interval_idx;
ALTER TABLE subscriptions DROP CONSTRAINT subscriptions_interval_valid;
DROP INDEX subscriptions_one_active_per_pair_idx;
ALTER TABLE subscriptions
    ADD CONSTRAINT subscriptions_endpoint_destination_key UNIQUE (ingestion_endpoint_id, destination_id);
