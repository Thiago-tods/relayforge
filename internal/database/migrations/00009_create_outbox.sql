-- Purpose: transactional outbox. A row is inserted in the same transaction as the
-- webhook event it announces, so a future publisher can deliver every accepted
-- event at least once. No publisher exists yet.

-- +goose Up
CREATE TABLE outbox (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id uuid        NOT NULL,
    event_id        uuid        NOT NULL,
    message_type    text        NOT NULL,
    -- Small envelope (identifiers and the payload hash), not the raw event payload.
    payload         jsonb       NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    -- NULL means unpublished.
    published_at    timestamptz,
    attempts        integer     NOT NULL DEFAULT 0,
    available_at    timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT outbox_event_fk FOREIGN KEY (event_id, organization_id)
        REFERENCES webhook_events (id, organization_id),
    -- Exactly one outbox record per event.
    CONSTRAINT outbox_event_id_key UNIQUE (event_id),
    CONSTRAINT outbox_message_type_length CHECK (char_length(message_type) BETWEEN 1 AND 100),
    CONSTRAINT outbox_payload_object CHECK (jsonb_typeof(payload) = 'object'),
    CONSTRAINT outbox_attempts_non_negative CHECK (attempts >= 0)
);

-- A publisher polls only unpublished rows that are due, oldest first.
CREATE INDEX outbox_unpublished_idx ON outbox (available_at, id) WHERE published_at IS NULL;
CREATE INDEX outbox_organization_idx ON outbox (organization_id);

-- +goose Down
DROP TABLE outbox;
