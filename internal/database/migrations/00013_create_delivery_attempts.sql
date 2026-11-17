-- Purpose: append-only history of delivery attempts. A row is written when an
-- attempt ends, in the same transaction that moves the delivery to its next
-- state, so a recorded result and the delivery state never disagree. Rows are
-- never updated or deleted, and they outlive revoked destinations.

-- +goose Up
CREATE TABLE delivery_attempts (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id uuid        NOT NULL,
    delivery_id     uuid        NOT NULL,
    attempt_number  integer     NOT NULL,
    started_at      timestamptz NOT NULL,
    finished_at     timestamptz NOT NULL,
    -- Mirrors delivery.Outcome in Go.
    outcome         text        NOT NULL,
    -- The destination's response status. No response body is stored: destinations
    -- can echo secrets back.
    http_status     smallint,
    -- A short machine-readable class for failures; never free text from a destination.
    error_class     text,
    duration_ms     integer     NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT delivery_attempts_delivery_fk FOREIGN KEY (delivery_id, organization_id)
        REFERENCES deliveries (id, organization_id),
    CONSTRAINT delivery_attempts_delivery_number_key UNIQUE (delivery_id, attempt_number),
    CONSTRAINT delivery_attempts_number_positive CHECK (attempt_number >= 1),
    CONSTRAINT delivery_attempts_duration_non_negative CHECK (duration_ms >= 0),
    CONSTRAINT delivery_attempts_finished_after_start CHECK (finished_at >= started_at),
    CONSTRAINT delivery_attempts_error_class_length CHECK (error_class IS NULL OR char_length(error_class) BETWEEN 1 AND 64),
    CONSTRAINT delivery_attempts_outcome_valid CHECK (outcome IN (
        'SUCCESS', 'HTTP_ERROR', 'REDIRECT', 'NETWORK_ERROR', 'TIMEOUT', 'BLOCKED_BY_POLICY', 'ABANDONED'
    )),
    -- The status code must agree with the outcome (delivery.OutcomeFromHTTPStatus).
    -- A NULL status never satisfies a status-bearing outcome (COALESCE turns the
    -- unknown comparison result into false).
    CONSTRAINT delivery_attempts_outcome_status_consistent CHECK (
        COALESCE(
            (outcome = 'SUCCESS' AND http_status BETWEEN 200 AND 299)
            OR (outcome = 'REDIRECT' AND http_status BETWEEN 300 AND 399)
            OR (outcome = 'HTTP_ERROR' AND http_status BETWEEN 400 AND 599),
            false)
        OR (outcome IN ('NETWORK_ERROR', 'TIMEOUT', 'BLOCKED_BY_POLICY', 'ABANDONED') AND http_status IS NULL)
    )
);

-- History by organization, newest first. History by delivery is served by the
-- unique (delivery_id, attempt_number) index.
CREATE INDEX delivery_attempts_history_idx
    ON delivery_attempts (organization_id, started_at DESC, id DESC);

-- +goose StatementBegin
CREATE FUNCTION relayforge_check_delivery_attempt() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    claimed integer;
BEGIN
    SELECT attempt_count INTO claimed
      FROM deliveries
     WHERE id = NEW.delivery_id AND organization_id = NEW.organization_id;
    -- An attempt can only be recorded for an attempt that was actually claimed.
    IF claimed IS NULL OR NEW.attempt_number > claimed THEN
        RAISE EXCEPTION 'delivery attempt % was never claimed (delivery attempt_count is %)', NEW.attempt_number, claimed
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER delivery_attempts_check_claimed
    BEFORE INSERT ON delivery_attempts
    FOR EACH ROW EXECUTE FUNCTION relayforge_check_delivery_attempt();

-- +goose StatementBegin
CREATE FUNCTION relayforge_reject_attempt_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'delivery_attempts is append-only' USING ERRCODE = 'integrity_constraint_violation';
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER delivery_attempts_append_only
    BEFORE UPDATE OR DELETE ON delivery_attempts
    FOR EACH ROW EXECUTE FUNCTION relayforge_reject_attempt_change();

-- +goose Down
-- Drops the attempt history. Only acceptable before the table is used in production.
DROP TABLE delivery_attempts;
DROP FUNCTION relayforge_check_delivery_attempt();
DROP FUNCTION relayforge_reject_attempt_change();
