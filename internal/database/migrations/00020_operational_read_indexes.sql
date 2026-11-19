-- Purpose: two partial indexes for the organization-scoped operational read models (ADR 31).
--
-- deliveries_org_pending_idx (organization_id, next_attempt_at) WHERE status = 'PENDING'
--   deliveries_due_idx has no organization_id, so counting one organization's pending
--   deliveries would walk every tenant's pending entries and fetch each heap row to test its
--   organization. This index makes pending, due, delayed and the oldest due delivery of ONE
--   organization an index range scan. The claim path keeps using deliveries_due_idx.
--
-- deliveries_org_completed_idx (organization_id, completed_at) WHERE completed_at IS NOT NULL
--   There was no index on completed_at, which the terminal-outcome windows (succeeded, failed,
--   cancelled and the failure reasons) filter on.
--
-- Write overhead: the pending index is maintained on the same events as deliveries_due_idx
-- (insert as PENDING, retry back to PENDING, claim leaving PENDING), so the hot path pays about
-- one more index entry per transition. The completed index adds one entry per delivery, when it
-- reaches a terminal status. Neither index changes any row, constraint or trigger.
-- This write overhead was NOT benchmarked: the figures above are an architectural estimate, not a
-- measured result.
--
-- Both indexes are built CONCURRENTLY, so ingestion and delivery keep writing while they build,
-- which requires running outside a transaction. Every statement is idempotent: an interrupted
-- or cancelled build leaves an INVALID index behind, and DROP INDEX CONCURRENTLY IF EXISTS
-- removes it before the next run builds it again.

-- +goose NO TRANSACTION

-- +goose Up
DROP INDEX CONCURRENTLY IF EXISTS deliveries_org_pending_idx;
CREATE INDEX CONCURRENTLY deliveries_org_pending_idx
    ON deliveries (organization_id, next_attempt_at) WHERE status = 'PENDING';

DROP INDEX CONCURRENTLY IF EXISTS deliveries_org_completed_idx;
CREATE INDEX CONCURRENTLY deliveries_org_completed_idx
    ON deliveries (organization_id, completed_at) WHERE completed_at IS NOT NULL;

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS deliveries_org_completed_idx;
DROP INDEX CONCURRENTLY IF EXISTS deliveries_org_pending_idx;
