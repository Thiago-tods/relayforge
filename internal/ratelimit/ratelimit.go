// Package ratelimit implements fixed-window rate limiting with counters stored in
// PostgreSQL, so every API instance shares the same limits.
//
// A bucket is "<name>:<sha256(identifier)>": identifiers such as client
// addresses and account emails are hashed, so the table holds no personal data.
// Each window is a row (bucket, window_start); an atomic upsert increments it,
// so concurrent requests on any instance are counted exactly.
//
// Fixed windows allow up to twice the limit across a window boundary. That is
// an accepted trade-off for a simple, exact, durable counter.
package ratelimit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"time"

	"github.com/Thiago-tods/relayforge/internal/database"
	"github.com/Thiago-tods/relayforge/internal/platform/apperror"
)

// Limited builds the 429 error returned when a limit is exceeded. The client is
// told when to retry through the Retry-After header.
func Limited(retryAfter time.Duration) *apperror.Error {
	e := apperror.New(http.StatusTooManyRequests, "rate_limited", "too many requests")
	e.RetryAfter = retryAfter
	return e
}

// Result is the outcome of a rate-limit check.
type Result struct {
	// Allowed is false once the count in the current window exceeds the limit.
	Allowed bool
	// Count is the number of hits in the current window, including this one for Hit.
	Count int
	// RetryAfter is the time until the current window ends.
	RetryAfter time.Duration
}

// Bucket builds a bucket key from a name and an identifier. The identifier is hashed.
func Bucket(name, identifier string) string {
	sum := sha256.Sum256([]byte(identifier))
	return name + ":" + hex.EncodeToString(sum[:])
}

func windowStart(now time.Time, window time.Duration) time.Time {
	return now.UTC().Truncate(window)
}

func retryAfter(now time.Time, window time.Duration) time.Duration {
	d := windowStart(now, window).Add(window).Sub(now)
	if d < time.Second {
		d = time.Second
	}
	return d
}

// Hit records one request in the bucket and reports whether it is within limit.
// Requests above the limit are still counted, so the count reflects attempted load.
func Hit(ctx context.Context, db database.Querier, bucket string, window time.Duration, limit int, now time.Time) (Result, error) {
	var count int
	err := db.QueryRow(ctx,
		`INSERT INTO rate_limit_counters (bucket, window_start, count) VALUES ($1, $2, 1)
		 ON CONFLICT (bucket, window_start) DO UPDATE SET count = rate_limit_counters.count + 1
		 RETURNING count`, bucket, windowStart(now, window)).Scan(&count)
	if err != nil {
		return Result{}, fmt.Errorf("record rate limit hit: %w", err)
	}
	return Result{Allowed: count <= limit, Count: count, RetryAfter: retryAfter(now, window)}, nil
}

// Exceeded reports whether the bucket has already reached limit in the current
// window, without recording a hit. Use it with Record to count only failures.
func Exceeded(ctx context.Context, db database.Querier, bucket string, window time.Duration, limit int, now time.Time) (Result, error) {
	var count int
	err := db.QueryRow(ctx,
		"SELECT coalesce(max(count), 0) FROM rate_limit_counters WHERE bucket = $1 AND window_start = $2",
		bucket, windowStart(now, window)).Scan(&count)
	if err != nil {
		return Result{}, fmt.Errorf("read rate limit counter: %w", err)
	}
	return Result{Allowed: count < limit, Count: count, RetryAfter: retryAfter(now, window)}, nil
}

// Record adds one hit to the bucket without checking it.
func Record(ctx context.Context, db database.Querier, bucket string, window time.Duration, now time.Time) error {
	_, err := Hit(ctx, db, bucket, window, 0, now)
	return err
}

// Release removes one previously reserved attempt from the current bucket window.
// It is used when a login succeeds or fails for an internal reason. Call it inside
// the same transaction as the successful session creation when possible.
func Release(ctx context.Context, db database.Querier, bucket string, window time.Duration, now time.Time) error {
	start := windowStart(now, window)
	tag, err := db.Exec(ctx,
		"UPDATE rate_limit_counters SET count = count - 1 WHERE bucket = $1 AND window_start = $2 AND count > 1",
		bucket, start)
	if err != nil {
		return fmt.Errorf("release rate limit reservation: %w", err)
	}
	if tag.RowsAffected() == 0 {
		if _, err := db.Exec(ctx,
			"DELETE FROM rate_limit_counters WHERE bucket = $1 AND window_start = $2 AND count = 1",
			bucket, start); err != nil {
			return fmt.Errorf("delete released rate limit reservation: %w", err)
		}
	}
	return nil
}

// Prune deletes counters whose window started before cutoff and returns how many.
func Prune(ctx context.Context, db database.Querier, cutoff time.Time) (int64, error) {
	tag, err := db.Exec(ctx, "DELETE FROM rate_limit_counters WHERE window_start < $1", cutoff)
	if err != nil {
		return 0, fmt.Errorf("prune rate limit counters: %w", err)
	}
	return tag.RowsAffected(), nil
}

// RunPruner deletes expired counters every interval until ctx is cancelled. It
// is a single goroutine owned by the caller, who should wait for it on shutdown.
// Counters older than keep are removed; keep must exceed the longest window.
func RunPruner(ctx context.Context, pool *database.Pool, interval, keep time.Duration, onError func(error)) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			q, err := pool.Querier()
			if err != nil {
				return // pool closed: the process is shutting down
			}
			if _, err := Prune(ctx, q, time.Now().UTC().Add(-keep)); err != nil && ctx.Err() == nil && onError != nil {
				onError(err)
			}
		}
	}
}
