package app_test

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Thiago-tods/relayforge/internal/app"
	"github.com/Thiago-tods/relayforge/internal/audit"
	"github.com/Thiago-tods/relayforge/internal/delivery"
)

func TestSubscriptionLifecycleThroughTheService(t *testing.T) {
	tm := newTeam(t)
	svc, org, owner := tm.env.Svc, tm.org.ID, tm.owner.Principal
	ep := tm.env.CreateEndpoint(t, tm.owner).Endpoint
	d := tm.dest(t, "D").Destination

	first, err := svc.CreateSubscription(bg, owner, org, ep.ID, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if first.Status != "ACTIVE" || first.RevokedAt != nil || first.EndpointID != ep.ID || first.DestinationID != d.ID || first.DestinationName != "D" {
		t.Fatalf("unexpected subscription: %+v", first)
	}
	if _, err := svc.CreateSubscription(bg, owner, org, ep.ID, d.ID); !errors.Is(err, app.ErrAlreadySubscribed) {
		t.Fatalf("a duplicate active subscription: %v, want ErrAlreadySubscribed", err)
	}
	got, err := svc.GetSubscription(bg, owner, org, first.ID)
	if err != nil || got.ID != first.ID {
		t.Fatal(err)
	}

	revoked, err := svc.RevokeSubscription(bg, owner, org, first.ID)
	if err != nil || revoked.Status != "REVOKED" || revoked.RevokedAt == nil {
		t.Fatalf("revoke: %+v %v", revoked, err)
	}
	if _, err := svc.RevokeSubscription(bg, owner, org, first.ID); !errors.Is(err, app.ErrSubscriptionRevoked) {
		t.Fatalf("revoking twice: %v, want ErrSubscriptionRevoked", err)
	}

	second, err := svc.CreateSubscription(bg, owner, org, ep.ID, d.ID)
	if err != nil || second.ID == first.ID {
		t.Fatalf("re-subscribing must create a new record: %v", err)
	}
	// The historical interval is preserved exactly.
	again, err := svc.GetSubscription(bg, owner, org, first.ID)
	if err != nil || again.Status != "REVOKED" || !again.CreatedAt.Equal(first.CreatedAt) || !again.RevokedAt.Equal(*revoked.RevokedAt) {
		t.Fatalf("the revoked record changed: %+v", again)
	}
	if !second.CreatedAt.After(*revoked.RevokedAt) && !second.CreatedAt.Equal(*revoked.RevokedAt) {
		t.Fatal("the new interval must not start before the old one ended")
	}

	all, total, err := svc.ListSubscriptions(bg, owner, org, app.SubscriptionFilter{}, 10, 0)
	if err != nil || total != 2 || len(all) != 2 || all[0].ID != second.ID {
		t.Fatalf("list (newest first): %d %v", total, err)
	}
	active, total, _ := svc.ListSubscriptions(bg, owner, org, app.SubscriptionFilter{Status: "ACTIVE"}, 10, 0)
	byDest, totalDest, _ := svc.ListSubscriptions(bg, owner, org, app.SubscriptionFilter{DestinationID: d.ID, EndpointID: ep.ID}, 10, 0)
	if total != 1 || active[0].ID != second.ID || totalDest != 2 || len(byDest) != 2 {
		t.Fatalf("filters: active=%d byDestination=%d", total, totalDest)
	}
	for _, f := range []app.SubscriptionFilter{{Status: "PAUSED"}, {EndpointID: "nope"}, {DestinationID: "nope"}} {
		if _, _, err := svc.ListSubscriptions(bg, owner, org, f, 10, 0); !isValidation(err) {
			t.Errorf("filter %+v: %v, want a validation error", f, err)
		}
	}

	for action, want := range map[string]int{audit.SubscriptionCreated: 2, audit.SubscriptionRevoked: 1} {
		if got := tm.audits(t, action); got != want {
			t.Errorf("audit %s: %d, want %d", action, got, want)
		}
	}
}

func TestSubscriptionPreconditions(t *testing.T) {
	tm := newTeam(t)
	svc, org, owner := tm.env.Svc, tm.org.ID, tm.owner.Principal
	ep := tm.env.CreateEndpoint(t, tm.owner).Endpoint
	d := tm.dest(t, "D").Destination
	const missing = "00000000-0000-4000-8000-000000000000"

	if _, err := svc.CreateSubscription(bg, owner, org, missing, d.ID); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("missing endpoint: %v", err)
	}
	if _, err := svc.CreateSubscription(bg, owner, org, ep.ID, missing); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("missing destination: %v", err)
	}
	for _, bad := range [][2]string{{"", d.ID}, {ep.ID, ""}, {"x", "y"}} {
		if _, err := svc.CreateSubscription(bg, owner, org, bad[0], bad[1]); !isValidation(err) {
			t.Errorf("ids %v: %v, want a validation error", bad, err)
		}
	}

	// A paused destination can be subscribed: pausing is reversible and keeps pending deliveries.
	if _, err := svc.PauseDestination(bg, owner, org, d.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateSubscription(bg, owner, org, ep.ID, d.ID); err != nil {
		t.Errorf("a paused destination must accept subscriptions: %v", err)
	}
	// Revoked ones cannot: revocation is permanent.
	d2 := tm.dest(t, "D2").Destination
	if _, err := svc.RevokeDestination(bg, owner, org, d2.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateSubscription(bg, owner, org, ep.ID, d2.ID); !errors.Is(err, app.ErrDestinationRevoked) {
		t.Errorf("revoked destination: %v", err)
	}
	ep2 := tm.env.CreateEndpoint(t, tm.owner).Endpoint
	if _, err := svc.RevokeEndpoint(bg, owner, org, ep2.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateSubscription(bg, owner, org, ep2.ID, d.ID); !errors.Is(err, app.ErrEndpointRevoked) {
		t.Errorf("revoked endpoint: %v", err)
	}
}

func TestConcurrentSubscriptionCreationCreatesOneActiveRecord(t *testing.T) {
	tm := newTeam(t)
	ep := tm.env.CreateEndpoint(t, tm.owner).Endpoint
	d := tm.dest(t, "D").Destination
	var ok, dup atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			switch _, err := tm.env.Svc.CreateSubscription(bg, tm.owner.Principal, tm.org.ID, ep.ID, d.ID); {
			case err == nil:
				ok.Add(1)
			case errors.Is(err, app.ErrAlreadySubscribed):
				dup.Add(1)
			default:
				t.Error(err)
			}
		}()
	}
	close(start)
	wg.Wait()
	if ok.Load() != 1 || dup.Load() != 9 || tm.env.Count(t, "SELECT count(*) FROM subscriptions WHERE status = 'ACTIVE'") != 1 {
		t.Fatalf("created %d, rejected %d", ok.Load(), dup.Load())
	}
	if tm.audits(t, audit.SubscriptionCreated) != 1 {
		t.Fatal("only the successful creation may be audited")
	}
}

func TestSubscribeAndRevokeRaceIsDeterministic(t *testing.T) {
	for round := 0; round < 10; round++ {
		tm := newTeam(t)
		ep := tm.env.CreateEndpoint(t, tm.owner).Endpoint
		d := tm.dest(t, "D").Destination
		first, err := tm.env.Svc.CreateSubscription(bg, tm.owner.Principal, tm.org.ID, ep.ID, d.ID)
		if err != nil {
			t.Fatal(err)
		}
		var subErr, revErr error
		var wg sync.WaitGroup
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, revErr = tm.env.Svc.RevokeSubscription(bg, tm.owner.Principal, tm.org.ID, first.ID)
		}()
		go func() {
			defer wg.Done()
			<-start
			_, subErr = tm.env.Svc.CreateSubscription(bg, tm.owner.Principal, tm.org.ID, ep.ID, d.ID)
		}()
		close(start)
		wg.Wait()
		active := tm.env.Count(t, "SELECT count(*) FROM subscriptions WHERE status = 'ACTIVE'")
		total := tm.env.Count(t, "SELECT count(*) FROM subscriptions")
		switch {
		case revErr == nil && subErr == nil && active == 1 && total == 2: // revoke first, then a new interval
		case revErr == nil && errors.Is(subErr, app.ErrAlreadySubscribed) && active == 0 && total == 1: // subscribe saw the active row
		default:
			t.Fatalf("round %d: revoke=%v subscribe=%v active=%d total=%d", round, revErr, subErr, active, total)
		}
	}
}

// The event-time rule of ADR 26 holds through the API: whether an event is
// delivered depends on the subscription interval it was accepted in, for fan-out
// and for reconciliation alike.
func TestAPISubscriptionsKeepTheEventTimeSemantics(t *testing.T) {
	tm := newTeam(t)
	svc, org, owner := tm.env.Svc, tm.org.ID, tm.owner.Principal
	epRes := tm.env.CreateEndpoint(t, tm.owner)
	ep := epRes.Endpoint
	d := tm.dest(t, "D").Destination
	n := 0
	send := func() string {
		n++
		res, err := tm.env.Send(ep.PublicID, tm.env.Signed(epRes.SigningSecret, []byte(`{"n":1}`), "k"+string(rune('a'+n))))
		if err != nil {
			t.Fatal(err)
		}
		return res.EventID
	}
	fanOut := func(event string) int64 {
		var c int64
		if err := tm.env.Pool.InTx(bg, func(tx pgx.Tx) error {
			var err error
			c, err = delivery.FanOut(bg, tx, org, event, time.Now().UTC())
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return c
	}

	before := send() // accepted before any subscription
	first, err := svc.CreateSubscription(bg, owner, org, ep.ID, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	during := send() // accepted while the first subscription is active
	if _, err := svc.RevokeSubscription(bg, owner, org, first.ID); err != nil {
		t.Fatal(err)
	}
	after := send() // accepted after revocation
	second, err := svc.CreateSubscription(bg, owner, org, ep.ID, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	renewed := send() // accepted in the second interval

	// The first subscription was revoked before any fan-out ran: "during" must still be delivered.
	for name, c := range map[string]struct {
		event string
		want  int64
	}{"before": {before, 0}, "during": {during, 1}, "after": {after, 0}, "renewed": {renewed, 1}} {
		if got := fanOut(c.event); got != c.want {
			t.Errorf("fan-out of the event accepted %s: created %d, want %d", name, got, c.want)
		}
	}
	if _, err := svc.RevokeSubscription(bg, owner, org, second.ID); err != nil {
		t.Fatal(err)
	}
	// Reconciliation applies the same rule, also after the later revocation, and creates nothing new here.
	var reconciled int64
	if err := tm.env.Pool.InTx(bg, func(tx pgx.Tx) error {
		var err error
		reconciled, err = delivery.Reconcile(bg, tx, time.Now().UTC().Add(time.Hour), 2*time.Minute, 24*time.Hour, 100)
		return err
	}); err != nil || reconciled != 0 {
		t.Fatalf("reconcile after revocations created %d (%v), want 0: everything eligible was already fanned out", reconciled, err)
	}
	for name, ev := range map[string]string{"before": before, "after": after} {
		if tm.env.Count(t, "SELECT count(*) FROM deliveries WHERE event_id = $1", ev) != 0 {
			t.Errorf("event accepted %s any active interval received a delivery", name)
		}
	}
	for name, ev := range map[string]string{"during": during, "renewed": renewed} {
		if tm.env.Count(t, "SELECT count(*) FROM deliveries WHERE event_id = $1", ev) != 1 {
			t.Errorf("event accepted %s must have exactly one delivery", name)
		}
	}
}
