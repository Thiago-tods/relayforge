-- Purpose: allow one controlled exit from a terminal FAILED delivery, for redrive (ADR 34).
--
-- A FAILED delivery may go back to PENDING in exactly one way. Every one of these must hold:
--   * the transaction set the local flag relayforge.redrive to 'on';
--   * the delivery was FAILED and its max_attempts was below 50;
--   * the new max_attempts is LEAST(old max_attempts + 5, 50);
--   * attempt_count is unchanged (it is never reset: attempt numbers keep growing);
--   * failure_reason and completed_at are cleared, and the claim fields stay empty.
-- Everything else keeps the rules of 00012: SUCCEEDED and CANCELLED stay immutable, FAILED stays
-- immutable except for this transition, the identity columns never change, max_attempts changes
-- only in a redrive, and attempt_count moves only when a delivery is claimed. The existing check
-- constraints still apply to the new row (a PENDING delivery needs attempt_count < max_attempts, the
-- claim fields exist only while IN_FLIGHT, completed_at only for terminal statuses, failure_reason
-- only while FAILED). The attempt history is not touched: delivery_attempts stays append-only, and
-- the 00014 trigger still requires attempt_number = attempt_count with no gap.
--
-- THE FLAG IS NOT A SECURITY BOUNDARY. relayforge.redrive is an ordinary custom setting that any
-- database session can set for its own transaction. It stops application code from changing a
-- terminal delivery by accident; it does not stop someone who can already run SQL as the
-- application's role, and it cannot stop a database owner or superuser, who can disable triggers.
-- Authorization, eligibility and audit are enforced by the service (internal/app/redrive.go), not by
-- this trigger. The other conditions above still hold even if the flag is set by hand.
--
-- Rollout: this is a permissive superset of the old function, so it is safe with binaries that
-- predate it. Down restores the 00012 function exactly. Rows already redriven stay valid under the
-- old function (it only restricts future updates).

-- +goose Up
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION relayforge_enforce_delivery_transition() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    is_redrive boolean;
BEGIN
    -- COALESCE: current_setting(..., true) is NULL when the flag was never set, which must read as "no".
    is_redrive := COALESCE(
        OLD.status = 'FAILED' AND NEW.status = 'PENDING'
        AND current_setting('relayforge.redrive', true) = 'on'
        AND OLD.max_attempts < 50
        AND NEW.max_attempts = LEAST(OLD.max_attempts + 5, 50)
        AND NEW.attempt_count = OLD.attempt_count
        AND NEW.failure_reason IS NULL AND NEW.completed_at IS NULL
        AND NEW.lease_expires_at IS NULL AND NEW.claim_token IS NULL AND NEW.claimed_by IS NULL,
        false);

    IF NEW.id <> OLD.id OR NEW.organization_id <> OLD.organization_id OR NEW.event_id <> OLD.event_id
       OR NEW.destination_id <> OLD.destination_id OR NEW.created_at <> OLD.created_at
       OR (NEW.max_attempts <> OLD.max_attempts AND NOT is_redrive) THEN
        RAISE EXCEPTION 'delivery identity columns are immutable' USING ERRCODE = 'integrity_constraint_violation';
    END IF;

    IF OLD.status IN ('SUCCEEDED', 'FAILED', 'CANCELLED') AND NOT is_redrive THEN
        RAISE EXCEPTION 'delivery in terminal status % is immutable', OLD.status USING ERRCODE = 'integrity_constraint_violation';
    END IF;

    IF NEW.status <> OLD.status AND NOT (
        (OLD.status = 'PENDING' AND NEW.status IN ('IN_FLIGHT', 'CANCELLED'))
        OR (OLD.status = 'IN_FLIGHT' AND NEW.status IN ('SUCCEEDED', 'PENDING', 'FAILED'))
        OR is_redrive
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

-- +goose Down
-- Restores the function of 00012 exactly.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION relayforge_enforce_delivery_transition() RETURNS trigger
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
