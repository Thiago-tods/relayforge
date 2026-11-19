package delivery

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestFailureRateIsFailedOverSucceededPlusFailedAndNullWhenMeaningless(t *testing.T) {
	val := func(p *float64) any {
		if p == nil {
			return nil
		}
		return *p
	}
	for _, tc := range []struct {
		name              string
		succeeded, failed int
		want              any
	}{
		{"nothing completed", 0, 0, nil},
		{"only successes", 10, 0, 0.0},
		{"only failures", 0, 4, 1.0},
		{"a quarter", 3, 1, 0.25},
		{"the denominator ignores cancelled deliveries (they are not an input)", 99, 1, 0.01},
		{"succeeded saturated", CountCap, 5, nil},
		{"failed saturated", 5, CountCap, nil},
		{"both saturated", CountCap, CountCap, nil},
		{"just below the cap on both sides", CountCap - 1, CountCap - 1, 0.5},
	} {
		if got := val(failureRate(tc.succeeded, tc.failed)); got != tc.want {
			t.Errorf("%s: failureRate(%d, %d) = %v, want %v", tc.name, tc.succeeded, tc.failed, got, tc.want)
		}
	}
}

func TestAnyAtCapIsTrueOnlyWhenACounterReachedTheCap(t *testing.T) {
	if anyAtCap() || anyAtCap(0, 1, CountCap-1) {
		t.Fatal("counts below the cap are exact")
	}
	if !anyAtCap(0, CountCap) || !anyAtCap(CountCap+1) {
		t.Fatal("a counter at (or beyond) the cap is saturated")
	}
}

func jsonKeys(t *testing.T, v any) []string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func TestTheFailureReasonObjectHasExactlyTheTaxonomyPlusUnrecorded(t *testing.T) {
	want := []string{UnrecordedFailureReason}
	for _, r := range AllFailureReasons() {
		want = append(want, string(r))
	}
	sort.Strings(want)
	if got := jsonKeys(t, ReasonCounts{}); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("byFailureReason keys %v, want %v (a new reason must be added to ReasonCounts and its query)", got, want)
	}
	if len(want) != 6 {
		t.Fatalf("the contract is six keys, got %d", len(want))
	}
	// UNRECORDED is a response-only bucket: it must never be a storable reason.
	if FailureReason(UnrecordedFailureReason).Valid() {
		t.Fatal("UNRECORDED must not be part of the closed taxonomy")
	}
	statuses := jsonKeys(t, StatusCounts{})
	if strings.Join(statuses, ",") != "CANCELLED,FAILED,IN_FLIGHT,PENDING,SUCCEEDED" {
		t.Fatalf("byStatus keys %v", statuses)
	}
}

func TestOperationalResponsesUseCamelCaseAndAlwaysCarryTheirFields(t *testing.T) {
	if got := strings.Join(jsonKeys(t, QueueSnapshot{}), ","); got != "capped,delayed,due,expiredLeases,generatedAt,inFlight,oldestDueAgeSeconds,pending" {
		t.Fatalf("queue fields: %s", got)
	}
	if got := strings.Join(jsonKeys(t, DeliverySummary{}), ","); got != "capped,completed,created,from,generatedAt,to" {
		t.Fatalf("summary fields: %s", got)
	}
	if got := strings.Join(jsonKeys(t, CompletedCohort{}), ","); got != "byFailureReason,cancelled,failed,failureRate,succeeded" {
		t.Fatalf("completed fields: %s", got)
	}
	// A null rate is present as null, not omitted, so clients can rely on the field.
	raw, _ := json.Marshal(CompletedCohort{})
	if !strings.Contains(string(raw), `"failureRate":null`) {
		t.Fatalf("failureRate must serialize as null: %s", raw)
	}
	raw, _ = json.Marshal(QueueSnapshot{GeneratedAt: time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)})
	if !strings.Contains(string(raw), `"generatedAt":"2026-11-18T10:00:00Z"`) {
		t.Fatalf("timestamps are RFC 3339 in UTC: %s", raw)
	}
}

// TestOperationalStatementsAreScopedBoundedAndLockFree reads the SQL itself: every scan of the
// deliveries table is organization-scoped, every counter carries its own LIMIT (which bounds the
// matching rows it counts, not the rows PostgreSQL examines: the scale tests record that cost),
// nothing locks a row, and every value is a bind parameter.
func TestOperationalStatementsAreScopedBoundedAndLockFree(t *testing.T) {
	for name, sql := range map[string]string{"queue": QueueStatement, "summary": SummaryStatement} {
		scans := strings.Count(sql, "FROM deliveries WHERE")
		if scans == 0 || scans != strings.Count(sql, "FROM deliveries WHERE organization_id = $1") {
			t.Errorf("%s: every scan of deliveries must start with organization_id = $1", name)
		}
		if got, want := strings.Count(sql, "LIMIT 100000"), strings.Count(sql, "count(*)"); got != want {
			t.Errorf("%s: %d counters, %d with their own LIMIT", name, want, got)
		}
		for _, banned := range []string{"FOR UPDATE", "FOR SHARE", "FOR NO KEY", "OFFSET", "delivery_attempts", "destinations", "JOIN"} {
			if strings.Contains(strings.ToUpper(sql), strings.ToUpper(banned)) {
				t.Errorf("%s: the statement must not contain %q", name, banned)
			}
		}
		// The partial-index predicates are constants, never bind parameters.
		if strings.Contains(sql, "status = $") || strings.Contains(sql, "status IN ($") {
			t.Errorf("%s: a status bind parameter would not match a partial index", name)
		}
	}
	if CountCap != 100_000 {
		t.Fatalf("the approved cap is 100,000, got %d", CountCap)
	}
}
