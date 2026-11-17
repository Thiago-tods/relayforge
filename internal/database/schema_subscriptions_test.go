package database_test

import (
	"context"
	"strings"
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

func TestSubscriptionLifecycleSchema(t *testing.T) {
	f := newDeliveryFixture(t)
	q, _ := f.env.Pool.Querier()
	ctx := context.Background()
	count := func(sql string, args ...any) int { return f.env.Count(t, sql, args...) }

	if count("SELECT count(*) FROM pg_constraint WHERE conname = 'subscriptions_endpoint_destination_key'") != 0 {
		t.Error("the unconditional unique constraint must be replaced")
	}
	var def string
	if err := q.QueryRow(ctx, "SELECT indexdef FROM pg_indexes WHERE indexname = 'subscriptions_one_active_per_pair_idx'").Scan(&def); err != nil ||
		!strings.Contains(def, "UNIQUE") || !strings.Contains(def, "ACTIVE") {
		t.Fatalf("expected a partial unique index on active subscriptions, got %q (%v)", def, err)
	}
	for _, c := range []string{"subscriptions_interval_valid", "subscriptions_endpoint_fk", "subscriptions_destination_fk"} {
		if count("SELECT count(*) FROM pg_constraint WHERE conname = $1", c) != 1 {
			t.Errorf("constraint %s is missing", c)
		}
	}
	if count("SELECT count(*) FROM pg_indexes WHERE indexname = 'subscriptions_endpoint_interval_idx'") != 1 {
		t.Error("the event-time lookup index is missing")
	}

	add := func(status, revoked string) error {
		_, err := q.Exec(ctx, `INSERT INTO subscriptions (organization_id, ingestion_endpoint_id, destination_id, created_by_user_id, created_at, status, revoked_at)
			VALUES ($1, $2, $3, $4, now() - interval '2 hours', $5, `+revoked+`)`, f.orgID, f.ep1, f.dest1, f.userID, status)
		return err
	}
	if err := add("REVOKED", "now() - interval '1 hour'"); err != nil {
		t.Fatalf("history record rejected: %v", err)
	}
	if err := add("REVOKED", "now() - interval '90 minutes'"); err != nil {
		t.Fatalf("a second revoked record for the same pair must be allowed: %v", err)
	}
	if err := add("REVOKED", "now() - interval '3 hours'"); err == nil {
		t.Error("revoked_at before created_at was accepted")
	}
	if err := add("ACTIVE", "NULL"); err != nil {
		t.Fatalf("an active subscription after revoked history: %v", err)
	}
	if err := add("ACTIVE", "NULL"); err == nil {
		t.Error("two active subscriptions for one pair were accepted")
	}
}

func TestRollbackOfSubscriptionLifecycleMigration(t *testing.T) {
	f := newDeliveryFixture(t)
	ctx := context.Background()
	q, _ := f.env.Pool.Querier()
	db, err := f.env.Pool.SQLDB()
	if err != nil {
		t.Fatal(err)
	}
	provider := newGooseProvider(t, db)
	insert := func(status, revoked string) {
		t.Helper()
		if _, err := q.Exec(ctx, `INSERT INTO subscriptions (organization_id, ingestion_endpoint_id, destination_id, created_by_user_id, created_at, status, revoked_at)
			VALUES ($1, $2, $3, $4, now() - interval '2 hours', $5, `+revoked+`)`, f.orgID, f.ep1, f.dest1, f.userID, status); err != nil {
			t.Fatal(err)
		}
	}
	insert("REVOKED", "now() - interval '1 hour'")
	insert("ACTIVE", "NULL")

	// History is never deleted, so a pair with several records blocks the rollback.
	if _, err := provider.DownTo(ctx, 15); err == nil {
		t.Fatal("rolling back must fail while a pair has several subscription records")
	}
	if f.env.Count(t, "SELECT count(*) FROM subscriptions") != 2 {
		t.Fatal("the failed rollback changed data")
	}
	// Re-sync the provider's view after the failed attempt and continue on a clean pair.
	f.env.Exec(t, "ALTER TABLE subscriptions DISABLE TRIGGER subscriptions_never_deleted")
	f.env.Exec(t, "ALTER TABLE subscriptions DISABLE TRIGGER subscriptions_lifecycle")
	f.env.Exec(t, "DELETE FROM subscriptions WHERE status = 'REVOKED'")
	f.env.Exec(t, "ALTER TABLE subscriptions ENABLE TRIGGER subscriptions_never_deleted")
	f.env.Exec(t, "ALTER TABLE subscriptions ENABLE TRIGGER subscriptions_lifecycle")

	if _, err := provider.DownTo(ctx, 15); err != nil {
		t.Fatalf("rollback to 15: %v", err)
	}
	if f.env.Count(t, "SELECT count(*) FROM pg_constraint WHERE conname = 'subscriptions_endpoint_destination_key'") != 1 {
		t.Error("the unconditional unique constraint must be restored")
	}
	if f.env.Count(t, "SELECT count(*) FROM pg_trigger WHERE tgname = 'subscriptions_lifecycle'") != 0 ||
		f.env.Count(t, "SELECT count(*) FROM pg_proc WHERE proname = 'relayforge_enforce_subscription_lifecycle'") != 0 {
		t.Error("the lifecycle trigger and function must be dropped")
	}
	if f.env.Count(t, "SELECT count(*) FROM pg_indexes WHERE indexname IN ('subscriptions_one_active_per_pair_idx', 'subscriptions_endpoint_interval_idx')") != 0 {
		t.Error("the new indexes must be dropped")
	}
	if f.env.Count(t, "SELECT count(*) FROM information_schema.tables WHERE table_name = 'subscriptions'") != 1 {
		t.Error("migration 00015 must stay applied")
	}
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("re-apply: %v", err)
	}
	if f.env.Count(t, "SELECT count(*) FROM pg_indexes WHERE indexname = 'subscriptions_one_active_per_pair_idx'") != 1 {
		t.Error("the partial unique index must exist after re-applying")
	}
	if res, err := provider.Up(ctx); err != nil || len(res) != 0 {
		t.Errorf("a second up must be a no-op: %d %v", len(res), err)
	}
}
