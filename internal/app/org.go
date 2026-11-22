package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/Thiago-tods/relayforge/internal/audit"
	"github.com/Thiago-tods/relayforge/internal/database"
	"github.com/Thiago-tods/relayforge/internal/identity"
	"github.com/Thiago-tods/relayforge/internal/tenancy"
)

// authorize resolves the principal's ACTIVE membership in the organization and
// checks the permission. It is the single tenant-isolation gate:
//
//   - a malformed ID, a missing organization and a non-member all return ErrNotFound,
//     so callers cannot probe which organizations exist;
//   - a member without the permission gets ErrForbidden.
func authorize(ctx context.Context, db database.Querier, p Principal, orgID string, perm tenancy.Permission) (tenancy.Membership, error) {
	if !IsUUID(orgID) {
		return tenancy.Membership{}, ErrNotFound
	}
	m, err := tenancy.ActiveMembership(ctx, db, orgID, p.User.ID)
	if errors.Is(err, tenancy.ErrNotMember) {
		return tenancy.Membership{}, ErrNotFound
	}
	if err != nil {
		return tenancy.Membership{}, err
	}
	if !tenancy.Can(m.Role, perm) {
		return tenancy.Membership{}, ErrForbidden
	}
	return m, nil
}

// lockAndAuthorize locks the organization row, then authorizes. Locking first
// means the caller's role is read after any concurrent membership change has
// committed, so a demoted or removed actor cannot act on stale authority.
func lockAndAuthorize(ctx context.Context, tx pgx.Tx, p Principal, orgID string, perm tenancy.Permission) (tenancy.Membership, error) {
	if !IsUUID(orgID) {
		return tenancy.Membership{}, ErrNotFound
	}
	if err := tenancy.LockOrganization(ctx, tx, orgID); err != nil {
		if errors.Is(err, tenancy.ErrOrgNotFound) {
			return tenancy.Membership{}, ErrNotFound
		}
		return tenancy.Membership{}, err
	}
	return authorize(ctx, tx, p, orgID, perm)
}

// lockSharedAndAuthorize is lockAndAuthorize with a shared organization lock. It exists only for the
// redrive flow (a test keeps it that way). The authorization guarantee is the same: the membership is
// read after the lock is held, so a role change committed earlier is visible, and a later one cannot
// commit until this transaction ends, because it needs the exclusive lock. See tenancy.LockOrganizationShared.
func lockSharedAndAuthorize(ctx context.Context, tx pgx.Tx, p Principal, orgID string, perm tenancy.Permission) (tenancy.Membership, error) {
	if !IsUUID(orgID) {
		return tenancy.Membership{}, ErrNotFound
	}
	if err := tenancy.LockOrganizationShared(ctx, tx, orgID); err != nil {
		if errors.Is(err, tenancy.ErrOrgNotFound) {
			return tenancy.Membership{}, ErrNotFound
		}
		return tenancy.Membership{}, err
	}
	return authorize(ctx, tx, p, orgID, perm)
}

// CreateOrganization creates an organization owned by the principal.
func (s *Service) CreateOrganization(ctx context.Context, p Principal, name, slug string) (tenancy.Organization, error) {
	cleanName, err := tenancy.NormalizeName(name)
	if err != nil {
		return tenancy.Organization{}, Validation("organization name must be 1 to 100 printable characters")
	}
	explicit := strings.TrimSpace(slug) != ""
	candidate := tenancy.Slugify(cleanName)
	if explicit {
		if candidate, err = tenancy.NormalizeSlug(slug); err != nil {
			return tenancy.Organization{}, Validation("organization slug must be 3 to 48 lowercase letters, digits or single hyphens")
		}
	}

	var org tenancy.Organization
	err = s.pool.InTx(ctx, func(tx pgx.Tx) error {
		var err error
		if org, err = createOrganization(ctx, tx, cleanName, candidate, !explicit); err != nil {
			return err
		}
		if _, err = tenancy.AddMembership(ctx, tx, org.ID, p.User.ID, tenancy.RoleOwner); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Event{
			OrganizationID: org.ID, ActorUserID: p.User.ID, Action: audit.OrganizationCreated,
			ResourceType: "organization", ResourceID: org.ID, Metadata: map[string]any{"slug": org.Slug},
		})
	})
	return org, err
}

// GetOrganization returns an organization the principal is a member of.
func (s *Service) GetOrganization(ctx context.Context, p Principal, orgID string) (tenancy.OrganizationWithRole, error) {
	q := s.mustQuerier()
	m, err := authorize(ctx, q, p, orgID, tenancy.PermOrganizationView)
	if err != nil {
		return tenancy.OrganizationWithRole{}, err
	}
	org, err := tenancy.OrganizationByID(ctx, q, orgID)
	if err != nil {
		return tenancy.OrganizationWithRole{}, err
	}
	return tenancy.OrganizationWithRole{Organization: org, Role: m.Role}, nil
}

// UpdateOrganization changes the name and/or slug; nil leaves a field unchanged.
func (s *Service) UpdateOrganization(ctx context.Context, p Principal, orgID string, name, slug *string) (tenancy.Organization, error) {
	var newName, newSlug *string
	if name != nil {
		n, err := tenancy.NormalizeName(*name)
		if err != nil {
			return tenancy.Organization{}, Validation("organization name must be 1 to 100 printable characters")
		}
		newName = &n
	}
	if slug != nil {
		sl, err := tenancy.NormalizeSlug(*slug)
		if err != nil {
			return tenancy.Organization{}, Validation("organization slug must be 3 to 48 lowercase letters, digits or single hyphens")
		}
		newSlug = &sl
	}
	if newName == nil && newSlug == nil {
		return tenancy.Organization{}, Validation("provide at least one field to update")
	}

	var org tenancy.Organization
	err := s.pool.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := lockAndAuthorize(ctx, tx, p, orgID, tenancy.PermOrganizationUpdate); err != nil {
			return err
		}
		sp, err := tx.Begin(ctx) // savepoint: a slug collision must not abort the outer transaction
		if err != nil {
			return fmt.Errorf("begin savepoint: %w", err)
		}
		org, err = tenancy.UpdateOrganization(ctx, sp, orgID, newName, newSlug)
		if err != nil {
			if rbErr := sp.Rollback(ctx); rbErr != nil {
				return rbErr
			}
			if errors.Is(err, tenancy.ErrSlugTaken) {
				return ErrSlugTaken
			}
			return err
		}
		if err := sp.Commit(ctx); err != nil {
			return err
		}
		meta := map[string]any{}
		if newName != nil {
			meta["name"] = org.Name
		}
		if newSlug != nil {
			meta["slug"] = org.Slug
		}
		return audit.Record(ctx, tx, audit.Event{
			OrganizationID: orgID, ActorUserID: p.User.ID, Action: audit.OrganizationUpdated,
			ResourceType: "organization", ResourceID: orgID, Metadata: meta,
		})
	})
	return org, err
}

// ListMembers returns a page of active members.
func (s *Service) ListMembers(ctx context.Context, p Principal, orgID string, limit, offset int) ([]tenancy.Member, int, error) {
	q := s.mustQuerier()
	if _, err := authorize(ctx, q, p, orgID, tenancy.PermMembersList); err != nil {
		return nil, 0, err
	}
	return tenancy.ListMembers(ctx, q, orgID, limit, offset)
}

// ChangeRole changes another member's role according to the RBAC policy.
func (s *Service) ChangeRole(ctx context.Context, p Principal, orgID, targetUserID string, newRole tenancy.Role) error {
	return s.pool.InTx(ctx, func(tx pgx.Tx) error {
		actor, err := lockAndAuthorize(ctx, tx, p, orgID, tenancy.PermMembersChangeRole)
		if err != nil {
			return err
		}
		target, err := s.targetMember(ctx, tx, orgID, targetUserID, p.User.ID)
		if err != nil {
			return err
		}
		if newRole == tenancy.RoleOwner || target.Role == tenancy.RoleOwner {
			return ErrUseTransfer
		}
		if !tenancy.CanChangeRole(actor.Role, target.Role, newRole) {
			return ErrForbidden
		}
		if err := tenancy.SetRole(ctx, tx, orgID, targetUserID, newRole); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Event{
			OrganizationID: orgID, ActorUserID: p.User.ID, Action: audit.MemberRoleChanged,
			ResourceType: "membership", ResourceID: target.ID,
			Metadata: map[string]any{"targetUserId": targetUserID, "from": string(target.Role), "to": string(newRole)},
		})
	})
}

// RemoveMember removes another member according to the RBAC policy.
func (s *Service) RemoveMember(ctx context.Context, p Principal, orgID, targetUserID string) error {
	return s.pool.InTx(ctx, func(tx pgx.Tx) error {
		actor, err := lockAndAuthorize(ctx, tx, p, orgID, tenancy.PermMembersRemove)
		if err != nil {
			return err
		}
		target, err := s.targetMember(ctx, tx, orgID, targetUserID, p.User.ID)
		if err != nil {
			return err
		}
		if !tenancy.CanRemove(actor.Role, target.Role) {
			return ErrForbidden
		}
		if err := tenancy.RemoveMembership(ctx, tx, orgID, targetUserID); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Event{
			OrganizationID: orgID, ActorUserID: p.User.ID, Action: audit.MemberRemoved,
			ResourceType: "membership", ResourceID: target.ID,
			Metadata: map[string]any{"targetUserId": targetUserID, "role": string(target.Role)},
		})
	})
}

// TransferOwnership makes targetUserID the OWNER and demotes the caller to ADMIN
// in one transaction. The organization row lock serializes concurrent transfers,
// so exactly one OWNER exists before and after every commit.
func (s *Service) TransferOwnership(ctx context.Context, p Principal, orgID, targetUserID string) error {
	return s.pool.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := lockAndAuthorize(ctx, tx, p, orgID, tenancy.PermOwnershipTransfer); err != nil {
			return err
		}
		if !IsUUID(targetUserID) {
			return ErrMemberNotFound
		}
		if targetUserID == p.User.ID {
			return ErrAlreadyOwner
		}
		target, err := tenancy.ActiveMembership(ctx, tx, orgID, targetUserID)
		if errors.Is(err, tenancy.ErrNotMember) {
			return ErrMemberNotFound
		}
		if err != nil {
			return err
		}
		// Demote first: the unique index allows only one active OWNER at a time.
		if err := tenancy.SetRole(ctx, tx, orgID, p.User.ID, tenancy.RoleAdmin); err != nil {
			return err
		}
		if err := tenancy.SetRole(ctx, tx, orgID, targetUserID, tenancy.RoleOwner); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Event{
			OrganizationID: orgID, ActorUserID: p.User.ID, Action: audit.OwnershipTransferred,
			ResourceType: "organization", ResourceID: orgID,
			Metadata: map[string]any{"fromUserId": p.User.ID, "toUserId": targetUserID, "previousRoleOfTarget": string(target.Role)},
		})
	})
}

// targetMember loads the member an operation acts on, rejecting self-targeting.
func (s *Service) targetMember(ctx context.Context, tx pgx.Tx, orgID, targetUserID, actorID string) (tenancy.Membership, error) {
	if !IsUUID(targetUserID) {
		return tenancy.Membership{}, ErrMemberNotFound
	}
	if targetUserID == actorID {
		return tenancy.Membership{}, ErrSelfModification
	}
	m, err := tenancy.ActiveMembership(ctx, tx, orgID, targetUserID)
	if errors.Is(err, tenancy.ErrNotMember) {
		return tenancy.Membership{}, ErrMemberNotFound
	}
	return m, err
}

// CreateInvitation creates an invitation and returns the raw token exactly once.
// Only its SHA-256 hash is stored; the caller must deliver the token securely.
func (s *Service) CreateInvitation(ctx context.Context, p Principal, orgID, email string, role tenancy.Role) (tenancy.Invitation, string, error) {
	normalized, err := identity.NormalizeEmail(email)
	if err != nil {
		return tenancy.Invitation{}, "", Validation("email address is invalid")
	}
	rawToken, tokenHash, err := identity.NewToken()
	if err != nil {
		return tenancy.Invitation{}, "", err
	}

	var inv tenancy.Invitation
	err = s.pool.InTx(ctx, func(tx pgx.Tx) error {
		actor, err := lockAndAuthorize(ctx, tx, p, orgID, tenancy.PermMembersInvite)
		if err != nil {
			return err
		}
		if !tenancy.CanInviteRole(actor.Role, role) {
			return ErrForbidden
		}
		if existing, err := identity.UserByNormalizedEmail(ctx, tx, normalized); err == nil {
			if _, err := tenancy.ActiveMembership(ctx, tx, orgID, existing.ID); err == nil {
				return ErrAlreadyMember
			} else if !errors.Is(err, tenancy.ErrNotMember) {
				return err
			}
		} else if !errors.Is(err, identity.ErrUserNotFound) {
			return err
		}

		now := s.now()
		if err := tenancy.RevokeExpiredPending(ctx, tx, orgID, normalized, now); err != nil {
			return err
		}
		inv, err = tenancy.CreateInvitation(ctx, tx, orgID, normalized, p.User.ID, role, tokenHash, now, now.Add(s.cfg.InvitationLifetime))
		if errors.Is(err, tenancy.ErrPendingInvitationExist) {
			return ErrInvitationExists
		}
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Event{
			OrganizationID: orgID, ActorUserID: p.User.ID, Action: audit.InvitationCreated,
			ResourceType: "invitation", ResourceID: inv.ID,
			Metadata: map[string]any{"invitedEmail": normalized, "role": string(role)},
		})
	})
	if err != nil {
		return tenancy.Invitation{}, "", err
	}
	return inv, rawToken, nil
}

// ListInvitations returns a page of pending invitations.
func (s *Service) ListInvitations(ctx context.Context, p Principal, orgID string, limit, offset int) ([]tenancy.Invitation, int, error) {
	q := s.mustQuerier()
	if _, err := authorize(ctx, q, p, orgID, tenancy.PermInvitationsList); err != nil {
		return nil, 0, err
	}
	return tenancy.ListPendingInvitations(ctx, q, orgID, s.now(), limit, offset)
}

// RevokeInvitation revokes a pending invitation of the organization.
func (s *Service) RevokeInvitation(ctx context.Context, p Principal, orgID, invitationID string) error {
	return s.pool.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := lockAndAuthorize(ctx, tx, p, orgID, tenancy.PermInvitationsRevoke); err != nil {
			return err
		}
		if !IsUUID(invitationID) {
			return ErrNotFound
		}
		inv, err := tenancy.RevokePendingInvitation(ctx, tx, orgID, invitationID, s.now())
		if errors.Is(err, tenancy.ErrInvitationNotFound) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Event{
			OrganizationID: orgID, ActorUserID: p.User.ID, Action: audit.InvitationRevoked,
			ResourceType: "invitation", ResourceID: inv.ID, Metadata: map[string]any{"invitedEmail": inv.InvitedEmail},
		})
	})
}

// AcceptInvitation consumes an invitation token for the authenticated user.
// Unknown, expired, revoked, used and wrong-recipient invitations all return
// ErrInvalidInvitation; the wrapped reason distinguishes them for tests and logs.
func (s *Service) AcceptInvitation(ctx context.Context, p Principal, rawToken string) (tenancy.OrganizationWithRole, error) {
	if !identity.WellFormedToken(rawToken) {
		return tenancy.OrganizationWithRole{}, invalidInvitation(ReasonInvitationUnknown)
	}
	var result tenancy.OrganizationWithRole
	err := s.pool.InTx(ctx, func(tx pgx.Tx) error {
		inv, err := tenancy.InvitationByTokenHashForUpdate(ctx, tx, identity.HashToken(rawToken))
		if errors.Is(err, tenancy.ErrInvitationNotFound) {
			return invalidInvitation(ReasonInvitationUnknown)
		}
		if err != nil {
			return err
		}
		now := s.now()
		switch {
		case inv.AcceptedAt != nil:
			return invalidInvitation(ReasonInvitationUsed)
		case inv.RevokedAt != nil:
			return invalidInvitation(ReasonInvitationRevoked)
		case !inv.ExpiresAt.After(now):
			return invalidInvitation(ReasonInvitationExpired)
		case inv.InvitedEmail != p.User.NormalizedEmail:
			return invalidInvitation(ReasonInvitationRecipient)
		}

		if err := tenancy.LockOrganization(ctx, tx, inv.OrganizationID); err != nil {
			return err
		}
		if err := tenancy.MarkInvitationAccepted(ctx, tx, inv.ID, now); err != nil {
			if errors.Is(err, tenancy.ErrInvitationNotFound) {
				return invalidInvitation(ReasonInvitationUsed)
			}
			return err
		}
		m, err := tenancy.AddMembership(ctx, tx, inv.OrganizationID, p.User.ID, inv.Role)
		if errors.Is(err, tenancy.ErrAlreadyMember) {
			return ErrAlreadyMember
		}
		if err != nil {
			return err
		}
		org, err := tenancy.OrganizationByID(ctx, tx, inv.OrganizationID)
		if err != nil {
			return err
		}
		result = tenancy.OrganizationWithRole{Organization: org, Role: m.Role}
		return audit.Record(ctx, tx, audit.Event{
			OrganizationID: inv.OrganizationID, ActorUserID: p.User.ID, Action: audit.InvitationAccepted,
			ResourceType: "invitation", ResourceID: inv.ID, Metadata: map[string]any{"role": string(inv.Role)},
		})
	})
	return result, err
}

// ListAuditEvents returns a page of the organization's audit events.
func (s *Service) ListAuditEvents(ctx context.Context, p Principal, orgID string, limit, offset int) ([]audit.Event, int, error) {
	q := s.mustQuerier()
	if _, err := authorize(ctx, q, p, orgID, tenancy.PermAuditView); err != nil {
		return nil, 0, err
	}
	return audit.ListForOrganization(ctx, q, orgID, limit, offset)
}
