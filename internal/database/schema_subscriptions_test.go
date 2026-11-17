package database_test

import (
	"context"
	"testing"
)

func TestSubscriptionsSchemaConstraints(t *testing.T) {
	f := newDeliveryFixture(t)
	q, _ := f.env.Pool.Querier()
	ctx := context.Background()
	add := func(org, ep, dest, user string) error {
		_, err := q.Exec(ctx, "INSERT INTO subscriptions (organization_id, ingestion_endpoint_id, destination_id, created_by_user_id) VALUES ($1, $2, $3, $4)", org, ep, dest, user)
		return err
	}

	if err := add(f.orgID, f.ep1, f.dest1, f.userID); err != nil {
		t.Fatalf("valid subscription rejected: %v", err)
	}
	if err := add(f.orgID, f.ep1, f.dest1, f.userID); err == nil {
		t.Error("a duplicate endpoint/destination pair was accepted")
	}
	if err := add(f.orgID, f.ep1, f.dest2, f.userID); err == nil {
		t.Error("a destination of another organization was accepted (tenant isolation)")
	}
	if err := add(f.org2, f.ep1, f.dest2, f.user2); err == nil {
		t.Error("an endpoint of another organization was accepted (tenant isolation)")
	}
	if _, err := q.Exec(ctx, "UPDATE subscriptions SET status = 'REVOKED'"); err == nil {
		t.Error("REVOKED without revoked_at was accepted")
	}
	if _, err := q.Exec(ctx, "UPDATE subscriptions SET status = 'PAUSED'"); err == nil {
		t.Error("an unknown status was accepted")
	}
	if _, err := q.Exec(ctx, "UPDATE subscriptions SET status = 'REVOKED', revoked_at = now()"); err != nil {
		t.Errorf("a proper revocation failed: %v", err)
	}
	if _, err := q.Exec(ctx, "DELETE FROM subscriptions"); err == nil {
		t.Error("subscriptions must never be deleted")
	}
	if _, err := q.Exec(ctx, "TRUNCATE subscriptions"); err == nil {
		t.Error("subscriptions must never be truncated")
	}
}

func TestRollbackOfSubscriptionsMigrationAndReapply(t *testing.T) {
	f := newDeliveryFixture(t)
	ctx := context.Background()
	db, err := f.env.Pool.SQLDB()
	if err != nil {
		t.Fatal(err)
	}
	provider := newGooseProvider(t, db)
	has := func(table string) bool {
		return f.env.Count(t, "SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public' AND table_name = $1", table) == 1
	}
	if !has("subscriptions") {
		t.Fatal("subscriptions must exist after migrating up")
	}
	if _, err := provider.DownTo(ctx, 14); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if has("subscriptions") {
		t.Error("subscriptions must be gone after the rollback")
	}
	if f.env.Count(t, "SELECT count(*) FROM pg_indexes WHERE indexname = 'webhook_events_received_at_idx'") != 0 {
		t.Error("the received_at index must be dropped by the rollback")
	}
	if !has("deliveries") || !has("destinations") || !has("outbox") {
		t.Error("the rollback touched earlier tables")
	}
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("re-apply: %v", err)
	}
	if !has("subscriptions") {
		t.Error("subscriptions must exist after re-applying")
	}
	if results, err := provider.Up(ctx); err != nil || len(results) != 0 {
		t.Errorf("a second up must be a no-op: %d %v", len(results), err)
	}
}
