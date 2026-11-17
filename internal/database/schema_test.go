package database_test

import (
	"context"
	"testing"

	"github.com/Thiago-tods/relayforge/internal/app/apptest"
	"github.com/Thiago-tods/relayforge/internal/identity"
)

// schemaFixture holds one user, organization and session inserted with raw SQL
// so constraint violations can be provoked directly.
type schemaFixture struct {
	env     *apptest.Env
	userID  string
	orgID   string
	tokHash []byte
}

func newSchemaFixture(t *testing.T) schemaFixture {
	t.Helper()
	env := apptest.New(t)
	q, err := env.Pool.Querier()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	f := schemaFixture{env: env}
	if err := q.QueryRow(ctx,
		"INSERT INTO users (email, normalized_email, password_hash, display_name) VALUES ('a@x.io','a@x.io','h','A') RETURNING id").Scan(&f.userID); err != nil {
		t.Fatal(err)
	}
	if err := q.QueryRow(ctx, "INSERT INTO organizations (name, slug) VALUES ('Org','org-one') RETURNING id").Scan(&f.orgID); err != nil {
		t.Fatal(err)
	}
	_, f.tokHash = mustToken(t)
	return f
}

func mustToken(t *testing.T) (string, []byte) {
	t.Helper()
	raw, hash, err := identity.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	return raw, hash
}

// reject runs a statement that must violate a constraint. Each runs outside a
// transaction, so a failure never poisons later statements.
func (f schemaFixture) reject(t *testing.T, name, sql string, args ...any) {
	t.Helper()
	q, _ := f.env.Pool.Querier()
	if _, err := q.Exec(context.Background(), sql, args...); err == nil {
		t.Errorf("%s: statement was accepted but must be rejected", name)
	}
}

func (f schemaFixture) accept(t *testing.T, name, sql string, args ...any) {
	t.Helper()
	q, _ := f.env.Pool.Querier()
	if _, err := q.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: unexpected failure: %v", name, err)
	}
}

func TestUsersConstraints(t *testing.T) {
	f := newSchemaFixture(t)
	f.reject(t, "duplicate normalized email", "INSERT INTO users (email, normalized_email, password_hash, display_name) VALUES ('A@X.io','a@x.io','h','B')")
	f.reject(t, "uppercase normalized email", "INSERT INTO users (email, normalized_email, password_hash, display_name) VALUES ('B@X.io','B@X.io','h','B')")
	f.reject(t, "invalid status", "INSERT INTO users (email, normalized_email, password_hash, display_name, status) VALUES ('c@x.io','c@x.io','h','C','ROOT')")
	f.reject(t, "empty display name", "INSERT INTO users (email, normalized_email, password_hash, display_name) VALUES ('d@x.io','d@x.io','h','')")
	f.accept(t, "valid second user", "INSERT INTO users (email, normalized_email, password_hash, display_name) VALUES ('e@x.io','e@x.io','h','E')")
}

func TestSessionsConstraints(t *testing.T) {
	f := newSchemaFixture(t)
	f.accept(t, "valid session", "INSERT INTO sessions (user_id, token_hash, csrf_token_hash, expires_at) VALUES ($1,$2,$3, now() + interval '1 hour')",
		f.userID, f.tokHash, f.tokHash)
	f.reject(t, "duplicate token hash", "INSERT INTO sessions (user_id, token_hash, csrf_token_hash, expires_at) VALUES ($1,$2,$3, now() + interval '1 hour')",
		f.userID, f.tokHash, f.tokHash)
	_, other := mustToken(t)
	f.reject(t, "short token hash", "INSERT INTO sessions (user_id, token_hash, csrf_token_hash, expires_at) VALUES ($1,$2,$3, now() + interval '1 hour')",
		f.userID, []byte("short"), other)
	f.reject(t, "unknown user", "INSERT INTO sessions (user_id, token_hash, csrf_token_hash, expires_at) VALUES ('00000000-0000-4000-8000-000000000000',$1,$2, now() + interval '1 hour')",
		other, other)
	f.reject(t, "already expired at creation", "INSERT INTO sessions (user_id, token_hash, csrf_token_hash, expires_at) VALUES ($1,$2,$3, now() - interval '1 hour')",
		f.userID, other, other)
}

func TestOrganizationAndMembershipConstraints(t *testing.T) {
	f := newSchemaFixture(t)
	f.reject(t, "duplicate slug", "INSERT INTO organizations (name, slug) VALUES ('Other','org-one')")
	for _, slug := range []string{"Bad Slug", "-lead", "trail-", "dou--ble", "ab", "../etc", "UPPER"} {
		f.reject(t, "slug "+slug, "INSERT INTO organizations (name, slug) VALUES ('X', $1)", slug)
	}

	f.accept(t, "owner membership", "INSERT INTO memberships (organization_id, user_id, role) VALUES ($1,$2,'OWNER')", f.orgID, f.userID)
	f.reject(t, "duplicate membership", "INSERT INTO memberships (organization_id, user_id, role) VALUES ($1,$2,'VIEWER')", f.orgID, f.userID)
	f.reject(t, "unknown organization", "INSERT INTO memberships (organization_id, user_id, role) VALUES ('00000000-0000-4000-8000-000000000000',$1,'VIEWER')", f.userID)

	var second string
	q, _ := f.env.Pool.Querier()
	if err := q.QueryRow(context.Background(),
		"INSERT INTO users (email, normalized_email, password_hash, display_name) VALUES ('b@x.io','b@x.io','h','B') RETURNING id").Scan(&second); err != nil {
		t.Fatal(err)
	}
	f.reject(t, "invalid role", "INSERT INTO memberships (organization_id, user_id, role) VALUES ($1,$2,'SUPERUSER')", f.orgID, second)
	f.reject(t, "lowercase role", "INSERT INTO memberships (organization_id, user_id, role) VALUES ($1,$2,'owner')", f.orgID, second)
	f.reject(t, "invalid status", "INSERT INTO memberships (organization_id, user_id, role, status) VALUES ($1,$2,'VIEWER','GHOST')", f.orgID, second)
	f.reject(t, "second active owner", "INSERT INTO memberships (organization_id, user_id, role) VALUES ($1,$2,'OWNER')", f.orgID, second)
	f.accept(t, "viewer membership", "INSERT INTO memberships (organization_id, user_id, role) VALUES ($1,$2,'VIEWER')", f.orgID, second)
	f.reject(t, "promote to second owner", "UPDATE memberships SET role = 'OWNER' WHERE user_id = $1", second)
	// A removed owner no longer counts toward the single-owner rule.
	f.accept(t, "remove owner", "UPDATE memberships SET status = 'REMOVED' WHERE user_id = $1", f.userID)
	f.accept(t, "promote after removal", "UPDATE memberships SET role = 'OWNER' WHERE user_id = $1", second)
}

func TestInvitationConstraints(t *testing.T) {
	f := newSchemaFixture(t)
	insert := "INSERT INTO organization_invitations (organization_id, invited_email, invited_by_user_id, token_hash, role, expires_at) VALUES ($1,$2,$3,$4,$5, now() + interval '1 day')"
	f.accept(t, "valid invitation", insert, f.orgID, "i@x.io", f.userID, f.tokHash, "VIEWER")
	_, other := mustToken(t)
	f.reject(t, "duplicate token hash", insert, f.orgID, "j@x.io", f.userID, f.tokHash, "VIEWER")
	f.reject(t, "second pending for same email", insert, f.orgID, "i@x.io", f.userID, other, "VIEWER")
	f.reject(t, "owner invitation", insert, f.orgID, "k@x.io", f.userID, other, "OWNER")
	f.reject(t, "invalid role", insert, f.orgID, "k@x.io", f.userID, other, "GOD")
	f.reject(t, "uppercase email", insert, f.orgID, "K@X.io", f.userID, other, "VIEWER")
	f.reject(t, "unknown organization", insert, "00000000-0000-4000-8000-000000000000", "k@x.io", f.userID, other, "VIEWER")
	f.reject(t, "short token hash", insert, f.orgID, "k@x.io", f.userID, []byte("x"), "VIEWER")
	f.reject(t, "accepted and revoked", "UPDATE organization_invitations SET accepted_at = now(), revoked_at = now()")

	// Once the first invitation is revoked, a new pending one may exist.
	f.accept(t, "revoke", "UPDATE organization_invitations SET revoked_at = now() WHERE invited_email = 'i@x.io'")
	f.accept(t, "re-invite after revoke", insert, f.orgID, "i@x.io", f.userID, other, "VIEWER")
}

func TestAuditEventsConstraints(t *testing.T) {
	f := newSchemaFixture(t)
	f.accept(t, "valid event", "INSERT INTO audit_events (organization_id, actor_user_id, action, resource_type, metadata) VALUES ($1,$2,'x.y','thing','{\"k\":1}')", f.orgID, f.userID)
	f.accept(t, "event without organization", "INSERT INTO audit_events (action, resource_type) VALUES ('x.y','thing')")
	f.reject(t, "non-object metadata", "INSERT INTO audit_events (action, resource_type, metadata) VALUES ('x.y','thing','[1]')")
	f.reject(t, "empty action", "INSERT INTO audit_events (action, resource_type) VALUES ('','thing')")
	f.reject(t, "unknown organization", "INSERT INTO audit_events (organization_id, action, resource_type) VALUES ('00000000-0000-4000-8000-000000000000','x.y','thing')")
	f.reject(t, "update", "UPDATE audit_events SET action = 'tampered'")
	f.reject(t, "delete", "DELETE FROM audit_events")
}

func TestForeignKeysProtectParents(t *testing.T) {
	f := newSchemaFixture(t)
	f.accept(t, "membership", "INSERT INTO memberships (organization_id, user_id, role) VALUES ($1,$2,'OWNER')", f.orgID, f.userID)
	f.reject(t, "delete organization with members", "DELETE FROM organizations WHERE id = $1", f.orgID)
	f.reject(t, "delete user with memberships", "DELETE FROM users WHERE id = $1", f.userID)
	f.accept(t, "session", "INSERT INTO sessions (user_id, token_hash, csrf_token_hash, expires_at) VALUES ($1,$2,$2, now() + interval '1 hour')", f.userID, f.tokHash)
}

func TestUpdatedAtTriggerRuns(t *testing.T) {
	f := newSchemaFixture(t)
	if n := f.env.Count(t, "SELECT count(*) FROM pg_trigger WHERE tgname IN ('users_set_updated_at','organizations_set_updated_at','memberships_set_updated_at')"); n != 3 {
		t.Fatalf("updated_at triggers = %d, want 3", n)
	}
	f.accept(t, "backdate", "UPDATE organizations SET updated_at = now() - interval '1 day' WHERE id = $1", f.orgID)
	f.accept(t, "touch", "UPDATE organizations SET name = 'Renamed' WHERE id = $1", f.orgID)
	if n := f.env.Count(t, "SELECT count(*) FROM organizations WHERE id = $1 AND updated_at > now() - interval '1 minute'", f.orgID); n != 1 {
		t.Fatal("updated_at must be refreshed by relayforge_set_updated_at()")
	}
}
