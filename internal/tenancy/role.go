// Package tenancy implements organizations (tenants), memberships, invitations
// and the role-based access control policy.
package tenancy

import (
	"errors"
	"fmt"
	"strings"
)

// Role is an organization-scoped role. Roles belong to memberships, never to users.
type Role string

// The four initial roles.
const (
	RoleOwner     Role = "OWNER"
	RoleAdmin     Role = "ADMIN"
	RoleDeveloper Role = "DEVELOPER"
	RoleViewer    Role = "VIEWER"
)

// ErrInvalidRole is returned for unknown role names.
var ErrInvalidRole = errors.New("invalid role")

// ParseRole parses an exact, upper-case role name.
func ParseRole(s string) (Role, error) {
	switch r := Role(strings.TrimSpace(s)); r {
	case RoleOwner, RoleAdmin, RoleDeveloper, RoleViewer:
		return r, nil
	default:
		return "", fmt.Errorf("%w: %q", ErrInvalidRole, s)
	}
}

// Permission is a single action a role may be allowed to perform.
type Permission string

// Permissions checked by the application layer. Handlers and services check
// permissions, never role names.
const (
	PermOrganizationView   Permission = "organization:view"
	PermOrganizationUpdate Permission = "organization:update"
	PermMembersList        Permission = "members:list"
	PermMembersInvite      Permission = "members:invite"
	PermInvitationsList    Permission = "invitations:list"
	PermInvitationsRevoke  Permission = "invitations:revoke"
	PermMembersChangeRole  Permission = "members:change_role"
	PermMembersRemove      Permission = "members:remove"
	PermOwnershipTransfer  Permission = "ownership:transfer"
	PermAuditView          Permission = "audit:view"
)

// permissions is the authorization matrix. PermMembersChangeRole and
// PermMembersRemove are "limited" for ADMIN: holding the permission is only the
// first gate, CanChangeRole and CanRemove decide per target.
var permissions = map[Role]map[Permission]bool{
	RoleOwner: {
		PermOrganizationView: true, PermOrganizationUpdate: true, PermMembersList: true,
		PermMembersInvite: true, PermInvitationsList: true, PermInvitationsRevoke: true,
		PermMembersChangeRole: true, PermMembersRemove: true, PermOwnershipTransfer: true, PermAuditView: true,
	},
	RoleAdmin: {
		PermOrganizationView: true, PermOrganizationUpdate: true, PermMembersList: true,
		PermMembersInvite: true, PermInvitationsList: true, PermInvitationsRevoke: true,
		PermMembersChangeRole: true, PermMembersRemove: true, PermAuditView: true,
	},
	RoleDeveloper: {PermOrganizationView: true, PermMembersList: true},
	RoleViewer:    {PermOrganizationView: true, PermMembersList: true},
}

// Can reports whether role holds permission.
func Can(role Role, permission Permission) bool {
	return permissions[role][permission]
}

// manageable reports whether role is one an ADMIN may manage: DEVELOPER or VIEWER.
func manageable(role Role) bool {
	return role == RoleDeveloper || role == RoleViewer
}

// CanInviteRole reports whether actor may invite someone with the given role.
//
// OWNER may invite ADMIN, DEVELOPER or VIEWER. ADMIN may invite DEVELOPER or
// VIEWER only. Nobody invites an OWNER; ownership moves by transfer.
func CanInviteRole(actor, invited Role) bool {
	if !Can(actor, PermMembersInvite) {
		return false
	}
	switch actor {
	case RoleOwner:
		return invited == RoleAdmin || manageable(invited)
	case RoleAdmin:
		return manageable(invited)
	default:
		return false
	}
}

// CanChangeRole reports whether actor may change a member currently holding
// targetCurrent to newRole.
//
// "Limited" for ADMIN means: an ADMIN may only change members who are currently
// DEVELOPER or VIEWER, and only between DEVELOPER and VIEWER. An ADMIN can never
// touch another ADMIN or the OWNER, and can never create an ADMIN or OWNER.
// OWNER may set ADMIN, DEVELOPER or VIEWER on any non-owner member. Nobody
// assigns or removes OWNER here; that is the ownership transfer operation.
func CanChangeRole(actor, targetCurrent, newRole Role) bool {
	if !Can(actor, PermMembersChangeRole) || targetCurrent == RoleOwner || newRole == RoleOwner {
		return false
	}
	switch actor {
	case RoleOwner:
		return true
	case RoleAdmin:
		return manageable(targetCurrent) && manageable(newRole)
	default:
		return false
	}
}

// CanRemove reports whether actor may remove a member currently holding target.
//
// OWNER may remove any non-owner. ADMIN may remove only DEVELOPER and VIEWER
// members. The OWNER can never be removed through this operation.
func CanRemove(actor, target Role) bool {
	if !Can(actor, PermMembersRemove) || target == RoleOwner {
		return false
	}
	switch actor {
	case RoleOwner:
		return true
	case RoleAdmin:
		return manageable(target)
	default:
		return false
	}
}
