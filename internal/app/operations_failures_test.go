package app_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Thiago-tods/relayforge/internal/app"
	"github.com/Thiago-tods/relayforge/internal/app/apptest"
	"github.com/Thiago-tods/relayforge/internal/tenancy"
)

func TestFailureFeedQueueAgeAndFilteredListAuthorizeBeforeTheyValidate(t *testing.T) {
	env := apptest.New(t)
	owner := env.Register(t, "owner@example.com")
	stranger := env.Register(t, "stranger@example.com")
	ctx := context.Background()

	// Garbage in every parameter: a non-member still learns nothing but "not found".
	bad := app.FailureFeedQuery{Limit: "0", Cursor: "!!!", FailureReason: "nope", DestinationID: "x", From: "soon", To: "later"}
	if _, err := env.Svc.FailureFeed(ctx, stranger.Principal, owner.Org.ID, bad); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("feed, non-member: %v, want not found", err)
	}
	if _, err := env.Svc.FailureFeed(ctx, owner.Principal, "not-a-uuid", app.FailureFeedQuery{}); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("feed, malformed organization: %v, want not found", err)
	}
	if _, err := env.Svc.QueueAge(ctx, stranger.Principal, owner.Org.ID); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("queue age, non-member: %v, want not found", err)
	}
	if _, _, err := env.Svc.ListDeliveries(ctx, stranger.Principal, owner.Org.ID, app.DeliveryFilter{FailureReason: "nope", Status: "PENDING"}, 20, 0); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("filtered list, non-member: %v, want not found", err)
	}

	// A member gets the validation error for the same input, one parameter at a time.
	for name, q := range map[string]app.FailureFeedQuery{
		"limit 0":         {Limit: "0"},
		"limit 101":       {Limit: "101"},
		"limit text":      {Limit: "ten"},
		"cursor":          {Cursor: "!!!"},
		"reason":          {FailureReason: "nope"},
		"lower case":      {FailureReason: "retries_exhausted"},
		"destination":     {DestinationID: "x"},
		"from":            {From: "soon"},
		"to":              {To: "later"},
		"window too long": {From: "2026-01-01T00:00:00Z", To: "2026-03-01T00:00:00Z"},
		"year 0000":       {From: "0000-01-01T00:00:00Z", To: "0000-01-02T00:00:00Z"},
	} {
		if _, err := env.Svc.FailureFeed(ctx, owner.Principal, owner.Org.ID, q); err == nil || errors.Is(err, app.ErrNotFound) {
			t.Errorf("feed %s: %v, want a validation error", name, err)
		}
	}
	for name, f := range map[string]app.DeliveryFilter{
		"unknown reason":  {FailureReason: "nope"},
		"status conflict": {FailureReason: "RETRIES_EXHAUSTED", Status: "PENDING"},
		"unrecorded conf": {FailureReason: "UNRECORDED", Status: "SUCCEEDED"},
	} {
		if _, _, err := env.Svc.ListDeliveries(ctx, owner.Principal, owner.Org.ID, f, 20, 0); err == nil || errors.Is(err, app.ErrNotFound) {
			t.Errorf("list %s: %v, want a validation error", name, err)
		}
	}
	// status=FAILED together with a reason is consistent, and no reason is no filter at all.
	for _, f := range []app.DeliveryFilter{{FailureReason: "RETRIES_EXHAUSTED", Status: "FAILED"}, {FailureReason: "UNRECORDED", Status: "FAILED"}, {Status: "PENDING"}} {
		if _, _, err := env.Svc.ListDeliveries(ctx, owner.Principal, owner.Org.ID, f, 20, 0); err != nil {
			t.Errorf("list %+v: %v", f, err)
		}
	}
	// A valid, empty feed is not an error, has an empty (not null) item list and no cursor.
	got, err := env.Svc.FailureFeed(ctx, owner.Principal, owner.Org.ID, app.FailureFeedQuery{})
	if err != nil || len(got.Items) != 0 || got.NextCursor != nil || got.Items == nil {
		t.Fatalf("empty feed: %+v, %v", got, err)
	}
	if d := got.To.Sub(got.From); d != 7*24*time.Hour {
		t.Fatalf("the default window is seven days: %v", d)
	}
}

func TestFailureFeedQueueAgeAndFilteredListGiveUpAtTheirDeadline(t *testing.T) {
	env := apptest.New(t)
	u := env.Register(t, "owner@example.com")
	svc := shortDeadlineService(t, env, 300*time.Millisecond)
	ctx := context.Background()

	blockDeliveries(t, env, func() {
		for name, call := range map[string]func() error{
			"feed": func() error {
				_, err := svc.FailureFeed(ctx, u.Principal, u.Org.ID, app.FailureFeedQuery{})
				return err
			},
			"queue age": func() error { _, err := svc.QueueAge(ctx, u.Principal, u.Org.ID); return err },
			"filtered list": func() error {
				_, _, err := svc.ListDeliveries(ctx, u.Principal, u.Org.ID, app.DeliveryFilter{FailureReason: "RETRIES_EXHAUSTED"}, 20, 0)
				return err
			},
			"unrecorded list": func() error {
				_, _, err := svc.ListDeliveries(ctx, u.Principal, u.Org.ID, app.DeliveryFilter{FailureReason: "UNRECORDED"}, 20, 0)
				return err
			},
		} {
			start := time.Now()
			err := call()
			if !errors.Is(err, app.ErrOperationsTimeout) {
				t.Errorf("%s: %v, want ErrOperationsTimeout", name, err)
			}
			if took := time.Since(start); took < 250*time.Millisecond || took > 5*time.Second {
				t.Errorf("%s gave up after %v, want about the 300ms deadline", name, took)
			}
		}
	})
	// Released: the same service answers, with the connections returned to the pool.
	if _, err := svc.FailureFeed(ctx, u.Principal, u.Org.ID, app.FailureFeedQuery{}); err != nil {
		t.Fatalf("feed after the lock: %v", err)
	}
	if _, err := svc.QueueAge(ctx, u.Principal, u.Org.ID); err != nil {
		t.Fatalf("queue age after the lock: %v", err)
	}
}

// The unfiltered list is deliberately not under the operations deadline: with a 200ms deadline configured and the
// table locked for longer, it keeps waiting and then answers, while the filtered list gives up.
func TestUnfilteredDeliveryListIsNotUnderTheOperationsDeadline(t *testing.T) {
	env := apptest.New(t)
	u := env.Register(t, "owner@example.com")
	svc := shortDeadlineService(t, env, 200*time.Millisecond)

	var wg sync.WaitGroup
	var listErr, filteredErr error
	blockDeliveries(t, env, func() {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _, listErr = svc.ListDeliveries(context.Background(), u.Principal, u.Org.ID, app.DeliveryFilter{Status: "FAILED"}, 20, 0)
		}()
		go func() {
			defer wg.Done()
			_, _, filteredErr = svc.ListDeliveries(context.Background(), u.Principal, u.Org.ID, app.DeliveryFilter{FailureReason: "UNRECORDED"}, 20, 0)
		}()
		time.Sleep(900 * time.Millisecond) // well past the 200ms deadline
	})
	wg.Wait()
	if listErr != nil {
		t.Errorf("the unfiltered list keeps waiting for the lock and answers: %v", listErr)
	}
	if !errors.Is(filteredErr, app.ErrOperationsTimeout) {
		t.Errorf("the filtered list times out at the deadline: %v", filteredErr)
	}
}

func TestFailureFeedQueueAgeAndFilteredListClientCancellationIsNotATimeout(t *testing.T) {
	env := apptest.New(t)
	u := env.Register(t, "owner@example.com")
	svc := shortDeadlineService(t, env, time.Minute)
	blockDeliveries(t, env, func() {
		for name, call := range map[string]func(ctx context.Context) error{
			"feed": func(ctx context.Context) error {
				_, err := svc.FailureFeed(ctx, u.Principal, u.Org.ID, app.FailureFeedQuery{})
				return err
			},
			"queue age": func(ctx context.Context) error { _, err := svc.QueueAge(ctx, u.Principal, u.Org.ID); return err },
			"filtered list": func(ctx context.Context) error {
				_, _, err := svc.ListDeliveries(ctx, u.Principal, u.Org.ID, app.DeliveryFilter{FailureReason: "INTERNAL_ERROR"}, 20, 0)
				return err
			},
		} {
			ctx, cancel := context.WithCancel(context.Background())
			time.AfterFunc(200*time.Millisecond, cancel)
			start := time.Now()
			err := call(ctx)
			if err == nil || errors.Is(err, app.ErrOperationsTimeout) || !errors.Is(err, context.Canceled) {
				t.Errorf("%s: a client that goes away is not a timeout: %v", name, err)
			}
			if time.Since(start) > 10*time.Second {
				t.Errorf("%s: the cancelled read did not stop", name)
			}
		}
	})
}

func TestFailureFeedAndQueueAgeAreAvailableToEveryRoleThatCanReadDeliveries(t *testing.T) {
	env := apptest.New(t)
	owner := env.Register(t, "owner@example.com")
	for role, r := range map[string]tenancy.Role{"admin": tenancy.RoleAdmin, "developer": tenancy.RoleDeveloper, "viewer": tenancy.RoleViewer} {
		m := env.Register(t, strings.ToLower(role)+"@example.com")
		env.AddMember(t, owner.Principal, owner.Org.ID, m, r)
		if _, err := env.Svc.FailureFeed(context.Background(), m.Principal, owner.Org.ID, app.FailureFeedQuery{}); err != nil {
			t.Errorf("%s feed: %v", role, err)
		}
		if _, err := env.Svc.QueueAge(context.Background(), m.Principal, owner.Org.ID); err != nil {
			t.Errorf("%s queue age: %v", role, err)
		}
	}
}
