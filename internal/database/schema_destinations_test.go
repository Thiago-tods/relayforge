package database_test

import (
	"context"
	"fmt"
	"testing"
)

func TestDestinationLifecycleGuards(t *testing.T) {
	f := newDeliveryFixture(t)
	q, _ := f.env.Pool.Querier()
	ctx := context.Background()
	id := f.insertDestination(t, f.orgID, f.userID, "Guarded", validURL)

	reject := func(name, stmt string, args ...any) {
		t.Helper()
		if _, err := q.Exec(ctx, stmt, append([]any{id}, args...)...); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	accept := func(name, stmt string, args ...any) {
		t.Helper()
		if _, err := q.Exec(ctx, stmt, append([]any{id}, args...)...); err != nil {
			t.Errorf("%s was rejected: %v", name, err)
		}
	}
	reject("changing the organization", "UPDATE destinations SET organization_id = $2 WHERE id = $1", f.org2)
	reject("changing the creator", "UPDATE destinations SET created_by_user_id = $2 WHERE id = $1", f.user2)
	reject("changing created_at", "UPDATE destinations SET created_at = created_at - interval '1 day' WHERE id = $1")
	reject("decreasing secret_version", "UPDATE destinations SET secret_version = 0 WHERE id = $1")
	accept("renaming", "UPDATE destinations SET name = 'Renamed' WHERE id = $1")
	accept("pausing", "UPDATE destinations SET status = 'PAUSED' WHERE id = $1")
	accept("activating", "UPDATE destinations SET status = 'ACTIVE' WHERE id = $1")
	accept("rotating the secret", "UPDATE destinations SET signing_secret_ciphertext = decode(repeat('ee', 40), 'hex'), secret_version = secret_version + 1 WHERE id = $1")
	accept("revoking", "UPDATE destinations SET status = 'REVOKED', revoked_at = now() WHERE id = $1")

	reject("reactivating a revoked destination", "UPDATE destinations SET status = 'ACTIVE', revoked_at = NULL WHERE id = $1")
	reject("renaming a revoked destination", "UPDATE destinations SET name = 'Again' WHERE id = $1")
	reject("a no-op update of a revoked destination", "UPDATE destinations SET status = status WHERE id = $1")
	reject("rotating a revoked destination", "UPDATE destinations SET secret_version = secret_version + 1 WHERE id = $1")
	reject("deleting a destination", "DELETE FROM destinations WHERE id = $1")

	if f.env.Count(t, "SELECT count(*) FROM pg_indexes WHERE indexname = 'subscriptions_organization_idx'") != 1 {
		t.Error("the organization-scoped subscription index is missing")
	}
}

// TestDestinationSecretRotationGuard pins the pairing of secret material and
// secret_version enforced by the lifecycle trigger.
func TestDestinationSecretRotationGuard(t *testing.T) {
	f := newDeliveryFixture(t)
	q, _ := f.env.Pool.Querier()
	ctx := context.Background()

	const newCipher = "signing_secret_ciphertext = decode(repeat('ee', 40), 'hex')"
	cases := []struct {
		name   string
		set    string
		accept bool
	}{
		{"ciphertext changed without a version increment", newCipher, false},
		{"key version changed without a version increment", "encryption_key_version = 2", false},
		{"ciphertext and key changed without a version increment", newCipher + ", encryption_key_version = 2", false},
		{"version incremented without a secret or key change", "secret_version = secret_version + 1", false},
		{"version incremented with an unchanged ciphertext rewritten to itself", "signing_secret_ciphertext = signing_secret_ciphertext, secret_version = secret_version + 1", false},
		{"ciphertext changed with a jump of two", newCipher + ", secret_version = secret_version + 2", false},
		{"key version changed with a jump of two", "encryption_key_version = 2, secret_version = secret_version + 2", false},
		{"ciphertext changed with the version unchanged explicitly", newCipher + ", secret_version = secret_version", false},
		{"ciphertext changed while the version decreases", newCipher + ", secret_version = secret_version - 1", false},
		{"ciphertext changed with exactly one increment", newCipher + ", secret_version = secret_version + 1", true},
		{"key version changed with exactly one increment", "encryption_key_version = 2, secret_version = secret_version + 1", true},
		{"ciphertext and key changed with exactly one increment", newCipher + ", encryption_key_version = 2, secret_version = secret_version + 1", true},
	}
	for i, c := range cases {
		id := f.insertDestination(t, f.orgID, f.userID, fmt.Sprintf("Rotating %d", i), validURL)
		var before int
		if err := q.QueryRow(ctx, "SELECT secret_version FROM destinations WHERE id = $1", id).Scan(&before); err != nil {
			t.Fatal(err)
		}
		_, err := q.Exec(ctx, "UPDATE destinations SET "+c.set+" WHERE id = $1", id)
		var after int
		if e := q.QueryRow(ctx, "SELECT secret_version FROM destinations WHERE id = $1", id).Scan(&after); e != nil {
			t.Fatal(e)
		}
		switch {
		case c.accept && err != nil:
			t.Errorf("%s: rejected: %v", c.name, err)
		case c.accept && after != before+1:
			t.Errorf("%s: version %d -> %d, want +1", c.name, before, after)
		case !c.accept && err == nil:
			t.Errorf("%s: accepted", c.name)
		case !c.accept && after != before:
			t.Errorf("%s: a rejected update changed the version %d -> %d", c.name, before, after)
		}
	}

	// A revoked destination stays immutable, including a well-formed rotation.
	id := f.insertDestination(t, f.orgID, f.userID, "Revoked rotation", validURL)
	if _, err := q.Exec(ctx, "UPDATE destinations SET status = 'REVOKED', revoked_at = now() WHERE id = $1", id); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Exec(ctx, "UPDATE destinations SET "+newCipher+", secret_version = secret_version + 1 WHERE id = $1", id); err == nil {
		t.Error("rotating a revoked destination was accepted")
	}
}

func TestRollbackOfDestinationGuardsMigrationAndReapply(t *testing.T) {
	f := newDeliveryFixture(t)
	ctx := context.Background()
	db, err := f.env.Pool.SQLDB()
	if err != nil {
		t.Fatal(err)
	}
	provider := newGooseProvider(t, db)
	count := func(sql string) int { return f.env.Count(t, sql) }

	if count("SELECT count(*) FROM pg_trigger WHERE tgname = 'destinations_lifecycle'") != 1 {
		t.Fatal("the lifecycle trigger must exist after migrating up")
	}
	if _, err := provider.DownTo(ctx, 16); err != nil {
		t.Fatalf("rollback to 16: %v", err)
	}
	if count("SELECT count(*) FROM pg_trigger WHERE tgname = 'destinations_lifecycle'") != 0 ||
		count("SELECT count(*) FROM pg_proc WHERE proname = 'relayforge_enforce_destination_lifecycle'") != 0 ||
		count("SELECT count(*) FROM pg_indexes WHERE indexname = 'subscriptions_organization_idx'") != 0 {
		t.Error("the rollback must remove the trigger, its function and the index")
	}
	if count("SELECT count(*) FROM pg_trigger WHERE tgname = 'destinations_never_deleted'") != 1 || count("SELECT count(*) FROM pg_trigger WHERE tgname = 'subscriptions_lifecycle'") != 1 {
		t.Error("the rollback touched earlier migrations")
	}
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("re-apply: %v", err)
	}
	if count("SELECT count(*) FROM pg_trigger WHERE tgname = 'destinations_lifecycle'") != 1 {
		t.Error("the trigger must exist after re-applying")
	}
	if res, err := provider.Up(ctx); err != nil || len(res) != 0 {
		t.Errorf("a second up must be a no-op: %d %v", len(res), err)
	}
}
