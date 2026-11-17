package app_test

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Thiago-tods/relayforge/internal/app"
	"github.com/Thiago-tods/relayforge/internal/app/apptest"
	"github.com/Thiago-tods/relayforge/internal/tenancy"
)

// team is an organization with one member per role.
type team struct {
	env       *apptest.Env
	org       tenancy.Organization
	owner     apptest.User
	admin     apptest.User
	admin2    apptest.User
	developer apptest.User
	viewer    apptest.User
}

func newTeam(t *testing.T) team {
	t.Helper()
	env := apptest.New(t)
	tm := team{env: env}
	tm.owner = env.Register(t, "owner@example.com")
	tm.org = tm.owner.Org
	tm.admin = env.Register(t, "admin@example.com")
	tm.admin2 = env.Register(t, "admin2@example.com")
	tm.developer = env.Register(t, "dev@example.com")
	tm.viewer = env.Register(t, "viewer@example.com")
	env.AddMember(t, tm.owner.Principal, tm.org.ID, tm.admin, tenancy.RoleAdmin)
	env.AddMember(t, tm.owner.Principal, tm.org.ID, tm.admin2, tenancy.RoleAdmin)
	env.AddMember(t, tm.owner.Principal, tm.org.ID, tm.developer, tenancy.RoleDeveloper)
	env.AddMember(t, tm.owner.Principal, tm.org.ID, tm.viewer, tenancy.RoleViewer)
	return tm
}

func (tm team) roleOf(t *testing.T, u apptest.User) string {
	t.Helper()
	q, err := tm.env.Pool.Querier()
	if err != nil {
		t.Fatal(err)
	}
	var role string
	err = q.QueryRow(bg, "SELECT role FROM memberships WHERE organization_id = $1 AND user_id = $2 AND status = 'ACTIVE'",
		tm.org.ID, u.Principal.User.ID).Scan(&role)
	if err != nil {
		return "NONE"
	}
	return role
}

func (tm team) owners(t *testing.T) int {
	t.Helper()
	return tm.env.Count(t, "SELECT count(*) FROM memberships WHERE organization_id = $1 AND role = 'OWNER' AND status = 'ACTIVE'", tm.org.ID)
}

func TestUsersCanBelongToMultipleOrganizations(t *testing.T) {
	env := apptest.New(t)
	a := env.Register(t, "multi@example.com")
	second, err := env.Svc.CreateOrganization(bg, a.Principal, "Second Org", "")
	if err != nil {
		t.Fatal(err)
	}
	orgs, err := env.Svc.Memberships(bg, a.Principal)
	if err != nil || len(orgs) != 2 {
		t.Fatalf("memberships = %v, %v; want 2", orgs, err)
	}
	for _, o := range orgs {
		if o.Role != tenancy.RoleOwner {
			t.Errorf("creator must be OWNER of %s, got %s", o.Name, o.Role)
		}
	}
	if second.Slug != "second-org" {
		t.Fatalf("slug = %q", second.Slug)
	}
}

func TestCreateOrganizationValidationAndSlugConflict(t *testing.T) {
	env := apptest.New(t)
	a := env.Register(t, "creator@example.com")
	if _, err := env.Svc.CreateOrganization(bg, a.Principal, "  ", ""); err == nil {
		t.Error("blank name must fail")
	}
	if _, err := env.Svc.CreateOrganization(bg, a.Principal, "Fine", "Bad Slug!"); err == nil {
		t.Error("bad slug must fail")
	}
	if _, err := env.Svc.CreateOrganization(bg, a.Principal, "One", "shared-slug"); err != nil {
		t.Fatal(err)
	}
	if _, err := env.Svc.CreateOrganization(bg, a.Principal, "Two", "shared-slug"); !errors.Is(err, app.ErrSlugTaken) {
		t.Fatalf("got %v, want ErrSlugTaken", err)
	}
	// The failed attempt must not leave an orphan organization or membership behind.
	if n := env.Count(t, "SELECT count(*) FROM organizations WHERE name = 'Two'"); n != 0 {
		t.Fatal("failed creation left an organization behind")
	}
}

func TestTenantIsolation(t *testing.T) {
	env := apptest.New(t)
	alice := env.Register(t, "alice@example.com")
	bob := env.Register(t, "bob@example.com")
	missing := "00000000-0000-4000-8000-000000000000"

	for name, fn := range map[string]func(orgID string) error{
		"get": func(id string) error { _, err := env.Svc.GetOrganization(bg, bob.Principal, id); return err },
		"update": func(id string) error {
			n := "hacked"
			_, err := env.Svc.UpdateOrganization(bg, bob.Principal, id, &n, nil)
			return err
		},
		"members":     func(id string) error { _, _, err := env.Svc.ListMembers(bg, bob.Principal, id, 10, 0); return err },
		"invitations": func(id string) error { _, _, err := env.Svc.ListInvitations(bg, bob.Principal, id, 10, 0); return err },
		"audit":       func(id string) error { _, _, err := env.Svc.ListAuditEvents(bg, bob.Principal, id, 10, 0); return err },
		"invite": func(id string) error {
			_, _, err := env.Svc.CreateInvitation(bg, bob.Principal, id, "x@example.com", tenancy.RoleViewer)
			return err
		},
		"change role": func(id string) error {
			return env.Svc.ChangeRole(bg, bob.Principal, id, alice.Principal.User.ID, tenancy.RoleViewer)
		},
		"remove":     func(id string) error { return env.Svc.RemoveMember(bg, bob.Principal, id, alice.Principal.User.ID) },
		"transfer":   func(id string) error { return env.Svc.TransferOwnership(bg, bob.Principal, id, bob.Principal.User.ID) },
		"revoke inv": func(id string) error { return env.Svc.RevokeInvitation(bg, bob.Principal, id, missing) },
	} {
		foreign := fn(alice.Org.ID)
		absent := fn(missing)
		malformed := fn("not-a-uuid")
		for label, err := range map[string]error{"foreign": foreign, "absent": absent, "malformed": malformed} {
			if !errors.Is(err, app.ErrNotFound) {
				t.Errorf("%s/%s: got %v, want ErrNotFound (no existence leak)", name, label, err)
			}
		}
	}

	// Alice's organization is untouched.
	if org, err := env.Svc.GetOrganization(bg, alice.Principal, alice.Org.ID); err != nil || org.Name == "hacked" {
		t.Fatalf("organization was modified across tenants: %+v, %v", org, err)
	}
	orgs, _ := env.Svc.Memberships(bg, bob.Principal)
	for _, o := range orgs {
		if o.ID == alice.Org.ID {
			t.Fatal("bob must not see alice's organization")
		}
	}
}

func TestRevokingAnotherTenantsInvitationIsImpossible(t *testing.T) {
	env := apptest.New(t)
	alice := env.Register(t, "alice@example.com")
	bob := env.Register(t, "bob@example.com")
	inv, _, err := env.Svc.CreateInvitation(bg, alice.Principal, alice.Org.ID, "someone@example.com", tenancy.RoleViewer)
	if err != nil {
		t.Fatal(err)
	}
	// Bob owns his own organization, so he passes authorization there, but the
	// invitation belongs to Alice's organization.
	if err := env.Svc.RevokeInvitation(bg, bob.Principal, bob.Org.ID, inv.ID); !errors.Is(err, app.ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
	if env.Count(t, "SELECT count(*) FROM organization_invitations WHERE id = $1 AND revoked_at IS NULL", inv.ID) != 1 {
		t.Fatal("invitation must be untouched")
	}
}

func TestViewPermissionsPerRole(t *testing.T) {
	tm := newTeam(t)
	for name, u := range map[string]apptest.User{"owner": tm.owner, "admin": tm.admin, "developer": tm.developer, "viewer": tm.viewer} {
		if _, err := tm.env.Svc.GetOrganization(bg, u.Principal, tm.org.ID); err != nil {
			t.Errorf("%s must view the organization: %v", name, err)
		}
		members, total, err := tm.env.Svc.ListMembers(bg, u.Principal, tm.org.ID, 50, 0)
		if err != nil || total != 5 || len(members) != 5 {
			t.Errorf("%s must list 5 members: %d %v", name, total, err)
		}
	}
}

func TestOrganizationUpdateAndAuditPermissions(t *testing.T) {
	tm := newTeam(t)
	newName := "Renamed"
	for _, u := range []apptest.User{tm.owner, tm.admin} {
		if _, err := tm.env.Svc.UpdateOrganization(bg, u.Principal, tm.org.ID, &newName, nil); err != nil {
			t.Errorf("update must be allowed: %v", err)
		}
		if _, _, err := tm.env.Svc.ListAuditEvents(bg, u.Principal, tm.org.ID, 10, 0); err != nil {
			t.Errorf("audit view must be allowed: %v", err)
		}
	}
	for _, u := range []apptest.User{tm.developer, tm.viewer} {
		if _, err := tm.env.Svc.UpdateOrganization(bg, u.Principal, tm.org.ID, &newName, nil); !errors.Is(err, app.ErrForbidden) {
			t.Errorf("update must be forbidden, got %v", err)
		}
		if _, _, err := tm.env.Svc.ListAuditEvents(bg, u.Principal, tm.org.ID, 10, 0); !errors.Is(err, app.ErrForbidden) {
			t.Errorf("audit view must be forbidden, got %v", err)
		}
	}
	if _, err := tm.env.Svc.UpdateOrganization(bg, tm.owner.Principal, tm.org.ID, nil, nil); err == nil {
		t.Error("empty update must fail validation")
	}
	taken := tm.admin.Org.Slug
	if _, err := tm.env.Svc.UpdateOrganization(bg, tm.owner.Principal, tm.org.ID, nil, &taken); !errors.Is(err, app.ErrSlugTaken) {
		t.Errorf("slug conflict: got %v", err)
	}
	// The slug conflict must not poison later operations in a fresh transaction.
	if _, err := tm.env.Svc.UpdateOrganization(bg, tm.owner.Principal, tm.org.ID, &newName, nil); err != nil {
		t.Errorf("update after conflict: %v", err)
	}
}

func TestChangeRoleMatrix(t *testing.T) {
	type tc struct {
		name   string
		actor  func(team) apptest.User
		target func(team) apptest.User
		to     tenancy.Role
		want   error // nil means allowed
	}
	cases := []tc{
		{"owner promotes viewer to admin", func(t team) apptest.User { return t.owner }, func(t team) apptest.User { return t.viewer }, tenancy.RoleAdmin, nil},
		{"owner demotes admin to viewer", func(t team) apptest.User { return t.owner }, func(t team) apptest.User { return t.admin }, tenancy.RoleViewer, nil},
		{"owner cannot assign owner", func(t team) apptest.User { return t.owner }, func(t team) apptest.User { return t.viewer }, tenancy.RoleOwner, app.ErrUseTransfer},
		{"admin moves viewer to developer", func(t team) apptest.User { return t.admin }, func(t team) apptest.User { return t.viewer }, tenancy.RoleDeveloper, nil},
		{"admin cannot promote to admin", func(t team) apptest.User { return t.admin }, func(t team) apptest.User { return t.viewer }, tenancy.RoleAdmin, app.ErrForbidden},
		{"admin cannot promote to owner", func(t team) apptest.User { return t.admin }, func(t team) apptest.User { return t.viewer }, tenancy.RoleOwner, app.ErrUseTransfer},
		{"admin cannot touch another admin", func(t team) apptest.User { return t.admin }, func(t team) apptest.User { return t.admin2 }, tenancy.RoleViewer, app.ErrForbidden},
		{"admin cannot demote owner", func(t team) apptest.User { return t.admin }, func(t team) apptest.User { return t.owner }, tenancy.RoleViewer, app.ErrUseTransfer},
		{"developer cannot change roles", func(t team) apptest.User { return t.developer }, func(t team) apptest.User { return t.viewer }, tenancy.RoleDeveloper, app.ErrForbidden},
		{"viewer cannot change roles", func(t team) apptest.User { return t.viewer }, func(t team) apptest.User { return t.developer }, tenancy.RoleViewer, app.ErrForbidden},
		{"cannot change own role", func(t team) apptest.User { return t.admin }, func(t team) apptest.User { return t.admin }, tenancy.RoleViewer, app.ErrSelfModification},
		{"owner cannot demote self", func(t team) apptest.User { return t.owner }, func(t team) apptest.User { return t.owner }, tenancy.RoleAdmin, app.ErrSelfModification},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tm := newTeam(t)
			before := tm.roleOf(t, c.target(tm))
			err := tm.env.Svc.ChangeRole(bg, c.actor(tm).Principal, tm.org.ID, c.target(tm).Principal.User.ID, c.to)
			after := tm.roleOf(t, c.target(tm))
			if c.want == nil {
				if err != nil {
					t.Fatalf("expected success, got %v", err)
				}
				if after != string(c.to) {
					t.Fatalf("role = %s, want %s", after, c.to)
				}
			} else {
				if !errors.Is(err, c.want) {
					t.Fatalf("got %v, want %v", err, c.want)
				}
				if after != before {
					t.Fatalf("rejected change altered the role: %s -> %s", before, after)
				}
			}
			if tm.owners(t) != 1 {
				t.Fatalf("owners = %d, want exactly 1", tm.owners(t))
			}
		})
	}
}

func TestChangeRoleUnknownTarget(t *testing.T) {
	tm := newTeam(t)
	outsider := tm.env.Register(t, "outsider@example.com")
	for _, id := range []string{outsider.Principal.User.ID, "00000000-0000-4000-8000-000000000000", "not-a-uuid"} {
		if err := tm.env.Svc.ChangeRole(bg, tm.owner.Principal, tm.org.ID, id, tenancy.RoleViewer); !errors.Is(err, app.ErrMemberNotFound) {
			t.Errorf("target %q: got %v, want ErrMemberNotFound", id, err)
		}
	}
}

func TestRemoveMemberMatrix(t *testing.T) {
	cases := []struct {
		name   string
		actor  func(team) apptest.User
		target func(team) apptest.User
		want   error
	}{
		{"owner removes admin", func(t team) apptest.User { return t.owner }, func(t team) apptest.User { return t.admin }, nil},
		{"owner removes viewer", func(t team) apptest.User { return t.owner }, func(t team) apptest.User { return t.viewer }, nil},
		{"admin removes developer", func(t team) apptest.User { return t.admin }, func(t team) apptest.User { return t.developer }, nil},
		{"admin cannot remove admin", func(t team) apptest.User { return t.admin }, func(t team) apptest.User { return t.admin2 }, app.ErrForbidden},
		{"admin cannot remove owner", func(t team) apptest.User { return t.admin }, func(t team) apptest.User { return t.owner }, app.ErrForbidden},
		{"developer cannot remove", func(t team) apptest.User { return t.developer }, func(t team) apptest.User { return t.viewer }, app.ErrForbidden},
		{"viewer cannot remove", func(t team) apptest.User { return t.viewer }, func(t team) apptest.User { return t.developer }, app.ErrForbidden},
		{"owner cannot remove self", func(t team) apptest.User { return t.owner }, func(t team) apptest.User { return t.owner }, app.ErrSelfModification},
		{"admin cannot remove self", func(t team) apptest.User { return t.admin }, func(t team) apptest.User { return t.admin }, app.ErrSelfModification},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tm := newTeam(t)
			target := c.target(tm)
			err := tm.env.Svc.RemoveMember(bg, c.actor(tm).Principal, tm.org.ID, target.Principal.User.ID)
			if c.want == nil && err != nil {
				t.Fatalf("expected success, got %v", err)
			}
			if c.want != nil && !errors.Is(err, c.want) {
				t.Fatalf("got %v, want %v", err, c.want)
			}
			removed := tm.roleOf(t, target) == "NONE"
			if removed != (c.want == nil) {
				t.Fatalf("removed=%v but expected success=%v", removed, c.want == nil)
			}
			if tm.owners(t) != 1 {
				t.Fatalf("owners = %d, want exactly 1", tm.owners(t))
			}
		})
	}
}

func TestRemovedMemberLosesAccessAndCanBeReinvited(t *testing.T) {
	tm := newTeam(t)
	if err := tm.env.Svc.RemoveMember(bg, tm.owner.Principal, tm.org.ID, tm.developer.Principal.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := tm.env.Svc.GetOrganization(bg, tm.developer.Principal, tm.org.ID); !errors.Is(err, app.ErrNotFound) {
		t.Fatalf("removed member must lose access, got %v", err)
	}
	tm.env.AddMember(t, tm.owner.Principal, tm.org.ID, tm.developer, tenancy.RoleViewer)
	if got := tm.roleOf(t, tm.developer); got != "VIEWER" {
		t.Fatalf("re-invited member role = %s, want VIEWER", got)
	}
}

func TestInvitationPermissionsAndRoleLimits(t *testing.T) {
	tm := newTeam(t)
	cases := []struct {
		actor apptest.User
		role  tenancy.Role
		want  error
	}{
		{tm.owner, tenancy.RoleAdmin, nil},
		{tm.owner, tenancy.RoleOwner, app.ErrForbidden},
		{tm.admin, tenancy.RoleDeveloper, nil},
		{tm.admin, tenancy.RoleAdmin, app.ErrForbidden},
		{tm.admin, tenancy.RoleOwner, app.ErrForbidden},
		{tm.developer, tenancy.RoleViewer, app.ErrForbidden},
		{tm.viewer, tenancy.RoleViewer, app.ErrForbidden},
	}
	for i, c := range cases {
		email := "invitee" + string(rune('a'+i)) + "@example.com"
		_, _, err := tm.env.Svc.CreateInvitation(bg, c.actor.Principal, tm.org.ID, email, c.role)
		if (c.want == nil) != (err == nil) || (c.want != nil && !errors.Is(err, c.want)) {
			t.Errorf("case %d (%s invites %s): got %v, want %v", i, c.actor.Email, c.role, err, c.want)
		}
	}
	if _, _, err := tm.env.Svc.CreateInvitation(bg, tm.owner.Principal, tm.org.ID, "bad", tenancy.RoleViewer); err == nil {
		t.Error("invalid email must fail")
	}
	if _, _, err := tm.env.Svc.CreateInvitation(bg, tm.owner.Principal, tm.org.ID, tm.viewer.Email, tenancy.RoleViewer); !errors.Is(err, app.ErrAlreadyMember) {
		t.Errorf("inviting an existing member: got %v", err)
	}
}

func TestInvitationLifecycle(t *testing.T) {
	env := apptest.New(t)
	owner := env.Register(t, "owner@example.com")
	invitee := env.Register(t, "invitee@example.com")

	inv, token, err := env.Svc.CreateInvitation(bg, owner.Principal, owner.Org.ID, "  Invitee@Example.com ", tenancy.RoleDeveloper)
	if err != nil {
		t.Fatal(err)
	}
	if inv.InvitedEmail != "invitee@example.com" || len(token) != 43 {
		t.Fatalf("unexpected invitation: %+v token len %d", inv, len(token))
	}
	// Only the hash is stored.
	if n := env.Count(t, "SELECT count(*) FROM organization_invitations t WHERE t::text LIKE '%' || $1 || '%'", token); n != 0 {
		t.Fatal("raw invitation token found in the database")
	}

	// Duplicate pending invitation.
	if _, _, err := env.Svc.CreateInvitation(bg, owner.Principal, owner.Org.ID, "invitee@example.com", tenancy.RoleViewer); !errors.Is(err, app.ErrInvitationExists) {
		t.Fatalf("got %v, want ErrInvitationExists", err)
	}
	items, total, err := env.Svc.ListInvitations(bg, owner.Principal, owner.Org.ID, 10, 0)
	if err != nil || total != 1 || len(items) != 1 {
		t.Fatalf("pending list: %d %v", total, err)
	}

	// Wrong recipient.
	stranger := env.Register(t, "stranger@example.com")
	if _, err := env.Svc.AcceptInvitation(bg, stranger.Principal, token); !errors.Is(err, app.ErrInvalidInvitation) || !errors.Is(err, app.ReasonInvitationRecipient) {
		t.Fatalf("wrong recipient: got %v", err)
	}

	// Success.
	org, err := env.Svc.AcceptInvitation(bg, invitee.Principal, token)
	if err != nil || org.Role != tenancy.RoleDeveloper || org.ID != owner.Org.ID {
		t.Fatalf("accept: %+v %v", org, err)
	}
	// One-time use.
	if _, err := env.Svc.AcceptInvitation(bg, invitee.Principal, token); !errors.Is(err, app.ReasonInvitationUsed) {
		t.Fatalf("second accept: got %v", err)
	}
	if _, pending, err := env.Svc.ListInvitations(bg, owner.Principal, owner.Org.ID, 10, 0); err != nil || pending != 0 {
		t.Fatalf("accepted invitation must leave the pending list, total=%d err=%v", pending, err)
	}
}

func TestInvitationRejections(t *testing.T) {
	env := apptest.New(t)
	owner := env.Register(t, "owner@example.com")
	invitee := env.Register(t, "invitee@example.com")

	t.Run("garbage and unknown tokens", func(t *testing.T) {
		for _, tok := range []string{"", "x", strings.Repeat("A", 43)} {
			if _, err := env.Svc.AcceptInvitation(bg, invitee.Principal, tok); !errors.Is(err, app.ErrInvalidInvitation) {
				t.Errorf("token %q: got %v", tok, err)
			}
		}
	})
	t.Run("expired", func(t *testing.T) {
		_, token, err := env.Svc.CreateInvitation(bg, owner.Principal, owner.Org.ID, invitee.Email, tenancy.RoleViewer)
		if err != nil {
			t.Fatal(err)
		}
		env.Clock.Advance(7*24*time.Hour + time.Minute)
		if _, err := env.Svc.AcceptInvitation(bg, invitee.Principal, token); !errors.Is(err, app.ReasonInvitationExpired) {
			t.Fatalf("got %v, want expired", err)
		}
		// An expired pending invitation must not block creating a new one.
		inv, fresh, err := env.Svc.CreateInvitation(bg, owner.Principal, owner.Org.ID, invitee.Email, tenancy.RoleViewer)
		if err != nil {
			t.Fatalf("re-inviting after expiry: %v", err)
		}
		if _, err := env.Svc.AcceptInvitation(bg, invitee.Principal, fresh); err != nil {
			t.Fatalf("fresh invitation %s: %v", inv.ID, err)
		}
	})
	t.Run("revoked", func(t *testing.T) {
		other := env.Register(t, "other@example.com")
		inv, token, err := env.Svc.CreateInvitation(bg, owner.Principal, owner.Org.ID, other.Email, tenancy.RoleViewer)
		if err != nil {
			t.Fatal(err)
		}
		if err := env.Svc.RevokeInvitation(bg, owner.Principal, owner.Org.ID, inv.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := env.Svc.AcceptInvitation(bg, other.Principal, token); !errors.Is(err, app.ReasonInvitationRevoked) {
			t.Fatalf("got %v, want revoked", err)
		}
		if err := env.Svc.RevokeInvitation(bg, owner.Principal, owner.Org.ID, inv.ID); !errors.Is(err, app.ErrNotFound) {
			t.Fatalf("revoking twice: got %v", err)
		}
	})
}

func TestRevokeInvitationPermissions(t *testing.T) {
	tm := newTeam(t)
	inv, _, err := tm.env.Svc.CreateInvitation(bg, tm.owner.Principal, tm.org.ID, "pending@example.com", tenancy.RoleViewer)
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range []apptest.User{tm.developer, tm.viewer} {
		if err := tm.env.Svc.RevokeInvitation(bg, u.Principal, tm.org.ID, inv.ID); !errors.Is(err, app.ErrForbidden) {
			t.Errorf("%s: got %v, want forbidden", u.Email, err)
		}
	}
	if err := tm.env.Svc.RevokeInvitation(bg, tm.admin.Principal, tm.org.ID, inv.ID); err != nil {
		t.Fatalf("admin must revoke: %v", err)
	}
}

func TestConcurrentInvitationAcceptanceOnlyOneWins(t *testing.T) {
	env := apptest.New(t)
	owner := env.Register(t, "owner@example.com")
	invitee := env.Register(t, "invitee@example.com")
	_, token, err := env.Svc.CreateInvitation(bg, owner.Principal, owner.Org.ID, invitee.Email, tenancy.RoleViewer)
	if err != nil {
		t.Fatal(err)
	}
	const racers = 6
	results := make([]error, racers)
	var wg sync.WaitGroup
	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, results[i] = env.Svc.AcceptInvitation(bg, invitee.Principal, token)
		}()
	}
	wg.Wait()
	wins := 0
	for _, err := range results {
		if err == nil {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("%d acceptances succeeded, want exactly 1: %v", wins, results)
	}
	if n := env.Count(t, "SELECT count(*) FROM memberships WHERE user_id = $1 AND organization_id = $2", invitee.Principal.User.ID, owner.Org.ID); n != 1 {
		t.Fatalf("memberships = %d, want 1", n)
	}
}

func TestOwnershipTransfer(t *testing.T) {
	tm := newTeam(t)

	t.Run("rejections", func(t *testing.T) {
		for name, c := range map[string]struct {
			actor  apptest.User
			target string
			want   error
		}{
			"admin cannot transfer":     {tm.admin, tm.admin2.Principal.User.ID, app.ErrForbidden},
			"developer cannot transfer": {tm.developer, tm.viewer.Principal.User.ID, app.ErrForbidden},
			"owner to self":             {tm.owner, tm.owner.Principal.User.ID, app.ErrAlreadyOwner},
			"target not a member":       {tm.owner, "00000000-0000-4000-8000-000000000000", app.ErrMemberNotFound},
			"malformed target":          {tm.owner, "nope", app.ErrMemberNotFound},
		} {
			if err := tm.env.Svc.TransferOwnership(bg, c.actor.Principal, tm.org.ID, c.target); !errors.Is(err, c.want) {
				t.Errorf("%s: got %v, want %v", name, err, c.want)
			}
		}
		if tm.owners(t) != 1 || tm.roleOf(t, tm.owner) != "OWNER" {
			t.Fatal("rejected transfers must not change ownership")
		}
	})

	if err := tm.env.Svc.TransferOwnership(bg, tm.owner.Principal, tm.org.ID, tm.developer.Principal.User.ID); err != nil {
		t.Fatal(err)
	}
	if tm.roleOf(t, tm.developer) != "OWNER" || tm.roleOf(t, tm.owner) != "ADMIN" || tm.owners(t) != 1 {
		t.Fatalf("after transfer: dev=%s old=%s owners=%d", tm.roleOf(t, tm.developer), tm.roleOf(t, tm.owner), tm.owners(t))
	}
	// The former owner lost owner-only rights.
	if err := tm.env.Svc.TransferOwnership(bg, tm.owner.Principal, tm.org.ID, tm.viewer.Principal.User.ID); !errors.Is(err, app.ErrForbidden) {
		t.Fatalf("former owner: got %v, want forbidden", err)
	}
	if n := tm.env.Count(t, "SELECT count(*) FROM audit_events WHERE organization_id = $1 AND action = 'organization.ownership.transferred'", tm.org.ID); n != 1 {
		t.Fatalf("ownership audit events = %d", n)
	}
}

func TestConcurrentOwnershipTransfersKeepExactlyOneOwner(t *testing.T) {
	tm := newTeam(t)
	targets := []apptest.User{tm.admin, tm.admin2, tm.developer, tm.viewer}

	var wg sync.WaitGroup
	results := make([]error, len(targets))
	for i, target := range targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = tm.env.Svc.TransferOwnership(bg, tm.owner.Principal, tm.org.ID, target.Principal.User.ID)
		}()
	}
	wg.Wait()

	wins := 0
	for _, err := range results {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, app.ErrForbidden):
			// Lost the race: the caller is no longer the owner.
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	if wins != 1 {
		t.Fatalf("%d transfers succeeded, want exactly 1", wins)
	}
	if tm.owners(t) != 1 {
		t.Fatalf("owners = %d, want exactly 1", tm.owners(t))
	}
	if tm.roleOf(t, tm.owner) == "OWNER" {
		t.Fatal("the original owner must have been demoted")
	}
}

func TestConcurrentRoleChangesAndTransfersNeverBreakTheOwnerInvariant(t *testing.T) {
	tm := newTeam(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = tm.env.Svc.TransferOwnership(bg, tm.owner.Principal, tm.org.ID, tm.admin.Principal.User.ID)
		}()
		go func() {
			defer wg.Done()
			_ = tm.env.Svc.ChangeRole(bg, tm.owner.Principal, tm.org.ID, tm.viewer.Principal.User.ID, tenancy.RoleDeveloper)
		}()
	}
	wg.Wait()
	if tm.owners(t) != 1 {
		t.Fatalf("owners = %d, want exactly 1", tm.owners(t))
	}
}

func TestAuditTrailAndIsolation(t *testing.T) {
	tm := newTeam(t)
	other := tm.env.Register(t, "other@example.com")

	newName := "Audited"
	if _, err := tm.env.Svc.UpdateOrganization(bg, tm.owner.Principal, tm.org.ID, &newName, nil); err != nil {
		t.Fatal(err)
	}
	inv, _, err := tm.env.Svc.CreateInvitation(bg, tm.owner.Principal, tm.org.ID, "audit-target@example.com", tenancy.RoleViewer)
	if err != nil {
		t.Fatal(err)
	}
	if err := tm.env.Svc.RevokeInvitation(bg, tm.owner.Principal, tm.org.ID, inv.ID); err != nil {
		t.Fatal(err)
	}
	if err := tm.env.Svc.ChangeRole(bg, tm.owner.Principal, tm.org.ID, tm.viewer.Principal.User.ID, tenancy.RoleDeveloper); err != nil {
		t.Fatal(err)
	}
	if err := tm.env.Svc.RemoveMember(bg, tm.owner.Principal, tm.org.ID, tm.viewer.Principal.User.ID); err != nil {
		t.Fatal(err)
	}

	events, total, err := tm.env.Svc.ListAuditEvents(bg, tm.owner.Principal, tm.org.ID, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, e := range events {
		seen[e.Action] = true
		if e.OrganizationID != tm.org.ID {
			t.Fatalf("event from another organization leaked: %+v", e)
		}
	}
	for _, want := range []string{
		"organization.created", "organization.updated", "organization.invitation.created", "organization.invitation.revoked",
		"organization.invitation.accepted", "organization.member.role_changed", "organization.member.removed",
	} {
		if !seen[want] {
			t.Errorf("missing audit action %s (have %v)", want, seen)
		}
	}
	// Pagination.
	page, total2, err := tm.env.Svc.ListAuditEvents(bg, tm.owner.Principal, tm.org.ID, 2, 1)
	if err != nil || len(page) != 2 || total2 != total {
		t.Fatalf("page: %d items, total %d vs %d, %v", len(page), total2, total, err)
	}
	// Another tenant sees none of it.
	if _, _, err := tm.env.Svc.ListAuditEvents(bg, other.Principal, tm.org.ID, 10, 0); !errors.Is(err, app.ErrNotFound) {
		t.Fatalf("cross-tenant audit read: got %v", err)
	}
}

func TestAuditMetadataNeverContainsSecrets(t *testing.T) {
	tm := newTeam(t)
	_, token, err := tm.env.Svc.CreateInvitation(bg, tm.owner.Principal, tm.org.ID, "leak-check@example.com", tenancy.RoleViewer)
	if err != nil {
		t.Fatal(err)
	}
	secrets := []string{apptest.Password, token, tm.owner.Auth.RawSessionToken, tm.owner.Auth.CSRFToken, "$argon2id$"}
	for _, s := range secrets {
		if n := tm.env.Count(t, "SELECT count(*) FROM audit_events t WHERE t::text LIKE '%' || $1 || '%'", s); n != 0 {
			t.Errorf("audit table contains a secret (%d rows)", n)
		}
	}
}

func TestAuditEventsAreAppendOnly(t *testing.T) {
	env := apptest.New(t)
	env.Register(t, "append@example.com")
	q, _ := env.Pool.Querier()
	if _, err := q.Exec(bg, "UPDATE audit_events SET action = 'tampered'"); err == nil {
		t.Fatal("UPDATE on audit_events must be rejected")
	}
	if _, err := q.Exec(bg, "DELETE FROM audit_events"); err == nil {
		t.Fatal("DELETE on audit_events must be rejected")
	}
	if env.Count(t, "SELECT count(*) FROM audit_events WHERE action = 'tampered'") != 0 || env.Count(t, "SELECT count(*) FROM audit_events") == 0 {
		t.Fatal("audit history was altered")
	}
}

func TestMutationAndAuditShareATransaction(t *testing.T) {
	tm := newTeam(t)
	// Make the audit insert fail inside the transaction: the role change must roll back with it.
	tm.env.Exec(t, "ALTER TABLE audit_events ADD CONSTRAINT audit_events_block CHECK (action <> 'organization.member.role_changed') NOT VALID")
	err := tm.env.Svc.ChangeRole(bg, tm.owner.Principal, tm.org.ID, tm.viewer.Principal.User.ID, tenancy.RoleDeveloper)
	if err == nil {
		t.Fatal("expected the audit failure to fail the operation")
	}
	if got := tm.roleOf(t, tm.viewer); got != "VIEWER" {
		t.Fatalf("role = %s: the mutation was committed without its audit event", got)
	}
}
