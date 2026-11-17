-- Purpose: close integrity gaps found in the review of 00011 to 00013, without
-- editing those migrations.
--
--   1. Delivery attempts are recorded only for the current claim.
--   2. TRUNCATE is rejected on the append-only attempt history.
--   3. Destinations and deliveries are never deleted or truncated, referenced or not.
--   4. The destination URL constraint rejects malformed hosts and ports.
--
-- ATTEMPT RECORDING PROTOCOL (the application must follow this order)
--
--   a. Claim:  UPDATE deliveries SET status = 'IN_FLIGHT', attempt_count = attempt_count + 1, ...
--              and commit. The claim increments attempt_count by exactly one.
--   b. Run the HTTP request outside any transaction.
--   c. In ONE transaction, in this order:
--        INSERT INTO delivery_attempts (attempt_number = the delivery's attempt_count, ...);
--        UPDATE deliveries SET <next state> WHERE id = $1 AND status = 'IN_FLIGHT' AND claim_token = $2;
--      If the UPDATE affects no row, the claim is stale: ROLLBACK, which also removes the attempt.
--      A reaper that gives up on an expired lease follows the same two steps, recording
--      an ABANDONED attempt first.
--
-- LOCKING AND DEADLOCK FREEDOM
--
--   The attempt trigger locks the delivery row with FOR NO KEY UPDATE before it checks
--   anything. That is the same lock an UPDATE of the row takes, so:
--     * every writer takes the delivery row first and the attempt row second, never the
--       reverse, so no lock cycle between the two tables is possible;
--     * the later UPDATE in the same transaction needs no lock upgrade (a plain
--       FOR SHARE lock would let two transactions deadlock while upgrading);
--     * a concurrent claim, retry or completion makes the inserter wait, and it then sees
--       the committed state: a stale worker finds a different status or attempt_count and
--       is rejected. Only one of a racing worker and reaper can record a given attempt.
--   Recording after the delivery has left IN_FLIGHT in the same transaction is rejected, so
--   the order above is enforced rather than merely recommended.
--
-- PRIVILEGES
--
--   These guarantees are triggers, which fire for every role regardless of its table
--   privileges, including a role that holds UPDATE, DELETE and TRUNCATE. Turning them off needs
--   table ownership or superuser rights. The application's runtime role must therefore
--   not own these tables and must not be a superuser; migrations run as a separate owner role.
--   Rolling back a migration uses DROP TABLE, which is deliberately not blocked.

-- +goose Up

-- 1. Attempts: the current claim only, in order, without gaps.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION relayforge_check_delivery_attempt() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    d_status text;
    d_count  integer;
BEGIN
    SELECT status, attempt_count INTO d_status, d_count
      FROM deliveries
     WHERE id = NEW.delivery_id AND organization_id = NEW.organization_id
       FOR NO KEY UPDATE;

    IF NOT FOUND THEN
        RAISE EXCEPTION 'delivery attempt references no delivery of this organization'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF d_status <> 'IN_FLIGHT' THEN
        RAISE EXCEPTION 'delivery attempts can only be recorded while the delivery is IN_FLIGHT (status is %)', d_status
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.attempt_number <> d_count THEN
        RAISE EXCEPTION 'delivery attempt number % must equal the delivery attempt_count %', NEW.attempt_number, d_count
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.attempt_number > 1 AND NOT EXISTS (
        SELECT 1 FROM delivery_attempts
         WHERE delivery_id = NEW.delivery_id AND attempt_number = NEW.attempt_number - 1
    ) THEN
        RAISE EXCEPTION 'delivery attempt % would leave a gap: attempt % was never recorded', NEW.attempt_number, NEW.attempt_number - 1
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- 2 and 3. Rows are never removed.
-- +goose StatementBegin
CREATE FUNCTION relayforge_reject_row_removal() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION '% rows are never deleted: % is not allowed (revoke or cancel instead)', TG_TABLE_NAME, TG_OP
        USING ERRCODE = 'integrity_constraint_violation';
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER destinations_never_deleted
    BEFORE DELETE ON destinations
    FOR EACH ROW EXECUTE FUNCTION relayforge_reject_row_removal();
CREATE TRIGGER deliveries_never_deleted
    BEFORE DELETE ON deliveries
    FOR EACH ROW EXECUTE FUNCTION relayforge_reject_row_removal();
CREATE TRIGGER destinations_never_truncated
    BEFORE TRUNCATE ON destinations
    FOR EACH STATEMENT EXECUTE FUNCTION relayforge_reject_row_removal();
CREATE TRIGGER deliveries_never_truncated
    BEFORE TRUNCATE ON deliveries
    FOR EACH STATEMENT EXECUTE FUNCTION relayforge_reject_row_removal();
CREATE TRIGGER delivery_attempts_never_truncated
    BEFORE TRUNCATE ON delivery_attempts
    FOR EACH STATEMENT EXECUTE FUNCTION relayforge_reject_row_removal();

-- 4. Destination URL: stricter host and port rules. The Go validator
-- (internal/egress) stays authoritative and rejects more; this only refuses
-- shapes that can never be valid, and never rejects a URL that validator
-- produces. A test keeps it that way.
ALTER TABLE destinations DROP CONSTRAINT destinations_url_shape;

-- https, dot-separated host labels of 1 to 63 characters that neither start nor end with a
-- hyphen (so no empty labels, consecutive dots or trailing dot), at least two labels, an
-- optional port of 1 to 5 digits, an optional path and query, and no fragment.
ALTER TABLE destinations ADD CONSTRAINT destinations_url_shape CHECK (
    url ~ '^https://([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(:[0-9]{1,5})?([/?][^#]*)?$'
);
ALTER TABLE destinations ADD CONSTRAINT destinations_url_host_length CHECK (
    char_length(substring(url from '^https://([^/?#:]+)')) <= 253
);
-- A numeric or 0x-hex last label is an ambiguous spelling of an IPv4 address, and IP
-- literals are never destinations.
ALTER TABLE destinations ADD CONSTRAINT destinations_url_last_label CHECK (
    substring(substring(url from '^https://([^/?#:]+)') from '[^.]+$') !~ '^([0-9]+|0x[0-9a-f]*)$'
);
-- Port 1 to 65535 without a leading zero. CASE keeps the integer cast from running on a too-long value.
ALTER TABLE destinations ADD CONSTRAINT destinations_url_port_range CHECK (
    substring(url from '^https://[^/?#:]+:([0-9]+)') IS NULL
    OR CASE
         WHEN char_length(substring(url from '^https://[^/?#:]+:([0-9]+)')) <= 5
              AND substring(url from '^https://[^/?#:]+:([0-9]+)') !~ '^0'
         THEN substring(url from '^https://[^/?#:]+:([0-9]+)')::integer BETWEEN 1 AND 65535
         ELSE false
       END
);

-- +goose Down
-- Restores the behavior of 00011 and 00013. Nothing else is touched, and rows
-- stay valid because the restored rules are looser.
ALTER TABLE destinations DROP CONSTRAINT destinations_url_port_range;
ALTER TABLE destinations DROP CONSTRAINT destinations_url_last_label;
ALTER TABLE destinations DROP CONSTRAINT destinations_url_host_length;
ALTER TABLE destinations DROP CONSTRAINT destinations_url_shape;
ALTER TABLE destinations ADD CONSTRAINT destinations_url_shape CHECK (
    url ~ '^https://[a-z0-9]([a-z0-9.-]*[a-z0-9])?(:[0-9]{1,5})?([/?][^#]*)?$'
);

DROP TRIGGER delivery_attempts_never_truncated ON delivery_attempts;
DROP TRIGGER deliveries_never_truncated ON deliveries;
DROP TRIGGER destinations_never_truncated ON destinations;
DROP TRIGGER deliveries_never_deleted ON deliveries;
DROP TRIGGER destinations_never_deleted ON destinations;
DROP FUNCTION relayforge_reject_row_removal();

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION relayforge_check_delivery_attempt() RETURNS trigger
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
