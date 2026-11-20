package delivery

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const (
	cursorID  = "0192a3b4-c5d6-7e8f-9a0b-1c2d3e4f5a6b"
	orgIDText = "11111111-2222-4333-8444-555555555555"
)

func TestFailureCursorRoundTripsAndRefusesAnythingElse(t *testing.T) {
	at := time.Date(2026, 11, 18, 12, 30, 45, 123456000, time.UTC)
	c := FailureCursor{CompletedAt: at, ID: cursorID}
	text := c.Encode()
	if strings.ContainsAny(text, "=+/ ") {
		t.Fatalf("a cursor must be URL-safe without padding: %q", text)
	}
	got, err := ParseFailureCursor(text)
	if err != nil || !got.CompletedAt.Equal(at) || got.ID != cursorID {
		t.Fatalf("round trip: %+v, %v", got, err)
	}
	// A non-UTC time encodes the same instant.
	if (FailureCursor{CompletedAt: at.In(time.FixedZone("x", -3*3600)), ID: cursorID}).Encode() != text {
		t.Fatal("the cursor encodes the instant, not the zone")
	}

	enc := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	for name, bad := range map[string]string{
		"empty":                "",
		"not base64":           "!!!",
		"padded":               text + "=",
		"standard alphabet":    strings.NewReplacer("-", "+", "_", "/").Replace(enc("v1:1:" + cursorID + "?>>")),
		"wrong version":        enc("v2:1700000000000000:" + cursorID),
		"missing id":           enc("v1:1700000000000000"),
		"extra field":          enc("v1:1700000000000000:" + cursorID + ":x"),
		"non numeric time":     enc("v1:soon:" + cursorID),
		"empty time":           enc("v1::" + cursorID),
		"signed time":          enc("v1:+1700000000000000:" + cursorID),
		"leading zero time":    enc("v1:01700000000000000:" + cursorID),
		"upper case id":        enc("v1:1700000000000000:" + strings.ToUpper(cursorID)),
		"short id":             enc("v1:1700000000000000:" + cursorID[:35]),
		"id with a quote":      enc("v1:1700000000000000:0192a3b4-c5d6-7e8f-9a0b-1c2d3e4f5a6'"),
		"id not hex":           enc("v1:1700000000000000:0192a3b4-c5d6-7e8f-9a0b-1c2d3e4f5a6z"),
		"year 0000":            enc("v1:-62167219200000000:" + cursorID),
		"year 10000":           enc("v1:253402300800000000:" + cursorID),
		"time overflows int64": enc("v1:99999999999999999999:" + cursorID),
		"sql in the id":        enc("v1:1700000000000000:' OR 1=1 --"),
	} {
		if _, err := ParseFailureCursor(bad); !errors.Is(err, ErrInvalidCursor) {
			t.Errorf("%s: %q was accepted (err %v)", name, bad, err)
		}
	}
	// The earliest and latest supported instants are valid.
	for _, ok := range []time.Time{time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC)} {
		if _, err := ParseFailureCursor(FailureCursor{CompletedAt: ok, ID: cursorID}.Encode()); err != nil {
			t.Errorf("%v: %v", ok, err)
		}
	}
}

func TestFailureFilterValues(t *testing.T) {
	for _, ok := range []string{"DESTINATION_REJECTED", "REDIRECT_REFUSED", "EGRESS_BLOCKED", "RETRIES_EXHAUSTED", "INTERNAL_ERROR", "UNRECORDED"} {
		if !ValidFailureFilter(ok) {
			t.Errorf("%s must be accepted", ok)
		}
	}
	for _, bad := range []string{"", "unrecorded", "retries_exhausted", "FAILED", "NONE", "RETRIES_EXHAUSTED ", "' OR 1=1 --", "DESTINATION_REJECTED'"} {
		if ValidFailureFilter(bad) {
			t.Errorf("%q must be refused", bad)
		}
	}
	if c, err := failureReasonClause(""); c != "" || err != nil {
		t.Errorf("no filter: %q, %v", c, err)
	}
	if c, _ := failureReasonClause("UNRECORDED"); c != " AND d.status = 'FAILED' AND d.failure_reason IS NULL" {
		t.Errorf("UNRECORDED means FAILED with a NULL reason: %q", c)
	}
	for _, r := range AllFailureReasons() {
		c, err := failureReasonClause(string(r))
		if err != nil || c != " AND d.status = 'FAILED' AND d.failure_reason = '"+string(r)+"'" {
			t.Errorf("%s: %q, %v", r, c, err)
		}
	}
	if c, err := failureReasonClause("x' OR '1'='1"); err == nil || c != "" {
		t.Errorf("client text must never become SQL: %q, %v", c, err)
	}
}

func TestFailureFeedSQLIsScopedToTheOrganizationAndUsesOnlyBindParameters(t *testing.T) {
	from, to := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)
	after := &FailureCursor{CompletedAt: to.Add(-time.Hour), ID: cursorID}
	for name, f := range map[string]FailureFilter{
		"plain":       {From: from, To: to, Limit: 50},
		"reason":      {From: from, To: to, Limit: 50, Reason: "RETRIES_EXHAUSTED"},
		"unrecorded":  {From: from, To: to, Limit: 50, Reason: "UNRECORDED"},
		"destination": {From: from, To: to, Limit: 7, DestinationID: "99999999-8888-4777-8666-555555555555"},
		"cursor":      {From: from, To: to, Limit: 1, After: after},
		"everything":  {From: from, To: to, Limit: 100, Reason: "INTERNAL_ERROR", DestinationID: "99999999-8888-4777-8666-555555555555", After: after},
	} {
		sql, args, err := failureFeedSQL(orgIDText, f)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, need := range []string{"d.organization_id = $1", "d.status = 'FAILED'", "d.completed_at >= $2", "d.completed_at < $3",
			"x.organization_id = d.organization_id", "ORDER BY d.completed_at DESC, d.id DESC"} {
			if !strings.Contains(sql, need) {
				t.Errorf("%s: the statement lacks %q:\n%s", name, need, sql)
			}
		}
		if args[0] != orgIDText {
			t.Errorf("%s: $1 must be the organization", name)
		}
		// Nothing a client sent appears in the text: no destination id, no cursor, no timestamp.
		for _, leaked := range []string{f.DestinationID, cursorID, "2026"} {
			if leaked != "" && strings.Contains(sql, leaked) {
				t.Errorf("%s: client data %q is in the statement text", name, leaked)
			}
		}
		if got, want := args[len(args)-1], f.Limit+1; got != want {
			t.Errorf("%s: the page reads one row more than the limit to learn whether there is a next page: %v != %d", name, got, want)
		}
		// Every placeholder has an argument and the other way round.
		max := 0
		for i := len(args); i >= 1; i-- {
			if strings.Contains(sql, "$"+itoa(i)) {
				max = i
				break
			}
		}
		if max != len(args) {
			t.Errorf("%s: %d arguments but the highest placeholder is $%d", name, len(args), max)
		}
		if strings.Contains(sql, "OFFSET") || strings.Contains(strings.ToLower(sql), "count(") {
			t.Errorf("%s: the feed uses keyset pagination and never counts:\n%s", name, sql)
		}
	}
	if _, _, err := failureFeedSQL(orgIDText, FailureFilter{Reason: "nope", From: from, To: to, Limit: 5}); err == nil {
		t.Fatal("an unknown reason must not build a statement")
	}
	// None of the sensitive columns is selected.
	sql, _, _ := failureFeedSQL(orgIDText, FailureFilter{From: from, To: to, Limit: 5})
	for _, banned := range []string{"url", "payload", "secret", "claim_token", "claimed_by", "lease_expires_at", "signing"} {
		if strings.Contains(strings.ToLower(sql), banned) {
			t.Errorf("the feed statement mentions %q", banned)
		}
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}

func TestFailureFeedRefusesALimitOutsideItsRange(t *testing.T) {
	for _, limit := range []int{-1, 0, MaxFailureFeedLimit + 1} {
		if _, err := FailureFeedOf(context.Background(), nil, orgIDText, FailureFilter{Limit: limit}, time.Now()); err == nil {
			t.Errorf("limit %d must be refused before any query", limit)
		}
	}
}

func TestQueueAgeBucketsAreContiguousFixedAndOnlyTheLastIsOpen(t *testing.T) {
	want := []struct {
		label    string
		min, max int64
	}{{"<1m", 0, 60}, {"1-5m", 60, 300}, {"5-15m", 300, 900}, {"15-60m", 900, 3600}, {"1-6h", 3600, 21600}, {"6-24h", 21600, 86400}, {">24h", 86400, -1}}
	if len(ageBucketBounds) != len(want) {
		t.Fatalf("%d buckets, want %d", len(ageBucketBounds), len(want))
	}
	for i, w := range want {
		if b := ageBucketBounds[i]; b.label != w.label || b.min != w.min || b.max != w.max {
			t.Errorf("bucket %d = %+v, want %+v", i, b, w)
		}
		if i > 0 && ageBucketBounds[i].min != ageBucketBounds[i-1].max {
			t.Errorf("bucket %d must start where bucket %d ends (half-open, no gap, no overlap)", i, i-1)
		}
	}
	// Every bucket has its own LIMIT, so each one saturates on its own, and every comparison uses the one instant $2.
	sql := queueAgeSQL()
	if strings.Count(sql, "LIMIT") != len(want) || strings.Count(sql, "SELECT count(*)") != len(want) {
		t.Fatalf("each bucket is an independent capped counter: %s", sql)
	}
	if strings.Contains(sql, "now()") || strings.Contains(sql, "$3") || strings.Contains(strings.ToLower(sql), "join") {
		t.Errorf("one bind instant, no clock of the database, no join:\n%s", sql)
	}
	if strings.Count(sql, "organization_id = $1") != len(want) || strings.Count(sql, "status = 'PENDING'") != len(want) {
		t.Errorf("every bucket is scoped to the organization and to PENDING with a literal:\n%s", sql)
	}
}

// recordingQuerier records the statements of a call without a database. QueryRow answers a count of zero.
type recordingQuerier struct{ sqls []string }

type zeroRow struct{}

func (zeroRow) Scan(dest ...any) error {
	for _, d := range dest {
		if p, ok := d.(*int); ok {
			*p = 0
		}
	}
	return nil
}

func (r *recordingQuerier) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errors.New("not expected")
}
func (r *recordingQuerier) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	r.sqls = append(r.sqls, sql)
	return nil, errors.New("stop")
}
func (r *recordingQuerier) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	r.sqls = append(r.sqls, sql)
	return zeroRow{}
}

// TestListDeliveriesWithoutAReasonRunsTheStatementsItAlwaysRan is the byte-compatibility guard: the
// two statements of an unfiltered list are the ones that shipped before ADR 32, character for character.
func TestListDeliveriesWithoutAReasonRunsTheStatementsItAlwaysRan(t *testing.T) {
	const oldWhere = ` WHERE d.organization_id = $1
		AND ($2::uuid IS NULL OR d.destination_id = $2::uuid)
		AND ($3::uuid IS NULL OR d.event_id = $3::uuid)
		AND ($4 = '' OR d.status = $4)
		AND ($5::timestamptz IS NULL OR d.created_at >= $5)
		AND ($6::timestamptz IS NULL OR d.created_at < $6)`
	for _, f := range []HistoryFilter{{}, {Status: "FAILED"}, {DestinationID: "x"}} {
		r := &recordingQuerier{}
		_, _, _ = ListDeliveries(context.Background(), r, orgIDText, f, 20, 0)
		if len(r.sqls) != 2 {
			t.Fatalf("%d statements, want the count and the page", len(r.sqls))
		}
		if r.sqls[0] != "SELECT count(*) FROM deliveries d"+oldWhere {
			t.Errorf("the count statement changed:\n%s", r.sqls[0])
		}
		if r.sqls[1] != deliverySelect+oldWhere+" ORDER BY d.created_at DESC, d.id DESC LIMIT $7 OFFSET $8" {
			t.Errorf("the page statement changed:\n%s", r.sqls[1])
		}
	}
	// A filtered list adds exactly the reason clause to both, and keeps ordering and pagination.
	r := &recordingQuerier{}
	_, _, _ = ListDeliveries(context.Background(), r, orgIDText, HistoryFilter{FailureReason: "UNRECORDED"}, 20, 0)
	clause := " AND d.status = 'FAILED' AND d.failure_reason IS NULL"
	if len(r.sqls) != 2 || r.sqls[0] != "SELECT count(*) FROM deliveries d"+oldWhere+clause ||
		r.sqls[1] != deliverySelect+oldWhere+clause+" ORDER BY d.created_at DESC, d.id DESC LIMIT $7 OFFSET $8" {
		t.Errorf("a filtered list differs by the reason clause only: %q", r.sqls)
	}
	if _, _, err := ListDeliveries(context.Background(), &recordingQuerier{}, orgIDText, HistoryFilter{FailureReason: "bogus"}, 20, 0); err == nil {
		t.Error("an unknown reason must not reach the database")
	}
}
