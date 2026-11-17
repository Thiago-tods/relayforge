package delivery

import (
	"testing"
	"time"
)

var fixedNow = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func mid() float64 { return 0.5 } // zero jitter: 1 + J*(2*0.5-1) = 1

func TestOutcomeFromHTTPStatus(t *testing.T) {
	cases := map[int]Outcome{
		200: OutcomeSuccess, 201: OutcomeSuccess, 204: OutcomeSuccess, 299: OutcomeSuccess,
		300: OutcomeRedirect, 301: OutcomeRedirect, 302: OutcomeRedirect, 307: OutcomeRedirect, 308: OutcomeRedirect, 399: OutcomeRedirect,
		400: OutcomeHTTPError, 401: OutcomeHTTPError, 404: OutcomeHTTPError, 408: OutcomeHTTPError, 429: OutcomeHTTPError,
		500: OutcomeHTTPError, 502: OutcomeHTTPError, 503: OutcomeHTTPError, 599: OutcomeHTTPError,
		// Not a valid final HTTP status: treated as a transport anomaly.
		0: OutcomeNetworkError, 99: OutcomeNetworkError, 100: OutcomeNetworkError, 199: OutcomeNetworkError, 600: OutcomeNetworkError, -1: OutcomeNetworkError,
	}
	for code, want := range cases {
		if got := OutcomeFromHTTPStatus(code); got != want {
			t.Errorf("OutcomeFromHTTPStatus(%d) = %s, want %s", code, got, want)
		}
	}
}

func TestRetryableResults(t *testing.T) {
	cases := []struct {
		name string
		res  AttemptResult
		want bool
	}{
		{"success", AttemptResult{Outcome: OutcomeSuccess, HTTPStatus: 200}, false},
		{"408", AttemptResult{Outcome: OutcomeHTTPError, HTTPStatus: 408}, true},
		{"429", AttemptResult{Outcome: OutcomeHTTPError, HTTPStatus: 429}, true},
		{"500", AttemptResult{Outcome: OutcomeHTTPError, HTTPStatus: 500}, true},
		{"503", AttemptResult{Outcome: OutcomeHTTPError, HTTPStatus: 503}, true},
		{"599", AttemptResult{Outcome: OutcomeHTTPError, HTTPStatus: 599}, true},
		{"400", AttemptResult{Outcome: OutcomeHTTPError, HTTPStatus: 400}, false},
		{"401", AttemptResult{Outcome: OutcomeHTTPError, HTTPStatus: 401}, false},
		{"403", AttemptResult{Outcome: OutcomeHTTPError, HTTPStatus: 403}, false},
		{"404", AttemptResult{Outcome: OutcomeHTTPError, HTTPStatus: 404}, false},
		{"410", AttemptResult{Outcome: OutcomeHTTPError, HTTPStatus: 410}, false},
		{"redirects are never followed", AttemptResult{Outcome: OutcomeRedirect, HTTPStatus: 302}, false},
		{"network error", AttemptResult{Outcome: OutcomeNetworkError}, true},
		{"timeout", AttemptResult{Outcome: OutcomeTimeout}, true},
		{"abandoned attempt (lease expired)", AttemptResult{Outcome: OutcomeAbandoned}, true},
		{"blocked by egress policy", AttemptResult{Outcome: OutcomeBlockedByPolicy}, false},
		{"unknown outcome", AttemptResult{Outcome: "WEIRD"}, false},
	}
	for _, c := range cases {
		if got := c.res.Retryable(); got != c.want {
			t.Errorf("%s: Retryable() = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestDefaultRetryPolicyIsValidAndDocumented(t *testing.T) {
	p := DefaultRetryPolicy()
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	if p.Base != 30*time.Second || p.Factor != 2 || p.Cap != 6*time.Hour || p.Jitter != 0.2 || p.MaxAttempts != 8 {
		t.Fatalf("defaults changed (update ADR 0025 too): %+v", p)
	}
}

func TestRetryPolicyValidation(t *testing.T) {
	good := DefaultRetryPolicy()
	bad := map[string]func(*RetryPolicy){
		"zero base":        func(p *RetryPolicy) { p.Base = 0 },
		"negative base":    func(p *RetryPolicy) { p.Base = -time.Second },
		"cap below base":   func(p *RetryPolicy) { p.Cap = p.Base - 1 },
		"factor below one": func(p *RetryPolicy) { p.Factor = 0.5 },
		"huge factor":      func(p *RetryPolicy) { p.Factor = 1000 },
		"negative jitter":  func(p *RetryPolicy) { p.Jitter = -0.1 },
		"jitter of one":    func(p *RetryPolicy) { p.Jitter = 1 },
		"zero attempts":    func(p *RetryPolicy) { p.MaxAttempts = 0 },
		"too many":         func(p *RetryPolicy) { p.MaxAttempts = 1000 },
		"huge cap":         func(p *RetryPolicy) { p.Cap = 30 * 24 * time.Hour },
	}
	for name, mutate := range bad {
		p := good
		mutate(&p)
		if p.Validate() == nil {
			t.Errorf("%s: policy accepted", name)
		}
	}
}

func TestBackoffGrowsExponentiallyThenCaps(t *testing.T) {
	p := DefaultRetryPolicy()
	want := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute,
		32 * time.Minute, 64 * time.Minute, 128 * time.Minute, 256 * time.Minute, 6 * time.Hour, 6 * time.Hour}
	for i, w := range want {
		if got := p.Backoff(i+1, mid); got != w {
			t.Errorf("Backoff(%d) = %v, want %v", i+1, got, w)
		}
	}
}

func TestBackoffIsBoundedEvenForAbsurdAttemptNumbers(t *testing.T) {
	p := DefaultRetryPolicy()
	for _, attempt := range []int{-5, 0, 1, 50, 64, 1000, 1 << 30, int(^uint(0) >> 1)} {
		for _, r := range []float64{0, 0.25, 0.5, 0.75, 0.999999999} {
			d := p.Backoff(attempt, func() float64 { return r })
			if d <= 0 || d > p.Cap {
				t.Errorf("Backoff(%d) with r=%v = %v: must be in (0, %v]", attempt, r, d, p.Cap)
			}
		}
	}
	// Attempts below one behave like the first attempt.
	if p.Backoff(0, mid) != p.Backoff(1, mid) || p.Backoff(-3, mid) != p.Backoff(1, mid) {
		t.Error("attempts below 1 must be treated as 1")
	}
}

func TestBackoffJitterStaysWithinTheConfiguredBand(t *testing.T) {
	p := DefaultRetryPolicy()
	for attempt := 1; attempt <= 6; attempt++ {
		raw := p.Backoff(attempt, mid)
		lo := time.Duration(float64(raw) * (1 - p.Jitter))
		hi := time.Duration(float64(raw) * (1 + p.Jitter))
		if got := p.Backoff(attempt, func() float64 { return 0 }); got != lo {
			t.Errorf("attempt %d lower edge = %v, want %v", attempt, got, lo)
		}
		if got := p.Backoff(attempt, func() float64 { return 0.999999999 }); got > hi || got < raw {
			t.Errorf("attempt %d upper edge = %v, want within (%v, %v]", attempt, got, raw, hi)
		}
	}
	// At the cap, jitter may only shorten the delay, never exceed the cap.
	for _, r := range []float64{0, 0.5, 0.999999999} {
		if d := p.Backoff(30, func() float64 { return r }); d > p.Cap {
			t.Errorf("r=%v: %v exceeds the cap", r, d)
		}
	}
}

func TestBackoffWithDefaultRandomSourceStaysInBounds(t *testing.T) {
	p := DefaultRetryPolicy()
	seen := map[time.Duration]bool{}
	for range 500 {
		d := p.Backoff(3, nil) // nil selects the package random source
		if d < time.Duration(float64(2*time.Minute)*0.8) || d > time.Duration(float64(2*time.Minute)*1.2) {
			t.Fatalf("jittered delay %v outside +/-20%% of 2m", d)
		}
		seen[d] = true
	}
	if len(seen) < 100 {
		t.Fatalf("jitter produced only %d distinct delays in 500 samples", len(seen))
	}
}

func TestZeroJitterIsDeterministic(t *testing.T) {
	p := DefaultRetryPolicy()
	p.Jitter = 0
	for range 20 {
		if p.Backoff(4, nil) != 4*time.Minute {
			t.Fatal("with no jitter the delay must be exact")
		}
	}
}

func TestDecideSuccess(t *testing.T) {
	d := DefaultRetryPolicy().Decide(1, AttemptResult{Outcome: OutcomeSuccess, HTTPStatus: 204}, fixedNow, mid)
	if d.Status != StatusSucceeded || !d.NextAttemptAt.IsZero() || d.Reason != ReasonSucceeded {
		t.Fatalf("%+v", d)
	}
}

func TestDecideRetriesWithBackoffUntilTheLastAttempt(t *testing.T) {
	p := DefaultRetryPolicy()
	res := AttemptResult{Outcome: OutcomeHTTPError, HTTPStatus: 503}
	for attempt := 1; attempt < p.MaxAttempts; attempt++ {
		d := p.Decide(attempt, res, fixedNow, mid)
		if d.Status != StatusPending || d.Reason != ReasonRetry || !d.NextAttemptAt.Equal(fixedNow.Add(p.Backoff(attempt, mid))) {
			t.Fatalf("attempt %d: %+v", attempt, d)
		}
	}
	last := p.Decide(p.MaxAttempts, res, fixedNow, mid)
	if last.Status != StatusFailed || last.Reason != ReasonExhausted || !last.NextAttemptAt.IsZero() {
		t.Fatalf("the last attempt must exhaust the delivery: %+v", last)
	}
	// Defensive: an attempt number beyond the maximum can never schedule another attempt.
	if d := p.Decide(p.MaxAttempts+5, res, fixedNow, mid); d.Status != StatusFailed {
		t.Fatalf("attempt beyond the maximum: %+v", d)
	}
}

func TestDecideNonRetryableFailsImmediately(t *testing.T) {
	p := DefaultRetryPolicy()
	for _, res := range []AttemptResult{
		{Outcome: OutcomeHTTPError, HTTPStatus: 400}, {Outcome: OutcomeHTTPError, HTTPStatus: 410},
		{Outcome: OutcomeRedirect, HTTPStatus: 301}, {Outcome: OutcomeBlockedByPolicy},
	} {
		d := p.Decide(1, res, fixedNow, mid)
		if d.Status != StatusFailed || d.Reason != ReasonNonRetryable || !d.NextAttemptAt.IsZero() {
			t.Errorf("%+v: %+v", res, d)
		}
	}
}

func TestDecideRetriesTransportFailuresAndAbandonedAttempts(t *testing.T) {
	p := DefaultRetryPolicy()
	for _, o := range []Outcome{OutcomeNetworkError, OutcomeTimeout, OutcomeAbandoned} {
		d := p.Decide(2, AttemptResult{Outcome: o}, fixedNow, mid)
		if d.Status != StatusPending || d.NextAttemptAt.Sub(fixedNow) != time.Minute {
			t.Errorf("%s: %+v", o, d)
		}
	}
}

func TestRetryAfterIsHonoredButBounded(t *testing.T) {
	p := DefaultRetryPolicy()
	res := func(ra time.Duration) AttemptResult {
		return AttemptResult{Outcome: OutcomeHTTPError, HTTPStatus: 429, RetryAfter: ra}
	}
	// Backoff after attempt 1 is 30s.
	if d := p.Decide(1, res(10*time.Minute), fixedNow, mid); d.NextAttemptAt.Sub(fixedNow) != 10*time.Minute {
		t.Errorf("a longer Retry-After must be honored: %v", d.NextAttemptAt.Sub(fixedNow))
	}
	if d := p.Decide(1, res(time.Second), fixedNow, mid); d.NextAttemptAt.Sub(fixedNow) != 30*time.Second {
		t.Errorf("a shorter Retry-After must not shorten the backoff: %v", d.NextAttemptAt.Sub(fixedNow))
	}
	if d := p.Decide(1, res(240*time.Hour), fixedNow, mid); d.NextAttemptAt.Sub(fixedNow) != p.Cap {
		t.Errorf("Retry-After must be capped at %v: %v", p.Cap, d.NextAttemptAt.Sub(fixedNow))
	}
	if d := p.Decide(1, res(-time.Hour), fixedNow, mid); d.NextAttemptAt.Sub(fixedNow) != 30*time.Second {
		t.Errorf("a negative Retry-After must be ignored: %v", d.NextAttemptAt.Sub(fixedNow))
	}
}

func TestDecisionStatusAlwaysFollowsTheStateMachine(t *testing.T) {
	p := DefaultRetryPolicy()
	outcomes := []AttemptResult{
		{Outcome: OutcomeSuccess, HTTPStatus: 200}, {Outcome: OutcomeHTTPError, HTTPStatus: 500}, {Outcome: OutcomeHTTPError, HTTPStatus: 404},
		{Outcome: OutcomeRedirect, HTTPStatus: 302}, {Outcome: OutcomeNetworkError}, {Outcome: OutcomeTimeout},
		{Outcome: OutcomeBlockedByPolicy}, {Outcome: OutcomeAbandoned}, {Outcome: "WEIRD"},
	}
	for attempt := 1; attempt <= p.MaxAttempts+1; attempt++ {
		for _, res := range outcomes {
			d := p.Decide(attempt, res, fixedNow, mid)
			if err := CheckTransition(StatusInFlight, d.Status); err != nil {
				t.Errorf("attempt %d %+v decided %s, which IN_FLIGHT cannot reach: %v", attempt, res, d.Status, err)
			}
			if (d.Status == StatusPending) == d.NextAttemptAt.IsZero() {
				t.Errorf("attempt %d %+v: NextAttemptAt must be set exactly when the delivery is pending: %+v", attempt, res, d)
			}
			if d.Status == StatusPending && !d.NextAttemptAt.After(fixedNow) {
				t.Errorf("a retry must be scheduled in the future: %+v", d)
			}
		}
	}
}
