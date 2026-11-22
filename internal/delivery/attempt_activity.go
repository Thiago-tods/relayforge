package delivery

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Thiago-tods/relayforge/internal/database"
)

// Attempt activity feed (ADR 33): a cross-delivery, newest-first view of recorded attempts.

// ErrInvalidAttemptCursor is returned for a cursor that this package did not issue, or that was altered.
var ErrInvalidAttemptCursor = errors.New("invalid attempt feed cursor")

// DefaultAttemptFeedLimit and MaxAttemptFeedLimit are the page size bounds of the attempt feed.
const (
	DefaultAttemptFeedLimit = 50
	MaxAttemptFeedLimit     = 100
)

// attemptOutcomes is the closed set of attempt outcomes (the delivery_attempts_outcome_valid constraint).
var attemptOutcomes = [...]Outcome{
	OutcomeSuccess, OutcomeHTTPError, OutcomeRedirect, OutcomeNetworkError,
	OutcomeTimeout, OutcomeBlockedByPolicy, OutcomeAbandoned,
}

// AttemptOutcomes returns the seven attempt outcomes, as a copy so callers cannot change the set.
func AttemptOutcomes() []Outcome {
	out := make([]Outcome, len(attemptOutcomes))
	copy(out, attemptOutcomes[:])
	return out
}

// ValidAttemptOutcome reports whether s is one of the seven attempt outcomes (exact, upper case).
func ValidAttemptOutcome(s string) bool {
	for _, o := range attemptOutcomes {
		if string(o) == s {
			return true
		}
	}
	return false
}

// AttemptCursor is the keyset position of the attempt feed: the (started_at, id) of the last item of the
// previous page. It carries a position and nothing else. It is NOT a capability: the organization predicate
// is applied to every query whatever the cursor says, so a cursor copied from another tenant can only move a
// position inside the caller's own data. Its version tag differs from the failure feed's, so a cursor of one
// feed is refused by the other.
type AttemptCursor struct {
	StartedAt time.Time
	ID        string
}

const attemptCursorVersion = "a1"

// Encode returns the opaque cursor text: base64url of "a1:<unix microseconds>:<attempt id>".
func (c AttemptCursor) Encode() string {
	return base64.RawURLEncoding.EncodeToString([]byte(attemptCursorVersion + ":" + strconv.FormatInt(c.StartedAt.UTC().UnixMicro(), 10) + ":" + c.ID))
}

// ParseAttemptCursor decodes a cursor. It is strict, like ParseFailureCursor: canonical base64url, the exact
// shape, a timestamp in years 0001 to 9999 and a lowercase UUID, and it must re-encode to the same text.
func ParseAttemptCursor(s string) (AttemptCursor, error) {
	raw, err := base64.RawURLEncoding.Strict().DecodeString(s)
	if err != nil {
		return AttemptCursor{}, ErrInvalidAttemptCursor
	}
	parts := strings.Split(string(raw), ":")
	if len(parts) != 3 || parts[0] != attemptCursorVersion {
		return AttemptCursor{}, ErrInvalidAttemptCursor
	}
	micros, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || !isLowerUUID(parts[2]) {
		return AttemptCursor{}, ErrInvalidAttemptCursor
	}
	at := time.UnixMicro(micros).UTC()
	if y := at.Year(); y < 1 || y > 9999 {
		return AttemptCursor{}, ErrInvalidAttemptCursor
	}
	c := AttemptCursor{StartedAt: at, ID: parts[2]}
	if c.Encode() != s {
		return AttemptCursor{}, ErrInvalidAttemptCursor
	}
	return c, nil
}

// AttemptActivityItem is one recorded attempt. It is operational metadata only: no payload, no response body,
// no destination URL, no secret, no raw error and none of the claim fields (token, worker, lease). Only stored
// fields are returned: nothing is derived. ErrorClass is the bounded identifier the worker records, never a raw
// error message. TerminalReason is set only on an attempt that ended a failure episode of its delivery (it made the delivery FAILED). A redriven delivery that failed again has one such attempt per episode (ADR 34).
type AttemptActivityItem struct {
	AttemptID       string    `json:"attemptId"`
	DeliveryID      string    `json:"deliveryId"`
	EventID         string    `json:"eventId"`
	DestinationID   string    `json:"destinationId"`
	DestinationName string    `json:"destinationName"`
	AttemptNumber   int       `json:"attemptNumber"`
	StartedAt       time.Time `json:"startedAt"`
	FinishedAt      time.Time `json:"finishedAt"`
	DurationMS      int       `json:"durationMs"`
	Outcome         string    `json:"outcome"`
	HTTPStatus      *int      `json:"httpStatus,omitempty"`
	ErrorClass      *string   `json:"errorClass,omitempty"`
	TerminalReason  *string   `json:"terminalReason,omitempty"`
}

// AttemptFeed is one page of the attempt feed. NextCursor is null on the last page.
type AttemptFeed struct {
	GeneratedAt time.Time             `json:"generatedAt"`
	From        time.Time             `json:"from"`
	To          time.Time             `json:"to"`
	Items       []AttemptActivityItem `json:"items"`
	NextCursor  *string               `json:"nextCursor"`
}

// AttemptFilter narrows the feed. From (inclusive) and To (exclusive) bound started_at and must already be
// normalized by the caller. Outcome is "" or one of the seven outcomes. DeliveryID is "" or a UUID. After, when set,
// continues after that position. Limit is 1 to MaxAttemptFeedLimit. There is deliberately no destination filter.
type AttemptFilter struct {
	DeliveryID string
	Outcome    string
	From, To   time.Time
	After      *AttemptCursor
	Limit      int
}

// attemptFeedSQL builds the statement and its arguments. Only constant fragments are concatenated: every value is
// a bind parameter, except the outcome, which is checked against the closed set first and so is one of seven
// constants (a literal also lets the planner see it).
//
// The organization predicate is on the attempt, and again on both joins, so a row of another tenant can never be
// returned whatever the other parameters say. The order, the window and the cursor all use the key of
// delivery_attempts_history_idx (organization_id, started_at DESC, id DESC), so the scan is bounded by LIMIT and not
// by the size of the table or of the window.
func attemptFeedSQL(orgID string, f AttemptFilter) (string, []any, error) {
	args := []any{orgID, f.From.UTC(), f.To.UTC()}
	var b strings.Builder
	b.WriteString(`SELECT a.id::text, a.delivery_id::text, d.event_id::text, d.destination_id::text, x.name, a.attempt_number,
	a.started_at, a.finished_at, a.duration_ms, a.outcome, a.http_status::int, a.error_class, a.terminal_reason
	FROM delivery_attempts a
	JOIN deliveries d ON d.id = a.delivery_id AND d.organization_id = a.organization_id
	JOIN destinations x ON x.id = d.destination_id AND x.organization_id = d.organization_id
	WHERE a.organization_id = $1 AND a.started_at >= $2 AND a.started_at < $3`)
	if f.Outcome != "" {
		if !ValidAttemptOutcome(f.Outcome) {
			return "", nil, fmt.Errorf("unknown attempt outcome filter %q", f.Outcome)
		}
		b.WriteString(" AND a.outcome = '" + f.Outcome + "'")
	}
	if f.DeliveryID != "" {
		args = append(args, f.DeliveryID)
		fmt.Fprintf(&b, " AND a.delivery_id = $%d::uuid", len(args))
	}
	if f.After != nil {
		args = append(args, f.After.StartedAt.UTC(), f.After.ID)
		fmt.Fprintf(&b, " AND (a.started_at, a.id) < ($%d::timestamptz, $%d::uuid)", len(args)-1, len(args))
	}
	args = append(args, f.Limit+1)
	fmt.Fprintf(&b, " ORDER BY a.started_at DESC, a.id DESC LIMIT $%d", len(args))
	return b.String(), args, nil
}

// AttemptFeedOf returns one page of an organization's recorded attempts, newest start first. It uses keyset
// pagination and fetches one row more than the page to learn whether another page exists, so it never counts
// anything. It takes no row lock. now is reported as GeneratedAt and is not used in the SQL.
//
// Consistency: a page is one statement, hence one snapshot. The feed is a live tail, as the failure feed is:
// started_at is stamped by the worker clock (or, for an attempt recorded by lease recovery, taken from the claim
// time), so an attempt that commits late can carry a started_at older than rows a client has already read, and a
// continuation from a cursor never revisits that part of the order. A client that wants the newest attempts
// starts again from the first page. An ABANDONED attempt is an attempt too: it says a worker did not finish, not
// that the destination failed.
func AttemptFeedOf(ctx context.Context, db database.Querier, orgID string, f AttemptFilter, now time.Time) (AttemptFeed, error) {
	if f.Limit < 1 || f.Limit > MaxAttemptFeedLimit {
		return AttemptFeed{}, fmt.Errorf("attempt feed limit %d out of range", f.Limit)
	}
	sql, args, err := attemptFeedSQL(orgID, f)
	if err != nil {
		return AttemptFeed{}, err
	}
	rows, err := db.Query(ctx, sql, args...)
	if err != nil {
		return AttemptFeed{}, fmt.Errorf("read attempt feed: %w", err)
	}
	defer rows.Close()
	feed := AttemptFeed{GeneratedAt: now.UTC(), From: f.From.UTC(), To: f.To.UTC(), Items: []AttemptActivityItem{}}
	for rows.Next() {
		var it AttemptActivityItem
		if err := rows.Scan(&it.AttemptID, &it.DeliveryID, &it.EventID, &it.DestinationID, &it.DestinationName, &it.AttemptNumber,
			&it.StartedAt, &it.FinishedAt, &it.DurationMS, &it.Outcome, &it.HTTPStatus, &it.ErrorClass, &it.TerminalReason); err != nil {
			return AttemptFeed{}, fmt.Errorf("scan attempt: %w", err)
		}
		it.StartedAt, it.FinishedAt = it.StartedAt.UTC(), it.FinishedAt.UTC()
		feed.Items = append(feed.Items, it)
	}
	if err := rows.Err(); err != nil {
		return AttemptFeed{}, fmt.Errorf("read attempt feed: %w", err)
	}
	if len(feed.Items) > f.Limit {
		feed.Items = feed.Items[:f.Limit]
		last := feed.Items[f.Limit-1]
		next := AttemptCursor{StartedAt: last.StartedAt, ID: last.AttemptID}.Encode()
		feed.NextCursor = &next
	}
	return feed, nil
}
