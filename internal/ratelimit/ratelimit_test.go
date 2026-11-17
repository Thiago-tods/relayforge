package ratelimit_test

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Thiago-tods/relayforge/internal/app/apptest"
	"github.com/Thiago-tods/relayforge/internal/ratelimit"
)

var bg = context.Background()

func TestBucketHashesIdentifier(t *testing.T) {
	b := ratelimit.Bucket("login-ip", "203.0.113.7")
	if strings.Contains(b, "203.0.113.7") || !strings.HasPrefix(b, "login-ip:") || len(b) != len("login-ip:")+64 {
		t.Fatalf("bucket %q must hold only a hash", b)
	}
	if b != ratelimit.Bucket("login-ip", "203.0.113.7") || b == ratelimit.Bucket("login-ip", "203.0.113.8") {
		t.Fatal("bucket must be deterministic and identifier-specific")
	}
}

func TestHitEnforcesLimitWithinWindowAndResets(t *testing.T) {
	env := apptest.New(t)
	q, _ := env.Pool.Querier()
	now := time.Date(2026, 1, 1, 12, 0, 30, 0, time.UTC)
	bucket := ratelimit.Bucket("t", "a")

	for i := 1; i <= 3; i++ {
		res, err := ratelimit.Hit(bg, q, bucket, time.Minute, 3, now)
		if err != nil || !res.Allowed || res.Count != i {
			t.Fatalf("hit %d: %+v %v", i, res, err)
		}
	}
	res, err := ratelimit.Hit(bg, q, bucket, time.Minute, 3, now)
	if err != nil || res.Allowed || res.Count != 4 {
		t.Fatalf("hit over limit: %+v %v", res, err)
	}
	if res.RetryAfter != 30*time.Second {
		t.Fatalf("RetryAfter = %v, want 30s to the end of the window", res.RetryAfter)
	}
	// Other buckets are independent.
	if other, _ := ratelimit.Hit(bg, q, ratelimit.Bucket("t", "b"), time.Minute, 3, now); !other.Allowed {
		t.Fatal("an unrelated bucket was limited")
	}
	// A new window starts from zero.
	next, err := ratelimit.Hit(bg, q, bucket, time.Minute, 3, now.Add(31*time.Second))
	if err != nil || !next.Allowed || next.Count != 1 {
		t.Fatalf("next window: %+v %v", next, err)
	}
}

func TestExceededAndRecordCountOnlyRecordedFailures(t *testing.T) {
	env := apptest.New(t)
	q, _ := env.Pool.Querier()
	now := time.Now().UTC()
	bucket := ratelimit.Bucket("fail", "acct")

	for i := 0; i < 2; i++ {
		if res, _ := ratelimit.Exceeded(bg, q, bucket, time.Minute, 2, now); !res.Allowed {
			t.Fatalf("blocked after %d failures, limit 2", i)
		}
		if err := ratelimit.Record(bg, q, bucket, time.Minute, now); err != nil {
			t.Fatal(err)
		}
	}
	if res, _ := ratelimit.Exceeded(bg, q, bucket, time.Minute, 2, now); res.Allowed || res.Count != 2 {
		t.Fatalf("expected blocked at 2 failures: %+v", res)
	}
	// Reading never increments.
	if res, _ := ratelimit.Exceeded(bg, q, bucket, time.Minute, 2, now); res.Count != 2 {
		t.Fatalf("Exceeded changed the counter: %+v", res)
	}
}

func TestConcurrentHitsAreCountedExactly(t *testing.T) {
	env := apptest.New(t)
	q, _ := env.Pool.Querier()
	now := time.Now().UTC()
	bucket := ratelimit.Bucket("race", "x")

	const n = 40
	var wg sync.WaitGroup
	var mu sync.Mutex
	allowed := 0
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := ratelimit.Hit(bg, q, bucket, time.Hour, 25, now)
			if err != nil {
				t.Error(err)
				return
			}
			if res.Allowed {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if allowed != 25 {
		t.Fatalf("%d requests allowed, want exactly the limit of 25", allowed)
	}
	if got := env.Count(t, "SELECT max(count) FROM rate_limit_counters WHERE bucket = $1", bucket); got != n {
		t.Fatalf("counter = %d, want %d", got, n)
	}
}

func TestPruneRemovesOnlyExpiredWindows(t *testing.T) {
	env := apptest.New(t)
	q, _ := env.Pool.Querier()
	now := time.Now().UTC()
	old := ratelimit.Bucket("p", "old")
	fresh := ratelimit.Bucket("p", "fresh")
	if err := ratelimit.Record(bg, q, old, time.Minute, now.Add(-3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := ratelimit.Record(bg, q, fresh, time.Minute, now); err != nil {
		t.Fatal(err)
	}
	removed, err := ratelimit.Prune(bg, q, now.Add(-2*time.Hour))
	if err != nil || removed != 1 {
		t.Fatalf("removed %d, %v; want 1", removed, err)
	}
	if env.Count(t, "SELECT count(*) FROM rate_limit_counters WHERE bucket = $1", fresh) != 1 {
		t.Fatal("a current counter was pruned")
	}
}

func TestPrunerStopsWithContext(t *testing.T) {
	env := apptest.New(t)
	ctx, cancel := context.WithCancel(bg)
	done := make(chan struct{})
	go func() {
		ratelimit.RunPruner(ctx, env.Pool, 10*time.Millisecond, time.Hour, func(err error) { t.Error(err) })
		close(done)
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("pruner did not stop after cancellation")
	}
	_ = slog.New(slog.NewTextHandler(io.Discard, nil))
}
