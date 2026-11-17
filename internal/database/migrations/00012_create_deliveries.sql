-- Purpose: durable delivery records. One row is one logical delivery of one
-- event to one destination; its id is the stable delivery identifier that
-- receivers use to deduplicate (delivery is at-least-once).
--
-- The status machine is enforced here as well as in Go (internal/delivery):
--   PENDING   -> IN_FLIGHT (claim), CANCELLED
--   IN_FLIGHT -> SUCCEEDED, PENDING (retry or expired lease), FAILED
--   SUCCEEDED, FAILED and CANCELLED are terminal and immutable.
-- A row may also be updated without changing status while PENDING (reschedule)
-- or IN_FLIGHT (renew the lease).

-- +goose Up
CREATE TABLE deliveries (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id  uuid        NOT NULL,
    event_id         uuid        NOT NULL,
    destination_id   uuid        NOT NULL,
    status           text        NOT NULL DEFAULT 'PENDING',
    -- Number of attempts started. It is incremented exactly once per claim.
    attempt_count    integer     NOT NULL DEFAULT 0,
    max_attempts     integer     NOT NULL DEFAULT 8,
    next_attempt_at  timestamptz NOT NULL DEFAULT now(),
    -- Lease held by the worker that claimed the delivery (IN_FLIGHT only).
    lease_expires_at timestamptz,
    -- Fencing token: results are recorded only by the holder of the current token.
    claim_token      uuid,
    claimed_by       text,
    last_http_status smallint,
    last_error_class text,
    completed_at     timestamptz,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),

    -- Tenant isolation: the event and the destination must belong to the same
    -- organization as the delivery. Neither cascades: history is never deleted.
    CONSTRAINT deliveries_event_fk FOREIGN KEY (event_id, organization_id)
        REFERENCES webhook_events (id, organization_id),
    CONSTRAINT deliveries_destination_fk FOREIGN KEY (destination_id, organization_id)
        REFERENCES destinations (id, organization_id),
    -- Exactly one logical delivery per event and destination, so duplicate
    -- messages or concurrent fan-out cannot create a second one.
    CONSTRAINT deliveries_event_destination_key UNIQUE (event_id, destination_id),
    CONSTRAINT deliveries_id_organization_key UNIQUE (id, organization_id),

    CONSTRAINT deliveries_status_valid CHECK (status IN ('PENDING', 'IN_FLIGHT', 'SUCCEEDED', 'FAILED', 'CANCELLED')),
    CONSTRAINT deliveries_attempt_count_non_negative CHECK (attempt_count >= 0),
    CONSTRAINT deliveries_max_attempts_range CHECK (max_attempts BETWEEN 1 AND 50),
    CONSTRAINT deliveries_attempts_within_max CHECK (attempt_count <= max_attempts),
    CONSTRAINT deliveries_http_status_range CHECK (last_http_status IS NULL OR last_http_status BETWEEN 100 AND 599),
    CONSTRAINT deliveries_error_class_length CHECK (last_error_class IS NULL OR char_length(last_error_class) BETWEEN 1 AND 64),
    CONSTRAINT deliveries_claimed_by_length CHECK (claimed_by IS NULL OR char_length(claimed_by) BETWEEN 1 AND 200),

    -- Claim fields exist exactly while a delivery is IN_FLIGHT.
    CONSTRAINT deliveries_claim_fields_consistent CHECK (
        (status = 'IN_FLIGHT') = (lease_expires_at IS NOT NULL AND claim_token IS NOT NULL AND claimed_by IS NOT NULL)
        AND (status = 'IN_FLIGHT' OR (lease_expires_at IS NULL AND claim_token IS NULL AND claimed_by IS NULL))
    ),
    -- completed_at exists exactly for terminal statuses.
    CONSTRAINT deliveries_completed_consistent CHECK (
        (status IN ('SUCCEEDED', 'FAILED', 'CANCELLED')) = (completed_at IS NOT NULL)
    ),
    -- Claiming counts an attempt, and results only follow a claimed attempt. A
    -- cancelled delivery may never have been tried. A PENDING delivery always has attempts left.
    CONSTRAINT deliveries_attempts_match_status CHECK (
        (status IN ('IN_FLIGHT', 'SUCCEEDED', 'FAILED') AND attempt_count >= 1)
        OR (status = 'PENDING' AND attempt_count < max_attempts)
        OR status = 'CANCELLED'
    ),
    -- Success means a 2xx response was recorded.
    CONSTRAINT deliveries_succeeded_has_2xx CHECK (status <> 'SUCCEEDED' OR COALESCE(last_http_status BETWEEN 200 AND 299, false))
);

-- Workers poll for due deliveries (new work and scheduled retries are both PENDING
-- with a next_attempt_at) and for expired leases, oldest first.
CREATE INDEX deliveries_due_idx ON deliveries (next_attempt_at, id) WHERE status = 'PENDING';
CREATE INDEX deliveries_lease_idx ON deliveries (lease_expires_at, id) WHERE status = 'IN_FLIGHT';
CREATE INDEX deliveries_destination_idx ON deliveries (destination_id, created_at DESC, id DESC);
CREATE INDEX deliveries_organization_idx ON deliveries (organization_id, created_at DESC, id DESC);

CREATE TRIGGER deliveries_set_updated_at
    BEFORE UPDATE ON deliveries
    FOR EACH ROW EXECUTE FUNCTION relayforge_set_updated_at();

-- +goose StatementBegin
CREATE FUNCTION relayforge_enforce_delivery_transition() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.id <> OLD.id OR NEW.organization_id <> OLD.organization_id OR NEW.event_id <> OLD.event_id
       OR NEW.destination_id <> OLD.destination_id OR NEW.max_attempts <> OLD.max_attempts
       OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'delivery identity columns are immutable' USING ERRCODE = 'integrity_constraint_violation';
    END IF;

    IF OLD.status IN ('SUCCEEDED', 'FAILED', 'CANCELLED') THEN
        RAISE EXCEPTION 'delivery in terminal status % is immutable', OLD.status USING ERRCODE = 'integrity_constraint_violation';
    END IF;

    IF NEW.status <> OLD.status AND NOT (
        (OLD.status = 'PENDING' AND NEW.status IN ('IN_FLIGHT', 'CANCELLED'))
        OR (OLD.status = 'IN_FLIGHT' AND NEW.status IN ('SUCCEEDED', 'PENDING', 'FAILED'))
    ) THEN
        RAISE EXCEPTION 'illegal delivery status transition: % -> %', OLD.status, NEW.status
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;

    -- The attempt counter moves only when a delivery is claimed, by exactly one.
    IF OLD.status = 'PENDING' AND NEW.status = 'IN_FLIGHT' THEN
        IF NEW.attempt_count <> OLD.attempt_count + 1 THEN
            RAISE EXCEPTION 'a delivery claim must increment the delivery attempt_count by exactly one'
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
    ELSIF NEW.attempt_count <> OLD.attempt_count THEN
        RAISE EXCEPTION 'delivery attempt_count changes only when the delivery is claimed'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;

    -- A lease can be renewed but not taken over by another claim token.
    IF OLD.status = 'IN_FLIGHT' AND NEW.status = 'IN_FLIGHT' AND NEW.claim_token IS DISTINCT FROM OLD.claim_token THEN
        RAISE EXCEPTION 'delivery claim token cannot change while the delivery is in flight'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;

    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER deliveries_enforce_transition
    BEFORE UPDATE ON deliveries
    FOR EACH ROW EXECUTE FUNCTION relayforge_enforce_delivery_transition();

-- +goose Down
-- Must run after 00013 has been rolled back. Drops every delivery record.
DROP TABLE deliveries;
DROP FUNCTION relayforge_enforce_delivery_transition();
