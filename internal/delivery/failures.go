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

// Recent-failure feed and failure-reason filter (ADR 32).

// ErrInvalidCursor is returned for a cursor that this package did not issue, or that was altered.
var ErrInvalidCursor = errors.New("invalid failure feed cursor")

// DefaultFailureFeedLimit and MaxFailureFeedLimit are the page size bounds of the feed.
const (
	DefaultFailureFeedLimit = 50
	MaxFailureFeedLimit     = 100
)

// ValidFailureFilter reports whether s is a value of the failureReason filter: one of the five
// persisted reasons, or UNRECORDED (a FAILED delivery whose reason was never recorded).
func ValidFailureFilter(s string) bool {
	return s == UnrecordedFailureReason || FailureReason(s).Valid()
}

// failureReasonClause returns the SQL predicate of a failureReason filter. The value is checked
// against the closed taxonomy first, so the returned text is always one of six constants and client
// text never reaches the statement. A literal (not a bind parameter) also lets the planner see it.
func failureReasonClause(filter string) (string, error) {
	switch {
	case filter == "":
		return "", nil
	case filter == UnrecordedFailureReason:
		return " AND d.status = 'FAILED' AND d.failure_reason IS NULL", nil
	case FailureReason(filter).Valid():
		return " AND d.status = 'FAILED' AND d.failure_reason = '" + filter + "'", nil
	}
	return "", fmt.Errorf("unknown failure reason filter %q", filter)
}

// FailureCursor is the keyset position of the feed: the (completed_at, id) of the last item of the
// previous page. It carries a position and nothing else. It is NOT a capability: the organization
// predicate is applied to every query whatever the cursor says, so a cursor copied from another
// tenant can only move a position inside the caller's own data.
type FailureCursor struct {
	CompletedAt time.Time
	ID          string
}

const cursorVersion = "v1"

// Encode returns the opaque cursor text: base64url of "v1:<unix microseconds>:<delivery id>".
func (c FailureCursor) Encode() string {
	return base64.RawURLEncoding.EncodeToString([]byte(cursorVersion + ":" + strconv.FormatInt(c.CompletedAt.UTC().UnixMicro(), 10) + ":" + c.ID))
}

// ParseFailureCursor decodes a cursor. It is strict: the text must be canonical base64url, have the
// exact shape, a timestamp in years 0001 to 9999 and a lowercase UUID, and it must re-encode to the same
// text, so a cursor that was altered in any way (padding, case, extra fields) is refused.
func ParseFailureCursor(s string) (FailureCursor, error) {
	raw, err := base64.RawURLEncoding.Strict().DecodeString(s)
	if err != nil {
		return FailureCursor{}, ErrInvalidCursor
	}
	parts := strings.Split(string(raw), ":")
	if len(parts) != 3 || parts[0] != cursorVersion {
		return FailureCursor{}, ErrInvalidCursor
	}
	micros, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || !isLowerUUID(parts[2]) {
		return FailureCursor{}, ErrInvalidCursor
	}
	at := time.UnixMicro(micros).UTC()
	if y := at.Year(); y < 1 || y > 9999 {
		return FailureCursor{}, ErrInvalidCursor
	}
	c := FailureCursor{CompletedAt: at, ID: parts[2]}
	if c.Encode() != s {
		return FailureCursor{}, ErrInvalidCursor
	}
	return c, nil
}

func isLowerUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch {
		case i == 8 || i == 13 || i == 18 || i == 23:
			if r != '-' {
				return false
			}
		case (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f'):
		default:
			return false
		}
	}
	return true
}

// FailureItem is one currently failed delivery. It is operational metadata only: no payload, no
// destination URL, no secret, no raw error and none of the claim fields (token, worker, lease).
// FailureReason is always present: a delivery that failed before reasons were recorded is UNRECORDED.
type FailureItem struct {
	DeliveryID      string    `json:"deliveryId"`
	EventID         string    `json:"eventId"`
	DestinationID   string    `json:"destinationId"`
	DestinationName string    `json:"destinationName"`
	FailureReason   string    `json:"failureReason"`
	AttemptCount    int       `json:"attemptCount"`
	LastHTTPStatus  *int      `json:"lastHttpStatus,omitempty"`
	LastErrorClass  *string   `json:"lastErrorClass,omitempty"`
	CreatedAt       time.Time `json:"createdAt"`
	CompletedAt     time.Time `json:"completedAt"`
}

// FailureFeed is one page of the feed. NextCursor is null on the last page.
type FailureFeed struct {
	GeneratedAt time.Time     `json:"generatedAt"`
	From        time.Time     `json:"from"`
	To          time.Time     `json:"to"`
	Items       []FailureItem `json:"items"`
	NextCursor  *string       `json:"nextCursor"`
}

// FailureFilter narrows the feed. From (inclusive) and To (exclusive) bound completed_at and must already
// be normalized by the caller. Reason is "", one of the five reasons, or UnrecordedFailureReason. After, when
// set, continues after that position. Limit is 1 to MaxFailureFeedLimit.
type FailureFilter struct {
	DestinationID string
	Reason        string
	From, To      time.Time
	After         *FailureCursor
	Limit         int
}

// failureFeedSQL builds the statement and its arguments. Only constant fragments are concatenated; every
// value is a bind parameter, except the reason clause, which is one of six constants.
//
// A delivery is a current failure when it is FAILED and completed. A delivery that is later redriven
// (status back to PENDING, reason and completed_at cleared) therefore leaves the feed by itself.
// The status literal and the completed_at range are what let the planner use a partial index.
func failureFeedSQL(orgID string, f FailureFilter) (string, []any, error) {
	reason, err := failureReasonClause(f.Reason)
	if err != nil {
		return "", nil, err
	}
	args := []any{orgID, f.From.UTC(), f.To.UTC()}
	var b strings.Builder
	b.WriteString(`SELECT d.id::text, d.event_id::text, d.destination_id::text, x.name, COALESCE(d.failure_reason, '` + UnrecordedFailureReason + `'),
	d.attempt_count, d.last_http_status::int, d.last_error_class, d.created_at, d.completed_at
	FROM deliveries d
	JOIN destinations x ON x.id = d.destination_id AND x.organization_id = d.organization_id
	WHERE d.organization_id = $1 AND d.status = 'FAILED' AND d.completed_at >= $2 AND d.completed_at < $3`)
	b.WriteString(reason)
	if f.DestinationID != "" {
		args = append(args, f.DestinationID)
		fmt.Fprintf(&b, " AND d.destination_id = $%d::uuid", len(args))
	}
	if f.After != nil {
		args = append(args, f.After.CompletedAt.UTC(), f.After.ID)
		fmt.Fprintf(&b, " AND (d.completed_at, d.id) < ($%d::timestamptz, $%d::uuid)", len(args)-1, len(args))
	}
	args = append(args, f.Limit+1)
	fmt.Fprintf(&b, " ORDER BY d.completed_at DESC, d.id DESC LIMIT $%d", len(args))
	return b.String(), args, nil
}

// FailureFeedOf returns one page of an organization's current failures, newest completion first. It
// uses keyset pagination and fetches one row more than the page to learn whether another page exists, so
// it never counts anything. It takes no row lock. now is reported as GeneratedAt and is not used in the SQL.
//
// Consistency: a page is one statement, hence one snapshot. Across pages the feed is eventually consistent:
// completed_at is stamped by the worker clock when it finishes a delivery, so a delivery that commits late can carry a
// completed_at older than rows a client has already read, and a continuation from a cursor never revisits that
// part of the order. A client that wants the newest failures starts again from the first page.
func FailureFeedOf(ctx context.Context, db database.Querier, orgID string, f FailureFilter, now time.Time) (FailureFeed, error) {
	if f.Limit < 1 || f.Limit > MaxFailureFeedLimit {
		return FailureFeed{}, fmt.Errorf("failure feed limit %d out of range", f.Limit)
	}
	sql, args, err := failureFeedSQL(orgID, f)
	if err != nil {
		return FailureFeed{}, err
	}
	rows, err := db.Query(ctx, sql, args...)
	if err != nil {
		return FailureFeed{}, fmt.Errorf("read failure feed: %w", err)
	}
	defer rows.Close()
	feed := FailureFeed{GeneratedAt: now.UTC(), From: f.From.UTC(), To: f.To.UTC(), Items: []FailureItem{}}
	for rows.Next() {
		var it FailureItem
		if err := rows.Scan(&it.DeliveryID, &it.EventID, &it.DestinationID, &it.DestinationName, &it.FailureReason, &it.AttemptCount,
			&it.LastHTTPStatus, &it.LastErrorClass, &it.CreatedAt, &it.CompletedAt); err != nil {
			return FailureFeed{}, fmt.Errorf("scan failure: %w", err)
		}
		it.CreatedAt, it.CompletedAt = it.CreatedAt.UTC(), it.CompletedAt.UTC()
		feed.Items = append(feed.Items, it)
	}
	if err := rows.Err(); err != nil {
		return FailureFeed{}, fmt.Errorf("read failure feed: %w", err)
	}
	if len(feed.Items) > f.Limit {
		feed.Items = feed.Items[:f.Limit]
		last := feed.Items[f.Limit-1]
		next := FailureCursor{CompletedAt: last.CompletedAt, ID: last.DeliveryID}.Encode()
		feed.NextCursor = &next
	}
	return feed, nil
}
