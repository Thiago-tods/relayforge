package delivery

import (
	"strings"
	"testing"
	"time"

	"github.com/Thiago-tods/relayforge/internal/egress"
)

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	cases := map[string]time.Duration{
		// delta-seconds
		"0": 0, "1": time.Second, "120": 2 * time.Minute, "3600": time.Hour, " 30 ": 30 * time.Second, "007": 7 * time.Second,
		// HTTP-date, all three formats RFC 9110 requires recipients to accept
		"Thu, 01 Jan 2026 12:05:00 GMT":    5 * time.Minute,
		"Thursday, 01-Jan-26 12:00:30 GMT": 30 * time.Second,
		"Thu Jan  1 12:01:00 2026":         time.Minute,
		// in the past: no usable hint
		"Thu, 01 Jan 2026 11:00:00 GMT": 0, "Wed, 31 Dec 2025 12:00:00 GMT": 0,
		// malformed values are ignored, never an error
		"": 0, "abc": 0, "-5": 0, "+5": 0, "5.5": 0, "1e3": 0, "0x10": 0, "5 seconds": 0, "١٢٣": 0,
		"Thu, 99 Jan 2026 12:00:00 GMT": 0, "tomorrow": 0, "2026-01-01T12:05:00Z": 0, "\x00": 0, "NaN": 0,
	}
	for in, want := range cases {
		if got := ParseRetryAfter(in, now); got != want {
			t.Errorf("ParseRetryAfter(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestParseRetryAfterIsBoundedAgainstHugeValues(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	for _, in := range []string{
		"99999999999999999999", "9223372036854775807", "4294967296", strings.Repeat("9", 5000),
		"Fri, 31 Dec 9999 23:59:59 GMT",
	} {
		got := ParseRetryAfter(in, now)
		if got < 0 || got > maxRetryAfter {
			t.Errorf("ParseRetryAfter(%.20q) = %v, want a value in [0, %v]", in, got, maxRetryAfter)
		}
	}
}

func TestResultFromResponse(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		code       int
		retryAfter string
		outcome    Outcome
		retryable  bool
		hint       time.Duration
	}{
		{200, "", OutcomeSuccess, false, 0}, {204, "", OutcomeSuccess, false, 0},
		{301, "", OutcomeRedirect, false, 0}, {302, "", OutcomeRedirect, false, 0}, {307, "", OutcomeRedirect, false, 0}, {308, "", OutcomeRedirect, false, 0},
		{400, "", OutcomeHTTPError, false, 0}, {401, "", OutcomeHTTPError, false, 0}, {403, "", OutcomeHTTPError, false, 0},
		{404, "", OutcomeHTTPError, false, 0}, {410, "", OutcomeHTTPError, false, 0}, {422, "", OutcomeHTTPError, false, 0},
		{408, "", OutcomeHTTPError, true, 0}, {425, "", OutcomeHTTPError, true, 0}, {429, "", OutcomeHTTPError, true, 0},
		{429, "45", OutcomeHTTPError, true, 45 * time.Second},
		{503, "Thu, 01 Jan 2026 12:10:00 GMT", OutcomeHTTPError, true, 10 * time.Minute},
		{500, "garbage", OutcomeHTTPError, true, 0}, {502, "", OutcomeHTTPError, true, 0}, {504, "", OutcomeHTTPError, true, 0},
		// Retry-After on a non-retryable status is carried but has no effect.
		{404, "30", OutcomeHTTPError, false, 30 * time.Second},
	}
	for _, c := range cases {
		r := ResultFromResponse(c.code, c.retryAfter, now)
		if r.Outcome != c.outcome || r.Retryable() != c.retryable || r.RetryAfter != c.hint || r.HTTPStatus != c.code {
			t.Errorf("status %d retry-after %q: %+v retryable=%v", c.code, c.retryAfter, r, r.Retryable())
		}
	}
}

func TestResultFromRequestErrorClasses(t *testing.T) {
	cases := []struct {
		class       egress.ErrorClass
		outcome     Outcome
		retryable   bool
		interrupted bool
	}{
		{egress.ClassBlockedAddress, OutcomeBlockedByPolicy, false, false},
		{egress.ClassInvalidURL, OutcomeBlockedByPolicy, false, false},
		{egress.ClassInvalidRequest, OutcomeBlockedByPolicy, false, false},
		{egress.ClassDNS, OutcomeNetworkError, true, false},
		{egress.ClassDNSTimeout, OutcomeTimeout, true, false},
		{egress.ClassConnect, OutcomeNetworkError, true, false},
		{egress.ClassConnectTimeout, OutcomeTimeout, true, false},
		{egress.ClassTimeout, OutcomeTimeout, true, false},
		{egress.ClassTLS, OutcomeNetworkError, true, false},
		{egress.ClassProtocol, OutcomeNetworkError, true, false},
		{egress.ClassCanceled, "", false, true},
		{"something_new", OutcomeNetworkError, true, false}, // unknown classes fail safe: retry, bounded by max attempts
	}
	for _, c := range cases {
		got := ResultFromRequestError(c.class)
		if got.Interrupted != c.interrupted {
			t.Errorf("%s: interrupted = %v", c.class, got.Interrupted)
			continue
		}
		if c.interrupted {
			continue
		}
		if got.Result.Outcome != c.outcome || got.Result.Retryable() != c.retryable || got.Result.HTTPStatus != 0 {
			t.Errorf("%s: %+v retryable=%v", c.class, got.Result, got.Result.Retryable())
		}
		if got.Result.ErrorClass != string(c.class) {
			t.Errorf("%s: the stored error class is %q", c.class, got.Result.ErrorClass)
		}
	}
}

func TestRetryAfterClassificationEndToEndWithTheRetryPolicy(t *testing.T) {
	p := DefaultRetryPolicy()
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	mid := func() float64 { return 0.5 }

	// A server asking for 10 minutes after the first failure (backoff would be 30 s).
	d := p.Decide(1, ResultFromResponse(503, "600", now), now, mid)
	if d.Status != StatusPending || d.NextAttemptAt.Sub(now) != 10*time.Minute {
		t.Fatalf("%+v", d)
	}
	// An HTTP-date beyond the policy cap is clamped to the cap.
	far := now.Add(100 * time.Hour).Format("Mon, 02 Jan 2006 15:04:05 GMT")
	d = p.Decide(1, ResultFromResponse(429, far, now), now, mid)
	if d.NextAttemptAt.Sub(now) != p.Cap {
		t.Fatalf("clamped delay = %v, want %v", d.NextAttemptAt.Sub(now), p.Cap)
	}
	// Malformed hints fall back to the normal backoff.
	d = p.Decide(1, ResultFromResponse(503, "soon", now), now, mid)
	if d.NextAttemptAt.Sub(now) != 30*time.Second {
		t.Fatalf("fallback delay = %v", d.NextAttemptAt.Sub(now))
	}
	// Redirects and policy blocks are final.
	for _, r := range []AttemptResult{ResultFromResponse(302, "", now), ResultFromRequestError(egress.ClassBlockedAddress).Result} {
		if d := p.Decide(1, r, now, mid); d.Status != StatusFailed || d.Reason != ReasonNonRetryable {
			t.Errorf("%+v: %+v", r, d)
		}
	}
	// A TLS failure retries but never forever.
	tls := ResultFromRequestError(egress.ClassTLS).Result
	for attempt := 1; attempt < p.MaxAttempts; attempt++ {
		if d := p.Decide(attempt, tls, now, mid); d.Status != StatusPending {
			t.Fatalf("attempt %d: %+v", attempt, d)
		}
	}
	if d := p.Decide(p.MaxAttempts, tls, now, mid); d.Status != StatusFailed || d.Reason != ReasonExhausted {
		t.Fatalf("the last attempt must exhaust: %+v", d)
	}
}
