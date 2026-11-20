package app_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Thiago-tods/relayforge/internal/app"
	"github.com/Thiago-tods/relayforge/internal/app/apptest"
)

// ListDeliveriesParsed authorizes before it lets the caller's input be parsed: for a caller who is not a member the
// parse function is never run, so malformed text cannot change the answer from "not found".
func TestListDeliveriesParsedAuthorizesBeforeItParses(t *testing.T) {
	env := apptest.New(t)
	owner := env.Register(t, "owner@example.com")
	stranger := env.Register(t, "stranger@example.com")
	ctx := context.Background()

	called := 0
	parse := func(err error) func() (app.DeliveryFilter, int, int, error) {
		return func() (app.DeliveryFilter, int, int, error) {
			called++
			return app.DeliveryFilter{FailureReason: "RETRIES_EXHAUSTED"}, 20, 0, err
		}
	}
	// A non-member and a malformed or foreign organization: not found, and parse is never called.
	for name, c := range map[string]struct {
		p   app.Principal
		org string
	}{
		"non-member":           {stranger.Principal, owner.Org.ID},
		"malformed org":        {owner.Principal, "not-a-uuid"},
		"organization of none": {owner.Principal, "00000000-0000-4000-8000-000000000000"},
	} {
		_, _, err := env.Svc.ListDeliveriesParsed(ctx, c.p, c.org, parse(app.Validation("limit must be an integer from 1 to 100")))
		if !errors.Is(err, app.ErrNotFound) {
			t.Errorf("%s: %v, want not found", name, err)
		}
	}
	if called != 0 {
		t.Fatalf("parse ran %d times for callers who are not authorized", called)
	}

	// A member: parse runs once, its error is returned as is, and a good result is listed like ListDeliveries lists it.
	want := app.Validation("limit must be an integer from 1 to 100")
	if _, _, err := env.Svc.ListDeliveriesParsed(ctx, owner.Principal, owner.Org.ID, parse(want)); !errors.Is(err, want) && !isValidation(err) {
		t.Errorf("a member gets the validation error: %v", err)
	}
	if called != 1 {
		t.Fatalf("parse ran %d times for a member, want 1", called)
	}
	items, total, err := env.Svc.ListDeliveriesParsed(ctx, owner.Principal, owner.Org.ID, parse(nil))
	if err != nil || total != 0 || len(items) != 0 {
		t.Fatalf("a valid filtered request: %v %d %v", err, total, items)
	}
	// The same checks as ListDeliveries follow the parsing: a reason with a conflicting status is still a validation error.
	_, _, err = env.Svc.ListDeliveriesParsed(ctx, owner.Principal, owner.Org.ID, func() (app.DeliveryFilter, int, int, error) {
		return app.DeliveryFilter{FailureReason: "RETRIES_EXHAUSTED", Status: "PENDING"}, 20, 0, nil
	})
	if err == nil || errors.Is(err, app.ErrNotFound) {
		t.Errorf("status conflict: %v", err)
	}
}
