-- Purpose: connect an ingestion endpoint to the destinations that receive its
-- events. When an event is accepted, one delivery is created for each ACTIVE
-- subscription of its endpoint (fan-out). A subscription only applies to events
-- received after it was created; history is never backfilled.
--
-- Also adds the index that lets the reconciliation sweep find recent events
-- by time across organizations.

-- +goose Up
CREATE TABLE subscriptions (
    id                    uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id       uuid        NOT NULL,
    ingestion_endpoint_id uuid        NOT NULL,
    destination_id        uuid        NOT NULL,
    status                text        NOT NULL DEFAULT 'ACTIVE',
    created_by_user_id    uuid        NOT NULL REFERENCES users (id),
    created_at            timestamptz NOT NULL DEFAULT now(),
    updated_at            timestamptz NOT NULL DEFAULT now(),
    revoked_at            timestamptz,
    -- Tenant isolation: the endpoint and the destination must both belong to the
    -- subscription's organization, so a subscription can never link two tenants.
    CONSTRAINT subscriptions_endpoint_fk FOREIGN KEY (ingestion_endpoint_id, organization_id)
        REFERENCES ingestion_endpoints (id, organization_id),
    CONSTRAINT subscriptions_destination_fk FOREIGN KEY (destination_id, organization_id)
        REFERENCES destinations (id, organization_id),
    CONSTRAINT subscriptions_endpoint_destination_key UNIQUE (ingestion_endpoint_id, destination_id),
    CONSTRAINT subscriptions_status_valid CHECK (status IN ('ACTIVE', 'REVOKED')),
    CONSTRAINT subscriptions_revoked_consistent CHECK ((status = 'REVOKED') = (revoked_at IS NOT NULL))
);

CREATE INDEX subscriptions_active_by_endpoint_idx
    ON subscriptions (ingestion_endpoint_id) WHERE status = 'ACTIVE';
CREATE INDEX subscriptions_destination_idx ON subscriptions (destination_id);

CREATE TRIGGER subscriptions_set_updated_at
    BEFORE UPDATE ON subscriptions
    FOR EACH ROW EXECUTE FUNCTION relayforge_set_updated_at();

-- Like destinations and deliveries, subscriptions are revoked, never deleted.
CREATE TRIGGER subscriptions_never_deleted
    BEFORE DELETE ON subscriptions
    FOR EACH ROW EXECUTE FUNCTION relayforge_reject_row_removal();
CREATE TRIGGER subscriptions_never_truncated
    BEFORE TRUNCATE ON subscriptions
    FOR EACH STATEMENT EXECUTE FUNCTION relayforge_reject_row_removal();

-- Supports the reconciliation sweep, which looks at events received within a recent window.
CREATE INDEX webhook_events_received_at_idx ON webhook_events (received_at);

-- +goose Down
-- Drops every subscription. Existing deliveries are not affected.
DROP INDEX webhook_events_received_at_idx;
DROP TABLE subscriptions;
