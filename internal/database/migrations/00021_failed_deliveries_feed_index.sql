-- Purpose: one partial index for the recent-failure feed (ADR 32).
--
-- deliveries_org_failed_idx (organization_id, completed_at DESC, id DESC) WHERE status = 'FAILED'
--   The feed lists an organization's FAILED deliveries newest completion first with keyset
--   pagination on (completed_at, id). The Stage 1 index deliveries_org_completed_idx knows
--   nothing about the status, so it had to read every completed delivery of the window (successes
--   included) and discard the ones that did not fail: with failures at 0.1% of a 300,000-row
--   tenant a first page examined the whole 7-day window, and over 31 days PostgreSQL chose a
--   sequential scan of the table (ADR 32 records the plans). This index holds failures only, so a
--   page reads about one index entry per returned row, whatever the success volume.
--
-- A delivery enters this index once, when it ends as FAILED, and leaves it only if a future
-- redrive moves it back out of FAILED. Rows, constraints and triggers are unchanged. The claim path
-- keeps using deliveries_due_idx.
--
-- Write overhead: one more index entry for each delivery that ends as FAILED, which is the rare
-- outcome, and nothing for successes, retries, claims or cancellations. This is an architectural
-- estimate based on which statements touch the indexed rows. The write overhead of this index was
-- NOT benchmarked; it is not a measured result.
--
-- The index is built CONCURRENTLY, so ingestion and delivery keep writing while it builds, which
-- requires running outside a transaction. Every statement is idempotent: an interrupted or
-- cancelled build leaves an INVALID index behind, and DROP INDEX CONCURRENTLY IF EXISTS removes it
-- before the next run builds it again.

-- +goose NO TRANSACTION

-- +goose Up
DROP INDEX CONCURRENTLY IF EXISTS deliveries_org_failed_idx;
CREATE INDEX CONCURRENTLY deliveries_org_failed_idx
    ON deliveries (organization_id, completed_at DESC, id DESC) WHERE status = 'FAILED';

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS deliveries_org_failed_idx;
