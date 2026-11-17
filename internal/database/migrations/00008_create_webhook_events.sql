-- Purpose: accepted webhook events. The payload is stored exactly as received.
-- Idempotency is enforced by the unique key per ingestion endpoint.

-- +goose Up
CREATE TABLE webhook_events (
    id                    uuid PRIMARY KEY,
    organization_id       uuid        NOT NULL,
    ingestion_endpoint_id uuid        NOT NULL,
    idempotency_key       text        NOT NULL,
    payload               bytea       NOT NULL,
    payload_sha256        bytea       NOT NULL,
    content_type          text        NOT NULL,
    received_at           timestamptz NOT NULL DEFAULT now(),
    status                text        NOT NULL DEFAULT 'RECEIVED',
    -- The endpoint must belong to the same organization as the event.
    CONSTRAINT webhook_events_endpoint_fk FOREIGN KEY (ingestion_endpoint_id, organization_id)
        REFERENCES ingestion_endpoints (id, organization_id),
    -- The idempotency guarantee: one event per endpoint and key.
    CONSTRAINT webhook_events_idempotency_key UNIQUE (ingestion_endpoint_id, idempotency_key),
    CONSTRAINT webhook_events_id_organization_key UNIQUE (id, organization_id),
    CONSTRAINT webhook_events_idempotency_key_format CHECK (
        char_length(idempotency_key) BETWEEN 1 AND 255 AND idempotency_key ~ '^[\x21-\x7e]+$'
    ),
    CONSTRAINT webhook_events_payload_size CHECK (octet_length(payload) BETWEEN 1 AND 1048576),
    CONSTRAINT webhook_events_payload_sha256_length CHECK (octet_length(payload_sha256) = 32),
    CONSTRAINT webhook_events_content_type_length CHECK (char_length(content_type) BETWEEN 1 AND 255),
    CONSTRAINT webhook_events_status_valid CHECK (status IN ('RECEIVED'))
);

CREATE INDEX webhook_events_organization_received_idx
    ON webhook_events (organization_id, received_at DESC, id DESC);
CREATE INDEX webhook_events_endpoint_received_idx
    ON webhook_events (ingestion_endpoint_id, received_at DESC, id DESC);

-- +goose Down
DROP TABLE webhook_events;
