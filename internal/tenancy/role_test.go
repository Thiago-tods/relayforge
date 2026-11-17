package tenancy

import (
	"errors"
	"testing"
)

var allRoles = []Role{RoleOwner, RoleAdmin, RoleDeveloper, RoleViewer}

func TestParseRole(t *testing.T) {
	for _, r := range allRoles {
		got, err := ParseRole(string(r))
		if err != nil || got != r {
			t.Errorf("ParseRole(%q) = %q, %v", r, got, err)
		}
	}
	for _, bad := range []string{"", "owner", "Admin", "SUPERUSER", "OWNER;", "ADMIN,OWNER"} {
		if _, err := ParseRole(bad); !errors.Is(err, ErrInvalidRole) {
			t.Errorf("ParseRole(%q) must fail with ErrInvalidRole, got %v", bad, err)
		}
	}
}

func TestPermissionMatrix(t *testing.T) {
	// rows follow the documented matrix; "Limited" counts as allowed at this first gate.
	matrix := map[Permission][4]bool{ // OWNER, ADMIN, DEVELOPER, VIEWER
		PermOrganizationView:   {true, true, true, true},
		PermOrganizationUpdate: {true, true, false, false},
		PermMembersList:        {true, true, true, true},
		PermMembersInvite:      {true, true, false, false},
		PermInvitationsList:    {true, true, false, false},
		PermInvitationsRevoke:  {true, true, false, false},
		PermMembersChangeRole:  {true, true, false, false},
		PermMembersRemove:      {true, true, false, false},
		PermOwnershipTransfer:  {true, false, false, false},
		PermAuditView:          {true, true, false, false},
	}
	for perm, want := range matrix {
		for i, role := range allRoles {
			if got := Can(role, perm); got != want[i] {
				t.Errorf("Can(%s, %s) = %v, want %v", role, perm, got, want[i])
			}
		}
	}
	if Can("", PermOrganizationView) || Can("ROOT", PermAuditView) || Can(RoleOwner, "nonexistent:permission") {
		t.Fatal("unknown roles and permissions must be denied")
	}
}

func TestCanChangeRole(t *testing.T) {
	type tc struct {
		actor, target, to Role
		want              bool
	}
	cases := []tc{
		// OWNER manages everyone except the owner, and never assigns OWNER.
		{RoleOwner, RoleAdmin, RoleViewer, true},
		{RoleOwner, RoleViewer, RoleAdmin, true},
		{RoleOwner, RoleDeveloper, RoleViewer, true},
		{RoleOwner, RoleViewer, RoleOwner, false},
		{RoleOwner, RoleOwner, RoleAdmin, false},
		// ADMIN is limited to DEVELOPER <-> VIEWER.
		{RoleAdmin, RoleViewer, RoleDeveloper, true},
		{RoleAdmin, RoleDeveloper, RoleViewer, true},
		{RoleAdmin, RoleViewer, RoleAdmin, false},
		{RoleAdmin, RoleDeveloper, RoleOwner, false},
		{RoleAdmin, RoleAdmin, RoleViewer, false},
		{RoleAdmin, RoleOwner, RoleViewer, false},
		{RoleAdmin, RoleOwner, RoleAdmin, false},
		// DEVELOPER and VIEWER manage nobody.
		{RoleDeveloper, RoleViewer, RoleDeveloper, false},
		{RoleViewer, RoleViewer, RoleDeveloper, false},
	}
	for _, c := range cases {
		if got := CanChangeRole(c.actor, c.target, c.to); got != c.want {
			t.Errorf("CanChangeRole(%s: %s -> %s) = %v, want %v", c.actor, c.target, c.to, got, c.want)
		}
	}
}

func TestCanRemove(t *testing.T) {
	for _, actor := range allRoles {
		for _, target := range allRoles {
			want := false
			switch actor {
			case RoleOwner:
				want = target != RoleOwner
			case RoleAdmin:
				want = target == RoleDeveloper || target == RoleViewer
			}
			if got := CanRemove(actor, target); got != want {
				t.Errorf("CanRemove(%s removes %s) = %v, want %v", actor, target, got, want)
			}
		}
	}
}

func TestCanInviteRole(t *testing.T) {
	for _, actor := range allRoles {
		for _, invited := range allRoles {
			want := false
			switch actor {
			case RoleOwner:
				want = invited != RoleOwner
			case RoleAdmin:
				want = invited == RoleDeveloper || invited == RoleViewer
			}
			if got := CanInviteRole(actor, invited); got != want {
				t.Errorf("CanInviteRole(%s invites %s) = %v, want %v", actor, invited, got, want)
			}
		}
	}
}

func TestNobodyCanEverAssignOrRemoveOwnerThroughRegularOperations(t *testing.T) {
	for _, actor := range allRoles {
		for _, other := range allRoles {
			if CanChangeRole(actor, other, RoleOwner) {
				t.Errorf("%s must not assign OWNER", actor)
			}
			if CanChangeRole(actor, RoleOwner, other) {
				t.Errorf("%s must not demote OWNER", actor)
			}
			if CanRemove(actor, RoleOwner) {
				t.Errorf("%s must not remove OWNER", actor)
			}
		}
		if CanInviteRole(actor, RoleOwner) {
			t.Errorf("%s must not invite an OWNER", actor)
		}
	}
}
