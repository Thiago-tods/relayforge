package delivery

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Thiago-tods/relayforge/internal/database"
)

// Queue-age distribution (ADR 32).

// AgeBucket is one fixed range of overdue time. MinSeconds is inclusive and MaxSeconds exclusive; the last
// bucket has no upper bound (MaxSeconds is null). Count saturates at CountCap like every operational
// counter, and each bucket saturates on its own.
type AgeBucket struct {
	Label      string `json:"label"`
	MinSeconds int64  `json:"minSeconds"`
	MaxSeconds *int64 `json:"maxSeconds"`
	Count      int    `json:"count"`
}

// QueueAge is the distribution of how long due deliveries have been overdue, at GeneratedAt.
//
// Overdue age is GeneratedAt - next_attempt_at, over the PENDING deliveries with next_attempt_at <=
// GeneratedAt. It is NOT the age since creation: a retried delivery's clock restarts when its retry
// becomes due. A due delivery of a PAUSED destination is counted (due does not mean claimable). Capped is
// true when at least one bucket reached CountCap.
type QueueAge struct {
	GeneratedAt time.Time   `json:"generatedAt"`
	Buckets     []AgeBucket `json:"buckets"`
	Capped      bool        `json:"capped"`
}

// ageBucketBounds are the fixed buckets, in seconds. They are constants of this file and the SQL is built
// from them, never from client input.
var ageBucketBounds = []struct {
	label    string
	min, max int64 // max < 0: unbounded
}{
	{"<1m", 0, 60},
	{"1-5m", 60, 300},
	{"5-15m", 300, 900},
	{"15-60m", 900, 3600},
	{"1-6h", 3600, 21600},
	{"6-24h", 21600, 86400},
	{">24h", 86400, -1},
}

// ageBucketWhere is the predicate of one bucket with now as $2: min <= now - next_attempt_at < max, written
// as a range of next_attempt_at so the partial index deliveries_org_pending_idx (organization_id,
// next_attempt_at) WHERE status = 'PENDING' serves it. The status is a literal so that index matches.
func ageBucketWhere(min, max int64) string {
	w := fmt.Sprintf("organization_id = $1 AND status = 'PENDING' AND next_attempt_at <= $2::timestamptz - interval '%d seconds'", min)
	if max >= 0 {
		w += fmt.Sprintf(" AND next_attempt_at > $2::timestamptz - interval '%d seconds'", max)
	}
	return w
}

// queueAgeSQL is one statement of seven independent capped counters, so every bucket comes from the same
// snapshot and is compared with the same instant.
func queueAgeSQL() string {
	cols := make([]string, len(ageBucketBounds))
	for i, b := range ageBucketBounds {
		cols[i] = capped(ageBucketWhere(b.min, b.max))
	}
	return "SELECT " + strings.Join(cols, ", ")
}

// QueueAgeOf reads the distribution in one statement. now is the single instant of every comparison and is
// reported as GeneratedAt; the caller normalizes it. It takes no row lock.
func QueueAgeOf(ctx context.Context, db database.Querier, orgID string, now time.Time) (QueueAge, error) {
	now = now.UTC()
	counts := make([]int, len(ageBucketBounds))
	dest := make([]any, len(counts))
	for i := range counts {
		dest[i] = &counts[i]
	}
	if err := db.QueryRow(ctx, queueAgeSQL(), orgID, now).Scan(dest...); err != nil {
		return QueueAge{}, fmt.Errorf("read queue age: %w", err)
	}
	out := QueueAge{GeneratedAt: now, Buckets: make([]AgeBucket, len(counts))}
	for i, b := range ageBucketBounds {
		ab := AgeBucket{Label: b.label, MinSeconds: b.min, Count: counts[i]}
		if b.max >= 0 {
			max := b.max
			ab.MaxSeconds = &max
		}
		out.Buckets[i] = ab
	}
	out.Capped = anyAtCap(counts...)
	return out, nil
}
