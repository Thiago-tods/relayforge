package delivery_test

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Thiago-tods/relayforge/internal/delivery"
)

func (f *fixture) subscribeTx(org, destID string) (string, error) {
	var id string
	err := f.env.Pool.InTx(bg, func(tx pgx.Tx) error {
		var err error
		id, err = delivery.Subscribe(bg, tx, org, f.ep, destID, f.owner.Principal.User.ID)
		return err
	})
	return id, err
}

func (f *fixture) revokeTx(org, subID string) error {
	return f.env.Pool.InTx(bg, func(tx pgx.Tx) error { return delivery.RevokeSubscription(bg, tx, org, subID) })
}

type subRow struct {
	Status                     string
	Created, Revoked, Updated  string
	Org, Endpoint, Destination string
}

func (f *fixture) subRow(t *testing.T, id string) subRow {
	t.Helper()
	q, _ := f.env.Pool.Querier()
	var r subRow
	err := q.QueryRow(bg, `SELECT status, created_at::text, coalesce(revoked_at::text, ''), updated_at::text,
		organization_id::text, ingestion_endpoint_id::text, destination_id::text FROM subscriptions WHERE id = $1`, id).
		Scan(&r.Status, &r.Created, &r.Revoked, &r.Updated, &r.Org, &r.Endpoint, &r.Destination)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (f *fixture) activeSubs(t *testing.T, destID string) int {
	return f.env.Count(t, "SELECT count(*) FROM subscriptions WHERE ingestion_endpoint_id = $1 AND destination_id = $2 AND status = 'ACTIVE'", f.ep, destID)
}

func TestSubscriptionLifecycleKeepsHistoryAndAllowsResubscription(t *testing.T) {
	f := newFixture(t)
	org, user := f.owner.Org.ID, f.owner.Principal.User.ID
	d := f.destination(t, org, user, "D")

	first, err := f.subscribeTx(org, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.subscribeTx(org, d.ID); !errors.Is(err, delivery.ErrAlreadySubscribed) {
		t.Fatalf("a second active subscription: %v, want ErrAlreadySubscribed", err)
	}
	if err := f.revokeTx(org, first); err != nil {
		t.Fatal(err)
	}
	revoked := f.subRow(t, first)
	if revoked.Status != "REVOKED" || revoked.Revoked == "" {
		t.Fatalf("revoked row: %+v", revoked)
	}
	if err := f.revokeTx(org, first); !errors.Is(err, delivery.ErrAlreadyRevoked) {
		t.Fatalf("revoking twice: %v, want ErrAlreadyRevoked", err)
	}

	second, err := f.subscribeTx(org, d.ID)
	if err != nil {
		t.Fatalf("re-subscribing after revocation: %v", err)
	}
	if second == first {
		t.Fatal("re-subscribing must create a new record, not reactivate the old one")
	}
	if got := f.subRow(t, first); got != revoked {
		t.Fatalf("the historical record changed:\n before %+v\n after  %+v", revoked, got)
	}
	if f.activeSubs(t, d.ID) != 1 || f.env.Count(t, "SELECT count(*) FROM subscriptions WHERE destination_id = $1", d.ID) != 2 {
		t.Fatal("expected exactly one active subscription and two records in total")
	}
}

func TestConcurrentSubscribeCreatesExactlyOneActiveSubscription(t *testing.T) {
	f := newFixture(t)
	org, user := f.owner.Org.ID, f.owner.Principal.User.ID
	d := f.destination(t, org, user, "D")
	var ok, dup atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			switch _, err := f.subscribeTx(org, d.ID); {
			case err == nil:
				ok.Add(1)
			case errors.Is(err, delivery.ErrAlreadySubscribed):
				dup.Add(1)
			default:
				t.Error(err)
			}
		}()
	}
	close(start)
	wg.Wait()
	if ok.Load() != 1 || dup.Load() != 15 || f.activeSubs(t, d.ID) != 1 {
		t.Fatalf("created %d, rejected %d, active %d; want 1, 15, 1", ok.Load(), dup.Load(), f.activeSubs(t, d.ID))
	}
}

func TestConcurrentRevokeAndSubscribeIsDeterministic(t *testing.T) {
	for round := 0; round < 15; round++ {
		f := newFixture(t)
		org, user := f.owner.Org.ID, f.owner.Principal.User.ID
		d := f.destination(t, org, user, "D")
		first, err := f.subscribeTx(org, d.ID)
		if err != nil {
			t.Fatal(err)
		}
		var subErr, revErr error
		var wg sync.WaitGroup
		start := make(chan struct{})
		wg.Add(2)
		go func() { defer wg.Done(); <-start; revErr = f.revokeTx(org, first) }()
		go func() { defer wg.Done(); <-start; _, subErr = f.subscribeTx(org, d.ID) }()
		close(start)
		wg.Wait()
		if revErr != nil {
			t.Fatalf("revoke: %v", revErr)
		}
		total := f.env.Count(t, "SELECT count(*) FROM subscriptions WHERE destination_id = $1", d.ID)
		switch {
		case subErr == nil && total == 2: // revoke committed first; the new subscription is the only active one
		case errors.Is(subErr, delivery.ErrAlreadySubscribed) && total == 1: // subscribe saw the still-active row
		default:
			t.Fatalf("round %d: subscribe=%v total=%d", round, subErr, total)
		}
		if subErr == nil && f.activeSubs(t, d.ID) != 1 || subErr != nil && f.activeSubs(t, d.ID) != 0 {
			t.Fatalf("round %d: inconsistent active count %d (subscribe=%v)", round, f.activeSubs(t, d.ID), subErr)
		}
		if f.subRow(t, first).Status != "REVOKED" {
			t.Fatal("the original record must end up revoked")
		}
	}
}

func TestSubscriptionTenantIsolation(t *testing.T) {
	f := newFixture(t)
	orgA, userA := f.owner.Org.ID, f.owner.Principal.User.ID
	other := f.env.Register(t, "other@example.com")
	destB := f.destination(t, other.Org.ID, other.Principal.User.ID, "B")
	destA := f.destination(t, orgA, userA, "A")

	if _, err := f.subscribeTx(orgA, destB.ID); err == nil {
		t.Fatal("subscribing tenant A's endpoint to tenant B's destination succeeded")
	}
	if _, err := f.subscribeTx(other.Org.ID, destA.ID); err == nil {
		t.Fatal("tenant B subscribing tenant A's endpoint and destination succeeded")
	}
	id, err := f.subscribeTx(orgA, destA.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.revokeTx(other.Org.ID, id); !errors.Is(err, delivery.ErrNotFound) {
		t.Fatalf("revoking another organization's subscription: %v, want ErrNotFound", err)
	}
	if f.subRow(t, id).Status != "ACTIVE" {
		t.Fatal("a cross-tenant revoke changed the subscription")
	}
}

func TestSubscriptionHistoryIsProtectedByTheDatabase(t *testing.T) {
	f := newFixture(t)
	org, user := f.owner.Org.ID, f.owner.Principal.User.ID
	d := f.destination(t, org, user, "D")
	id, _ := f.subscribeTx(org, d.ID)
	q, _ := f.env.Pool.Querier()

	if _, err := q.Exec(bg, "UPDATE subscriptions SET created_at = created_at - interval '1 day' WHERE id = $1", id); err == nil {
		t.Error("created_at of an active subscription must be immutable")
	}
	if _, err := q.Exec(bg, "UPDATE subscriptions SET destination_id = gen_random_uuid() WHERE id = $1", id); err == nil {
		t.Error("the destination of a subscription must be immutable")
	}
	if _, err := q.Exec(bg, "UPDATE subscriptions SET status = 'REVOKED', revoked_at = created_at - interval '1 second' WHERE id = $1", id); err == nil {
		t.Error("revoked_at before created_at must be rejected")
	}
	if err := f.revokeTx(org, id); err != nil {
		t.Fatal(err)
	}
	for name, stmt := range map[string]string{
		"reactivate":      "UPDATE subscriptions SET status = 'ACTIVE', revoked_at = NULL WHERE id = $1",
		"move revoked_at": "UPDATE subscriptions SET revoked_at = revoked_at + interval '1 hour' WHERE id = $1",
		"no-op update":    "UPDATE subscriptions SET status = status WHERE id = $1",
		"delete":          "DELETE FROM subscriptions WHERE id = $1",
	} {
		if _, err := q.Exec(bg, stmt, id); err == nil {
			t.Errorf("%s: a revoked subscription is immutable evidence", name)
		}
	}
}
