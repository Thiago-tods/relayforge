-- Purpose: a durable marker that says "the fan-out of this event has been committed", so
-- reconciliation can find lost fan-out work directly instead of re-examining every event
-- received inside a time window (ADR 29).
--
-- outbox.fanned_out_at
--   NULL      the deliveries for this event have not been confirmed.
--   non-NULL  the transaction that created the event's deliveries (delivery.FanOut, or
--             delivery.Reconcile) committed at that moment. The marker is written in the
--             SAME transaction as those deliveries, so it can never be set without them and
--             never be missing after they committed.
--
-- The marker records "fan-out ran", not "deliveries exist": an event with no eligible
-- subscription is marked too. Eligibility stays event-time (ADR 26), so re-running the
-- fan-out for a marked or historical row is always safe and creates nothing that was not due.
--
-- Existing rows are NOT updated here. After this migration every historical outbox row has a
-- NULL marker, and reconciliation verifies them oldest first in bounded batches (a batch size
-- per call and a batch count per sweep); each verification creates only what is missing and
-- marks the row. That is the bounded backfill: no bulk UPDATE, no long lock, and it also
-- repairs gaps older than the former 48 h lookback.
--
-- ADD COLUMN without a default is a catalog-only change. The partial index is built
-- CONCURRENTLY so ingestion keeps writing to outbox while it builds, which requires running
-- outside a transaction. Every statement is idempotent so an interrupted run can simply be
-- run again: DROP INDEX IF EXISTS removes an INVALID index left behind by a cancelled build.

-- +goose NO TRANSACTION

-- +goose Up
ALTER TABLE outbox ADD COLUMN IF NOT EXISTS fanned_out_at timestamptz;

DROP INDEX CONCURRENTLY IF EXISTS outbox_unfanned_idx;
CREATE INDEX CONCURRENTLY outbox_unfanned_idx ON outbox (created_at, id) WHERE fanned_out_at IS NULL;

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS outbox_unfanned_idx;
ALTER TABLE outbox DROP COLUMN IF EXISTS fanned_out_at;
