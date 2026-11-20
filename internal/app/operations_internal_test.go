package app

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/Thiago-tods/relayforge/internal/platform/apperror"
)

// The failure feed uses the same window rules with a seven-day default span (ADR 32).
func TestFailureFeedWindowDefaultsToSevenDaysAndKeepsTheStageOneLimits(t *testing.T) {
	now := time.Date(2026, 11, 18, 12, 0, 0, 123456000, time.UTC)
	from, to, err := operationsWindowOf("", "", now, defaultFailureFeedWindow)
	if err != nil || !to.Equal(now) || !from.Equal(now.Add(-7*24*time.Hour)) {
		t.Fatalf("default window: %v %v %v", from, to, err)
	}
	// Only `to`: the default span counts back from it.
	from, to, err = operationsWindowOf("", "2026-11-10T00:00:00Z", now, defaultFailureFeedWindow)
	if err != nil || !from.Equal(time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)) || !to.Equal(time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("only to: %v %v %v", from, to, err)
	}
	// 31 days is the longest span, a microsecond more is refused, and normalization is shared with Stage 1.
	if _, _, err := operationsWindowOf("2026-10-01T00:00:00Z", "2026-11-01T00:00:00Z", now, defaultFailureFeedWindow); err != nil {
		t.Fatalf("exactly 31 days: %v", err)
	}
	if _, _, err := operationsWindowOf("2026-10-01T00:00:00Z", "2026-11-01T00:00:00.000001Z", now, defaultFailureFeedWindow); err == nil {
		t.Fatal("31 days and a microsecond must be refused")
	}
	from, to, err = operationsWindowOf("2026-11-18T09:00:00.1234567-03:00", "2026-11-18T10:00:00-03:00", now, defaultFailureFeedWindow)
	if err != nil || !from.Equal(time.Date(2026, 11, 18, 12, 0, 0, 123456000, time.UTC)) || to.Location() != time.UTC {
		t.Fatalf("offsets and microseconds: %v %v %v", from, to, err)
	}
	// The summary default is untouched.
	if f, _, err := operationsWindow("", "", now); err != nil || !f.Equal(now.Add(-24*time.Hour)) {
		t.Fatalf("the summary still defaults to 24 hours: %v %v", f, err)
	}
}

func TestOperationsWindowDefaultsLimitsAndOffsets(t *testing.T) {
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	type want struct {
		from, to time.Time
		invalid  string
	}
	at := func(s string) time.Time {
		ts, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return ts.UTC()
	}
	for _, tc := range []struct {
		name, from, to string
		want           want
	}{
		{"defaults", "", "", want{from: now.Add(-24 * time.Hour), to: now}},
		{"only from", "2026-11-18T00:00:00Z", "", want{from: at("2026-11-18T00:00:00Z"), to: now}},
		{"only to", "", "2026-11-18T06:00:00Z", want{from: at("2026-11-17T06:00:00Z"), to: at("2026-11-18T06:00:00Z")}},
		{"offsets become UTC", "2026-11-18T09:00:00-03:00", "2026-11-18T16:00:00+02:00", want{from: at("2026-11-18T12:00:00Z"), to: at("2026-11-18T14:00:00Z")}},
		{"fractional seconds", "2026-11-18T09:00:00.5Z", "2026-11-18T10:00:00.25Z", want{from: at("2026-11-18T09:00:00.5Z"), to: at("2026-11-18T10:00:00.25Z")}},
		{"exactly 31 days", "2026-10-01T00:00:00Z", "2026-11-01T00:00:00Z", want{from: at("2026-10-01T00:00:00Z"), to: at("2026-11-01T00:00:00Z")}},
		{"31 days and one microsecond", "2026-10-01T00:00:00Z", "2026-11-01T00:00:00.000001Z", want{invalid: "the time window must not exceed 31 days"}},
		// PostgreSQL stores microseconds, so a bound is truncated to them and the response echoes what was read.
		{"nanoseconds are truncated: 31 days and one nanosecond is exactly 31 days", "2026-10-01T00:00:00Z", "2026-11-01T00:00:00.000000001Z",
			want{from: at("2026-10-01T00:00:00Z"), to: at("2026-11-01T00:00:00Z")}},
		{"sub-microsecond bounds truncate", "2026-11-18T09:00:00.1234567Z", "2026-11-18T10:00:00.9999999Z",
			want{from: at("2026-11-18T09:00:00.123456Z"), to: at("2026-11-18T10:00:00.999999Z")}},
		{"a window that is empty after truncation", "2026-11-18T09:00:00.0000001Z", "2026-11-18T09:00:00.0000009Z", want{invalid: "from must be before to"}},
		// Year 0000 is valid RFC 3339 but PostgreSQL has no year zero; years beyond 0001..9999 (UTC) are refused.
		{"year 0000 in from", "0000-06-01T00:00:00Z", "0000-06-02T00:00:00Z", want{invalid: "from must be a time between the years 0001 and 9999 (UTC)"}},
		{"year 0000 in to only", "0001-12-31T00:00:00Z", "0000-01-01T00:00:00Z", want{invalid: "to must be a time between the years 0001 and 9999 (UTC)"}},
		{"an offset moves 0001 into year 0000", "0001-01-01T00:00:00+14:00", "0001-01-02T00:00:00Z", want{invalid: "from must be a time between the years 0001 and 9999 (UTC)"}},
		{"an offset moves 9999 into year 10000", "9999-12-31T00:00:00Z", "9999-12-31T23:59:59-05:00", want{invalid: "to must be a time between the years 0001 and 9999 (UTC)"}},
		{"a default from before year 0001", "", "0001-01-01T12:00:00Z", want{invalid: "from must be a time between the years 0001 and 9999 (UTC)"}},
		{"the first supported instant", "0001-01-01T00:00:00Z", "0001-01-02T00:00:00Z", want{from: at("0001-01-01T00:00:00Z"), to: at("0001-01-02T00:00:00Z")}},
		{"the last supported instant", "9999-12-31T00:00:00Z", "9999-12-31T23:59:59.999999Z", want{from: at("9999-12-31T00:00:00Z"), to: at("9999-12-31T23:59:59.999999Z")}},
		{"a very wide valid span", "0001-01-01T00:00:00Z", "9999-12-31T23:59:59Z", want{invalid: "the time window must not exceed 31 days"}},
		{"a leap second is not a timestamp", "2026-06-30T23:59:60Z", "", want{invalid: "from must be an RFC 3339 timestamp"}},
		{"a five-digit year is not RFC 3339", "10000-01-01T00:00:00Z", "", want{invalid: "from must be an RFC 3339 timestamp"}},
		{"from equals to", "2026-11-18T00:00:00Z", "2026-11-18T00:00:00Z", want{invalid: "from must be before to"}},
		{"from after to", "2026-11-18T05:00:00Z", "2026-11-18T04:00:00Z", want{invalid: "from must be before to"}},
		{"default to is before an explicit from", "2026-11-19T00:00:00Z", "", want{invalid: "from must be before to"}},
		{"bad from", "soon", "", want{invalid: "from must be an RFC 3339 timestamp"}},
		{"bad to", "", "later", want{invalid: "to must be an RFC 3339 timestamp"}},
		{"a date only", "2026-11-18", "", want{invalid: "from must be an RFC 3339 timestamp"}},
		{"a unix timestamp", "1763467200", "", want{invalid: "from must be an RFC 3339 timestamp"}},
	} {
		from, to, err := operationsWindow(tc.from, tc.to, now)
		if tc.want.invalid != "" {
			var ae *apperror.Error
			if !errors.As(err, &ae) || ae.Status != http.StatusBadRequest || ae.Code != "validation_failed" || ae.Message != tc.want.invalid {
				t.Errorf("%s: err = %v, want 400 %q", tc.name, err, tc.want.invalid)
			}
			continue
		}
		if err != nil || !from.Equal(tc.want.from) || !to.Equal(tc.want.to) || from.Location() != time.UTC || to.Location() != time.UTC {
			t.Errorf("%s: got %v %v %v, want %v %v", tc.name, from, to, err, tc.want.from, tc.want.to)
		}
	}
}

func TestOperationsDeadlineBecomesASafeServiceUnavailableError(t *testing.T) {
	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	for name, err := range map[string]error{
		"a deadline error":   context.DeadlineExceeded,
		"a wrapped deadline": errors.Join(errors.New("read queue health"), context.DeadlineExceeded),
	} {
		if got := operationsError(context.Background(), err); !errors.Is(got, ErrOperationsTimeout) {
			t.Errorf("%s: %v", name, got)
		}
	}
	// Any error raised after the deadline passed is reported as the timeout, not as a database error.
	if got := operationsError(expired, errors.New("canceling statement due to user request (SQLSTATE 57014)")); !errors.Is(got, ErrOperationsTimeout) {
		t.Errorf("a query cancelled by the deadline: %v", got)
	}
	if ErrOperationsTimeout.Status != http.StatusServiceUnavailable || ErrOperationsTimeout.Code != "read_model_timeout" {
		t.Fatalf("%+v", ErrOperationsTimeout)
	}
	if other := errors.New("boom"); operationsError(context.Background(), other) != other || operationsError(context.Background(), nil) != nil {
		t.Fatal("other errors, and success, pass through unchanged")
	}
}
