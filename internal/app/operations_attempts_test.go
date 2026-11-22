package app_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Thiago-tods/relayforge/internal/app"
	"github.com/Thiago-tods/relayforge/internal/app/apptest"
	"github.com/Thiago-tods/relayforge/internal/delivery"
	"github.com/Thiago-tods/relayforge/internal/tenancy"
)

func TestAttemptFeedAuthorizesBeforeItValidates(t *testing.T) {
	env := apptest.New(t)
	owner := env.Register(t, "owner@example.com")
	stranger := env.Register(t, "stranger@example.com")
	ctx := context.Background()

	// Garbage in every parameter: a non-member still learns nothing but "not found".
	bad := app.AttemptFeedQuery{Limit: "0", Cursor: "!!!", Outcome: "nope", DeliveryID: "x", From: "soon", To: "later"}
	if _, err := env.Svc.AttemptFeed(ctx, stranger.Principal, owner.Org.ID, bad); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("non-member: %v, want not found", err)
	}
	if _, err := env.Svc.AttemptFeed(ctx, owner.Principal, "not-a-uuid", app.AttemptFeedQuery{}); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("malformed organization: %v, want not found", err)
	}
	// A member gets the validation error for the same input, one parameter at a time.
	for name, q := range map[string]app.AttemptFeedQuery{
		"limit 0":         {Limit: "0"},
		"limit 101":       {Limit: "101"},
		"limit text":      {Limit: "ten"},
		"cursor":          {Cursor: "!!!"},
		"failure cursor":  {Cursor: delivery.FailureCursor{CompletedAt: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), ID: "0192a3b4-c5d6-7e8f-9a0b-1c2d3e4f5a6b"}.Encode()},
		"outcome":         {Outcome: "nope"},
		"lower case":      {Outcome: "success"},
		"delivery":        {DeliveryID: "x"},
		"from":            {From: "soon"},
		"to":              {To: "later"},
		"window too long": {From: "2026-01-01T00:00:00Z", To: "2026-03-01T00:00:00Z"},
		"year 0000":       {From: "0000-01-01T00:00:00Z", To: "0000-01-02T00:00:00Z"},
		"from after to":   {From: "2026-02-02T00:00:00Z", To: "2026-02-01T00:00:00Z"},
	} {
		if _, err := env.Svc.AttemptFeed(ctx, owner.Principal, owner.Org.ID, q); err == nil || errors.Is(err, app.ErrNotFound) {
			t.Errorf("%s: %v, want a validation error", name, err)
		}
	}
	// Valid values pass validation: every outcome, the limit bounds and an RFC 3339 offset.
	for _, o := range []string{"SUCCESS", "HTTP_ERROR", "REDIRECT", "NETWORK_ERROR", "TIMEOUT", "BLOCKED_BY_POLICY", "ABANDONED"} {
		if _, err := env.Svc.AttemptFeed(ctx, owner.Principal, owner.Org.ID, app.AttemptFeedQuery{Outcome: o, Limit: "100"}); err != nil {
			t.Errorf("outcome %s: %v", o, err)
		}
	}
	if _, err := env.Svc.AttemptFeed(ctx, owner.Principal, owner.Org.ID, app.AttemptFeedQuery{Limit: "1", From: "2026-11-01T00:00:00-03:00", To: "2026-11-02T00:00:00-03:00"}); err != nil {
		t.Errorf("an RFC 3339 offset must be accepted: %v", err)
	}
}

func TestAttemptFeedDefaultsToTheLastTwentyFourHoursAndAllowsThirtyOneDays(t *testing.T) {
	env := apptest.New(t)
	owner := env.Register(t, "owner@example.com")
	ctx := context.Background()
	got, err := env.Svc.AttemptFeed(ctx, owner.Principal, owner.Org.ID, app.AttemptFeedQuery{})
	if err != nil || len(got.Items) != 0 || got.NextCursor != nil || got.Items == nil {
		t.Fatalf("empty feed: %+v, %v", got, err)
	}
	if d := got.To.Sub(got.From); d != 24*time.Hour {
		t.Fatalf("the default window is 24 hours: %v", d)
	}
	if !got.To.Equal(got.GeneratedAt) {
		t.Errorf("a default `to` is the service clock: to %v, generated %v", got.To, got.GeneratedAt)
	}
	got, err = env.Svc.AttemptFeed(ctx, owner.Principal, owner.Org.ID, app.AttemptFeedQuery{From: "2026-10-01T00:00:00Z", To: "2026-11-01T00:00:00Z"})
	if err != nil || got.To.Sub(got.From) != 31*24*time.Hour {
		t.Fatalf("31 days is the longest window: %v, %v", got.To.Sub(got.From), err)
	}
}

func TestAttemptFeedGivesUpAtItsDeadlineAndClientCancellationIsNotATimeout(t *testing.T) {
	env := apptest.New(t)
	u := env.Register(t, "owner@example.com")
	svc := shortDeadlineService(t, env, 300*time.Millisecond)
	ctx := context.Background()
	blockDeliveries(t, env, func() {
		start := time.Now()
		_, err := svc.AttemptFeed(ctx, u.Principal, u.Org.ID, app.AttemptFeedQuery{})
		if !errors.Is(err, app.ErrOperationsTimeout) {
			t.Errorf("%v, want ErrOperationsTimeout", err)
		}
		if took := time.Since(start); took < 250*time.Millisecond || took > 5*time.Second {
			t.Errorf("gave up after %v, want about the 300ms deadline", took)
		}
	})
	if _, err := svc.AttemptFeed(ctx, u.Principal, u.Org.ID, app.AttemptFeedQuery{}); err != nil {
		t.Fatalf("after the lock: %v", err)
	}
	long := shortDeadlineService(t, env, time.Minute)
	blockDeliveries(t, env, func() {
		cctx, cancel := context.WithCancel(ctx)
		time.AfterFunc(200*time.Millisecond, cancel)
		_, err := long.AttemptFeed(cctx, u.Principal, u.Org.ID, app.AttemptFeedQuery{})
		if err == nil || errors.Is(err, app.ErrOperationsTimeout) || !errors.Is(err, context.Canceled) {
			t.Errorf("a client that goes away is not a timeout: %v", err)
		}
	})
}

func TestAttemptFeedIsAvailableToEveryRoleThatCanReadDeliveries(t *testing.T) {
	env := apptest.New(t)
	owner := env.Register(t, "owner@example.com")
	for role, r := range map[string]tenancy.Role{"admin": tenancy.RoleAdmin, "developer": tenancy.RoleDeveloper, "viewer": tenancy.RoleViewer} {
		m := env.Register(t, strings.ToLower(role)+"@example.com")
		env.AddMember(t, owner.Principal, owner.Org.ID, m, r)
		if _, err := env.Svc.AttemptFeed(context.Background(), m.Principal, owner.Org.ID, app.AttemptFeedQuery{}); err != nil {
			t.Errorf("%s: %v", role, err)
		}
	}
}
