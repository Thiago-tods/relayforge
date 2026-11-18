-- Purpose: record WHY a delivery ended as FAILED (ADR 30).
--
--   deliveries.failure_reason          the delivery-level reason; non-NULL only while status is FAILED.
--   delivery_attempts.terminal_reason  the same reason, on the one attempt that ended the delivery.
--
-- Both use one closed taxonomy (delivery.FailureReason in Go):
--   DESTINATION_REJECTED  the destination answered with a final 4xx
--   REDIRECT_REFUSED      the destination answered with a 3xx; redirects are never followed
--   EGRESS_BLOCKED        the egress guard refused the destination address or URL
--   RETRIES_EXHAUSTED     the attempt budget ran out on a retryable result
--   INTERNAL_ERROR        a platform fault, or a result that could not be classified
--
-- The attempt column exists because attempt history is append-only and cannot be amended later:
-- a future redrive can clear deliveries.failure_reason (the constraints allow it, because a
-- non-FAILED delivery must have a NULL reason) while the attempt that failed keeps its
-- terminal_reason for good.
--
-- LEGACY ROWS. Deliveries that are already FAILED keep failure_reason NULL: nothing is
-- backfilled. The old reason is only partly derivable, and terminal rows are immutable (trigger
-- relayforge_enforce_delivery_transition), so a backfill would have to bypass that guard.
-- "A FAILED delivery has a reason" is therefore added NOT VALID: it is enforced for every row
-- inserted or updated from now on and is deliberately never validated against legacy rows.
--
-- LOCKING. ADD COLUMN without a default is a catalog-only change. Each constraint is added NOT
-- VALID (a brief lock, no table scan) and the ones every existing row satisfies are then
-- VALIDATEd, which takes only SHARE UPDATE EXCLUSIVE and does not block reads or writes. Running
-- outside a transaction is what keeps those two steps separate. Every statement is idempotent
-- (IF EXISTS / IF NOT EXISTS), so an interrupted run can simply be run again.
--
-- No trigger changes: terminal deliveries stay immutable, and the existing transition trigger
-- does not look at these columns.

-- +goose NO TRANSACTION

-- +goose Up
ALTER TABLE deliveries ADD COLUMN IF NOT EXISTS failure_reason text;
ALTER TABLE delivery_attempts ADD COLUMN IF NOT EXISTS terminal_reason text;

ALTER TABLE deliveries DROP CONSTRAINT IF EXISTS deliveries_failure_reason_valid;
ALTER TABLE deliveries ADD CONSTRAINT deliveries_failure_reason_valid CHECK (
    failure_reason IS NULL OR failure_reason IN (
        'DESTINATION_REJECTED', 'REDIRECT_REFUSED', 'EGRESS_BLOCKED', 'RETRIES_EXHAUSTED', 'INTERNAL_ERROR'
    )) NOT VALID;
ALTER TABLE deliveries VALIDATE CONSTRAINT deliveries_failure_reason_valid;

ALTER TABLE deliveries DROP CONSTRAINT IF EXISTS deliveries_failure_reason_only_when_failed;
ALTER TABLE deliveries ADD CONSTRAINT deliveries_failure_reason_only_when_failed CHECK (
    failure_reason IS NULL OR status = 'FAILED') NOT VALID;
ALTER TABLE deliveries VALIDATE CONSTRAINT deliveries_failure_reason_only_when_failed;

-- Intentionally never validated: legacy FAILED rows have no reason.
ALTER TABLE deliveries DROP CONSTRAINT IF EXISTS deliveries_failed_has_reason;
ALTER TABLE deliveries ADD CONSTRAINT deliveries_failed_has_reason CHECK (
    status <> 'FAILED' OR failure_reason IS NOT NULL) NOT VALID;

ALTER TABLE delivery_attempts DROP CONSTRAINT IF EXISTS delivery_attempts_terminal_reason_valid;
ALTER TABLE delivery_attempts ADD CONSTRAINT delivery_attempts_terminal_reason_valid CHECK (
    terminal_reason IS NULL OR terminal_reason IN (
        'DESTINATION_REJECTED', 'REDIRECT_REFUSED', 'EGRESS_BLOCKED', 'RETRIES_EXHAUSTED', 'INTERNAL_ERROR'
    )) NOT VALID;
ALTER TABLE delivery_attempts VALIDATE CONSTRAINT delivery_attempts_terminal_reason_valid;

ALTER TABLE delivery_attempts DROP CONSTRAINT IF EXISTS delivery_attempts_terminal_reason_not_success;
ALTER TABLE delivery_attempts ADD CONSTRAINT delivery_attempts_terminal_reason_not_success CHECK (
    terminal_reason IS NULL OR outcome <> 'SUCCESS') NOT VALID;
ALTER TABLE delivery_attempts VALIDATE CONSTRAINT delivery_attempts_terminal_reason_not_success;

-- +goose Down
-- Drops only the new data. Delivery and attempt rows are otherwise unchanged.
ALTER TABLE delivery_attempts DROP CONSTRAINT IF EXISTS delivery_attempts_terminal_reason_not_success;
ALTER TABLE delivery_attempts DROP CONSTRAINT IF EXISTS delivery_attempts_terminal_reason_valid;
ALTER TABLE deliveries DROP CONSTRAINT IF EXISTS deliveries_failed_has_reason;
ALTER TABLE deliveries DROP CONSTRAINT IF EXISTS deliveries_failure_reason_only_when_failed;
ALTER TABLE deliveries DROP CONSTRAINT IF EXISTS deliveries_failure_reason_valid;
ALTER TABLE delivery_attempts DROP COLUMN IF EXISTS terminal_reason;
ALTER TABLE deliveries DROP COLUMN IF EXISTS failure_reason;
