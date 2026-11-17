-- Purpose: shared fixed-window counters for rate limiting. Counters live in
-- PostgreSQL so every API instance enforces the same limits. The bucket holds a
-- name plus a SHA-256 of the identifier (client address, account), never the
-- identifier itself.

-- +goose Up
CREATE TABLE rate_limit_counters (
    bucket       text        NOT NULL,
    window_start timestamptz NOT NULL,
    count        integer     NOT NULL DEFAULT 0,
    PRIMARY KEY (bucket, window_start),
    CONSTRAINT rate_limit_counters_bucket_length CHECK (char_length(bucket) BETWEEN 1 AND 200),
    CONSTRAINT rate_limit_counters_count_non_negative CHECK (count >= 0)
);

-- Supports pruning expired windows.
CREATE INDEX rate_limit_counters_window_idx ON rate_limit_counters (window_start);

-- +goose Down
DROP TABLE rate_limit_counters;
