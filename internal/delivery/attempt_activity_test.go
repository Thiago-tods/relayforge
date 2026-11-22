package delivery

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

const attemptCursorID = "0192a3b4-c5d6-7e8f-9a0b-1c2d3e4f5a6b"

func TestAttemptCursorRoundTripsAndRefusesAnythingElse(t *testing.T) {
	at := time.Date(2026, 11, 18, 12, 30, 45, 123456000, time.UTC)
	text := AttemptCursor{StartedAt: at, ID: attemptCursorID}.Encode()
	if strings.ContainsAny(text, "=+/ ") {
		t.Fatalf("a cursor must be URL-safe without padding: %q", text)
	}
	got, err := ParseAttemptCursor(text)
	if err != nil || !got.StartedAt.Equal(at) || got.ID != attemptCursorID {
		t.Fatalf("round trip: %+v, %v", got, err)
	}
	if (AttemptCursor{StartedAt: at.In(time.FixedZone("x", -3*3600)), ID: attemptCursorID}).Encode() != text {
		t.Fatal("the cursor encodes the instant, not the zone")
	}
	enc := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	for name, bad := range map[string]string{
		"empty":             "",
		"not base64":        "!!!",
		"padded":            text + "=",
		"wrong version":     enc("a2:1700000000000000:" + attemptCursorID),
		"failure feed tag":  enc("v1:1700000000000000:" + attemptCursorID),
		"missing id":        enc("a1:1700000000000000"),
		"extra field":       enc("a1:1700000000000000:" + attemptCursorID + ":x"),
		"non numeric time":  enc("a1:soon:" + attemptCursorID),
		"signed time":       enc("a1:+1700000000000000:" + attemptCursorID),
		"leading zero time": enc("a1:01700000000000000:" + attemptCursorID),
		"upper case id":     enc("a1:1700000000000000:" + strings.ToUpper(attemptCursorID)),
		"short id":          enc("a1:1700000000000000:" + attemptCursorID[:35]),
		"year 0000":         enc("a1:-62167219200000000:" + attemptCursorID),
		"year 10000":        enc("a1:253402300800000000:" + attemptCursorID),
		"time overflows":    enc("a1:99999999999999999999:" + attemptCursorID),
		"sql in the id":     enc("a1:1700000000000000:' OR 1=1 --"),
	} {
		if _, err := ParseAttemptCursor(bad); !errors.Is(err, ErrInvalidAttemptCursor) {
			t.Errorf("%s: %q was accepted (err %v)", name, bad, err)
		}
	}
}

func TestAttemptAndFailureCursorsAreNotInterchangeable(t *testing.T) {
	at := time.Date(2026, 11, 18, 12, 30, 45, 0, time.UTC)
	if _, err := ParseFailureCursor(AttemptCursor{StartedAt: at, ID: attemptCursorID}.Encode()); !errors.Is(err, ErrInvalidCursor) {
		t.Errorf("the failure feed accepted an attempt cursor: %v", err)
	}
	if _, err := ParseAttemptCursor(FailureCursor{CompletedAt: at, ID: attemptCursorID}.Encode()); !errors.Is(err, ErrInvalidAttemptCursor) {
		t.Errorf("the attempt feed accepted a failure cursor: %v", err)
	}
}

func TestAttemptOutcomeFilterIsTheClosedSetOfSevenAndExactCase(t *testing.T) {
	want := []string{"SUCCESS", "HTTP_ERROR", "REDIRECT", "NETWORK_ERROR", "TIMEOUT", "BLOCKED_BY_POLICY", "ABANDONED"}
	var got []string
	for _, o := range AttemptOutcomes() {
		got = append(got, string(o))
		if !ValidAttemptOutcome(string(o)) {
			t.Errorf("%s must be a valid outcome", o)
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("outcomes = %v, want %v", got, want)
	}
	for _, bad := range []string{"", "success", "Success", "SUCCESS ", "FAILED", "ABANDON", "NONE", "SUCCESS' OR 1=1 --"} {
		if ValidAttemptOutcome(bad) {
			t.Errorf("%q must not be a valid outcome", bad)
		}
	}
	// AttemptOutcomes returns a copy: mutating it cannot change the authoritative set.
	list := AttemptOutcomes()
	list[0] = "HACKED"
	if !ValidAttemptOutcome("SUCCESS") || ValidAttemptOutcome("HACKED") || AttemptOutcomes()[0] != OutcomeSuccess {
		t.Fatal("the outcome set must not be mutable from outside the package")
	}
}

func TestAttemptFeedSQLIsScopedToTheOrganizationAndUsesOnlyBindParameters(t *testing.T) {
	from, to := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	after := &AttemptCursor{StartedAt: to.Add(-time.Hour), ID: attemptCursorID}
	deliveryID := "99999999-8888-4777-8666-555555555555"
	for name, f := range map[string]AttemptFilter{
		"plain":      {From: from, To: to, Limit: 50},
		"outcome":    {From: from, To: to, Limit: 50, Outcome: "ABANDONED"},
		"delivery":   {From: from, To: to, Limit: 7, DeliveryID: deliveryID},
		"cursor":     {From: from, To: to, Limit: 1, After: after},
		"everything": {From: from, To: to, Limit: 100, Outcome: "HTTP_ERROR", DeliveryID: deliveryID, After: after},
	} {
		sql, args, err := attemptFeedSQL(orgIDText, f)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, need := range []string{"a.organization_id = $1", "a.started_at >= $2", "a.started_at < $3",
			"d.organization_id = a.organization_id", "x.organization_id = d.organization_id", "ORDER BY a.started_at DESC, a.id DESC"} {
			if !strings.Contains(sql, need) {
				t.Errorf("%s: the statement lacks %q:\n%s", name, need, sql)
			}
		}
		if args[0] != orgIDText {
			t.Errorf("%s: $1 must be the organization", name)
		}
		for _, leaked := range []string{f.DeliveryID, attemptCursorID, "2026"} {
			if leaked != "" && strings.Contains(sql, leaked) {
				t.Errorf("%s: client data %q is in the statement text", name, leaked)
			}
		}
		if got, want := args[len(args)-1], f.Limit+1; got != want {
			t.Errorf("%s: the page reads one row more than the limit: %v != %d", name, got, want)
		}
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
		if strings.Contains(strings.ToLower(sql), "destination_id =") {
			t.Errorf("%s: there is no destination filter in this version:\n%s", name, sql)
		}
	}
	if _, _, err := attemptFeedSQL(orgIDText, AttemptFilter{Outcome: "nope", From: from, To: to, Limit: 5}); err == nil {
		t.Fatal("an unknown outcome must not build a statement")
	}
	sql, _, _ := attemptFeedSQL(orgIDText, AttemptFilter{From: from, To: to, Limit: 5})
	for _, banned := range []string{"url", "payload", "secret", "claim_token", "claimed_by", "lease_expires_at", "signing", "response"} {
		if strings.Contains(strings.ToLower(sql), banned) {
			t.Errorf("the attempt feed statement mentions %q", banned)
		}
	}
}

func TestAttemptFeedRefusesALimitOutsideItsRange(t *testing.T) {
	for _, limit := range []int{-1, 0, MaxAttemptFeedLimit + 1} {
		if _, err := AttemptFeedOf(context.Background(), nil, orgIDText, AttemptFilter{Limit: limit}, time.Now()); err == nil {
			t.Errorf("limit %d must be refused before any query", limit)
		}
	}
}

// The item exposes stored operational metadata only. This pins the exact JSON keys, so adding a field (and so possibly
// a leak) is a conscious change that has to edit this test.
func TestAttemptActivityItemExposesOnlyTheApprovedFields(t *testing.T) {
	status, class, reason := 503, "http_5xx", "RETRIES_EXHAUSTED"
	raw, err := json.Marshal(AttemptActivityItem{HTTPStatus: &status, ErrorClass: &class, TerminalReason: &reason})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	want := []string{"attemptId", "deliveryId", "eventId", "destinationId", "destinationName", "attemptNumber", "startedAt", "finishedAt",
		"durationMs", "outcome", "httpStatus", "errorClass", "terminalReason"}
	if len(m) != len(want) {
		t.Errorf("the item has %d keys, want %d: %v", len(m), len(want), m)
	}
	for _, k := range want {
		if _, ok := m[k]; !ok {
			t.Errorf("missing key %q", k)
		}
	}
	// Nullable fields are omitted when unset.
	raw, _ = json.Marshal(AttemptActivityItem{})
	m = map[string]any{}
	_ = json.Unmarshal(raw, &m)
	for _, k := range []string{"httpStatus", "errorClass", "terminalReason"} {
		if _, ok := m[k]; ok {
			t.Errorf("%s must be omitted when null", k)
		}
	}
	// An empty feed is an empty array and a null cursor, never absent.
	raw, _ = json.Marshal(AttemptFeed{Items: []AttemptActivityItem{}})
	if !strings.Contains(string(raw), `"items":[]`) || !strings.Contains(string(raw), `"nextCursor":null`) {
		t.Errorf("an empty page must serialize as items [] and nextCursor null: %s", raw)
	}
}
