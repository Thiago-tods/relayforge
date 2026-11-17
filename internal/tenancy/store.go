package tenancy

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Thiago-tods/relayforge/internal/database"
)

// Membership statuses.
const (
	MemberActive  = "ACTIVE"
	MemberRemoved = "REMOVED"
)

// Sentinel errors returned by the repositories.
var (
	ErrOrgNotFound            = errors.New("organization not found")
	ErrNotMember              = errors.New("user is not an active member")
	ErrSlugTaken              = errors.New("organization slug is already taken")
	ErrAlreadyMember          = errors.New("user is already an active member")
	ErrInvitationNotFound     = errors.New("invitation not found")
	ErrPendingInvitationExist = errors.New("a pending invitation already exists for this email")
)

// Organization is a tenant.
type Organization struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Slug      string    `json:"slug"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Membership links a user to an organization with a role.
type Membership struct {
	ID             string
	OrganizationID string
	UserID         string
	Role           Role
	Status         string
}

// OrganizationWithRole is an organization together with the caller's role in it.
type OrganizationWithRole struct {
	Organization
	Role Role `json:"role"`
}

// Member is the public view of an active member.
type Member struct {
	UserID      string    `json:"userId"`
	DisplayName string    `json:"displayName"`
	Email       string    `json:"email"`
	Role        Role      `json:"role"`
	JoinedAt    time.Time `json:"joinedAt"`
}

// Invitation is a pending or historical invitation. The token hash is never exposed.
type Invitation struct {
	ID              string     `json:"id"`
	OrganizationID  string     `json:"organizationId"`
	InvitedEmail    string     `json:"invitedEmail"`
	InvitedByUserID string     `json:"invitedByUserId"`
	Role            Role       `json:"role"`
	ExpiresAt       time.Time  `json:"expiresAt"`
	AcceptedAt      *time.Time `json:"acceptedAt,omitempty"`
	RevokedAt       *time.Time `json:"revokedAt,omitempty"`
	CreatedAt       time.Time  `json:"createdAt"`
}

const orgColumns = "id, name, slug, created_at, updated_at"

func scanOrg(row pgx.Row) (Organization, error) {
	var o Organization
	err := row.Scan(&o.ID, &o.Name, &o.Slug, &o.CreatedAt, &o.UpdatedAt)
	return o, err
}

func isUnique(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == constraint
}

// CreateOrganization inserts an organization. It returns ErrSlugTaken on a slug collision.
func CreateOrganization(ctx context.Context, db database.Querier, name, slug string) (Organization, error) {
	o, err := scanOrg(db.QueryRow(ctx,
		"INSERT INTO organizations (name, slug) VALUES ($1, $2) RETURNING "+orgColumns, name, slug))
	if err != nil {
		if isUnique(err, "organizations_slug_key") {
			return Organization{}, ErrSlugTaken
		}
		return Organization{}, fmt.Errorf("insert organization: %w", err)
	}
	return o, nil
}

// LockOrganization takes a row lock on the organization for the rest of the
// transaction. Every operation that changes memberships calls it first, which
// serializes concurrent role changes, removals, acceptances and transfers.
func LockOrganization(ctx context.Context, db database.Querier, orgID string) error {
	var id string
	err := db.QueryRow(ctx, "SELECT id FROM organizations WHERE id = $1 FOR UPDATE", orgID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrOrgNotFound
	}
	if err != nil {
		return fmt.Errorf("lock organization: %w", err)
	}
	return nil
}

// OrganizationByID loads an organization without any authorization. Callers
// must have verified membership first.
func OrganizationByID(ctx context.Context, db database.Querier, orgID string) (Organization, error) {
	o, err := scanOrg(db.QueryRow(ctx, "SELECT "+orgColumns+" FROM organizations WHERE id = $1", orgID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Organization{}, ErrOrgNotFound
	}
	if err != nil {
		return Organization{}, fmt.Errorf("select organization: %w", err)
	}
	return o, nil
}

// UpdateOrganization changes the name and/or slug; nil leaves a field unchanged.
func UpdateOrganization(ctx context.Context, db database.Querier, orgID string, name, slug *string) (Organization, error) {
	o, err := scanOrg(db.QueryRow(ctx,
		`UPDATE organizations SET name = COALESCE($2, name), slug = COALESCE($3, slug)
		  WHERE id = $1 RETURNING `+orgColumns, orgID, name, slug))
	if errors.Is(err, pgx.ErrNoRows) {
		return Organization{}, ErrOrgNotFound
	}
	if err != nil {
		if isUnique(err, "organizations_slug_key") {
			return Organization{}, ErrSlugTaken
		}
		return Organization{}, fmt.Errorf("update organization: %w", err)
	}
	return o, nil
}

// AddMembership grants role to userID. A previously REMOVED membership is
// reactivated with the new role; an ACTIVE one yields ErrAlreadyMember.
func AddMembership(ctx context.Context, db database.Querier, orgID, userID string, role Role) (Membership, error) {
	var m Membership
	err := db.QueryRow(ctx,
		`INSERT INTO memberships (organization_id, user_id, role) VALUES ($1, $2, $3)
		 ON CONFLICT (organization_id, user_id)
		 DO UPDATE SET role = EXCLUDED.role, status = 'ACTIVE'
		       WHERE memberships.status = 'REMOVED'
		 RETURNING id, organization_id, user_id, role, status`,
		orgID, userID, string(role)).
		Scan(&m.ID, &m.OrganizationID, &m.UserID, &m.Role, &m.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return Membership{}, ErrAlreadyMember // conflict on an ACTIVE row: DO UPDATE was skipped
	}
	if err != nil {
		return Membership{}, fmt.Errorf("insert membership: %w", err)
	}
	return m, nil
}

// ActiveMembership returns the user's ACTIVE membership in the organization, or
// ErrNotMember. It is the tenant-isolation gate: a missing organization and a
// missing membership are indistinguishable to the caller.
func ActiveMembership(ctx context.Context, db database.Querier, orgID, userID string) (Membership, error) {
	var m Membership
	err := db.QueryRow(ctx,
		`SELECT id, organization_id, user_id, role, status FROM memberships
		  WHERE organization_id = $1 AND user_id = $2 AND status = 'ACTIVE'`, orgID, userID).
		Scan(&m.ID, &m.OrganizationID, &m.UserID, &m.Role, &m.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return Membership{}, ErrNotMember
	}
	if err != nil {
		return Membership{}, fmt.Errorf("select membership: %w", err)
	}
	return m, nil
}

// OrganizationsForUser lists organizations in which the user has an ACTIVE membership.
func OrganizationsForUser(ctx context.Context, db database.Querier, userID string) ([]OrganizationWithRole, error) {
	rows, err := db.Query(ctx,
		`SELECT o.id, o.name, o.slug, o.created_at, o.updated_at, m.role
		   FROM memberships m JOIN organizations o ON o.id = m.organization_id
		  WHERE m.user_id = $1 AND m.status = 'ACTIVE'
		  ORDER BY o.name, o.id`, userID)
	if err != nil {
		return nil, fmt.Errorf("list organizations: %w", err)
	}
	defer rows.Close()
	out := []OrganizationWithRole{}
	for rows.Next() {
		var o OrganizationWithRole
		if err := rows.Scan(&o.ID, &o.Name, &o.Slug, &o.CreatedAt, &o.UpdatedAt, &o.Role); err != nil {
			return nil, fmt.Errorf("scan organization: %w", err)
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// ListMembers returns a page of active members and the total count.
func ListMembers(ctx context.Context, db database.Querier, orgID string, limit, offset int) ([]Member, int, error) {
	var total int
	if err := db.QueryRow(ctx,
		"SELECT count(*) FROM memberships WHERE organization_id = $1 AND status = 'ACTIVE'", orgID).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count members: %w", err)
	}
	rows, err := db.Query(ctx,
		`SELECT u.id, u.display_name, u.email, m.role, m.created_at
		   FROM memberships m JOIN users u ON u.id = m.user_id
		  WHERE m.organization_id = $1 AND m.status = 'ACTIVE'
		  ORDER BY m.created_at, u.id LIMIT $2 OFFSET $3`, orgID, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("list members: %w", err)
	}
	defer rows.Close()
	out := []Member{}
	for rows.Next() {
		var m Member
		if err := rows.Scan(&m.UserID, &m.DisplayName, &m.Email, &m.Role, &m.JoinedAt); err != nil {
			return nil, 0, fmt.Errorf("scan member: %w", err)
		}
		out = append(out, m)
	}
	return out, total, rows.Err()
}

// SetRole changes the role of an ACTIVE membership, scoped to the organization.
func SetRole(ctx context.Context, db database.Querier, orgID, userID string, role Role) error {
	tag, err := db.Exec(ctx,
		"UPDATE memberships SET role = $3 WHERE organization_id = $1 AND user_id = $2 AND status = 'ACTIVE'",
		orgID, userID, string(role))
	if err != nil {
		return fmt.Errorf("set role: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotMember
	}
	return nil
}

// RemoveMembership marks an ACTIVE membership REMOVED, scoped to the organization.
func RemoveMembership(ctx context.Context, db database.Querier, orgID, userID string) error {
	tag, err := db.Exec(ctx,
		"UPDATE memberships SET status = 'REMOVED' WHERE organization_id = $1 AND user_id = $2 AND status = 'ACTIVE'",
		orgID, userID)
	if err != nil {
		return fmt.Errorf("remove membership: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotMember
	}
	return nil
}

const invitationColumns = "id, organization_id, invited_email, invited_by_user_id, role, expires_at, accepted_at, revoked_at, created_at"

func scanInvitation(row pgx.Row) (Invitation, error) {
	var i Invitation
	err := row.Scan(&i.ID, &i.OrganizationID, &i.InvitedEmail, &i.InvitedByUserID, &i.Role,
		&i.ExpiresAt, &i.AcceptedAt, &i.RevokedAt, &i.CreatedAt)
	return i, err
}

// RevokeExpiredPending marks expired, still-pending invitations for the same
// organization and email as revoked so a fresh invitation can be created.
func RevokeExpiredPending(ctx context.Context, db database.Querier, orgID, email string, now time.Time) error {
	if _, err := db.Exec(ctx,
		`UPDATE organization_invitations SET revoked_at = $3
		  WHERE organization_id = $1 AND invited_email = $2
		    AND accepted_at IS NULL AND revoked_at IS NULL AND expires_at <= $3`, orgID, email, now); err != nil {
		return fmt.Errorf("revoke expired invitations: %w", err)
	}
	return nil
}

// CreateInvitation stores an invitation by token hash. It returns
// ErrPendingInvitationExist if an unexpired pending invitation already exists.
func CreateInvitation(ctx context.Context, db database.Querier, orgID, email, invitedBy string, role Role,
	tokenHash []byte, now, expiresAt time.Time) (Invitation, error) {
	i, err := scanInvitation(db.QueryRow(ctx,
		`INSERT INTO organization_invitations
		   (organization_id, invited_email, invited_by_user_id, role, token_hash, created_at, expires_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING `+invitationColumns,
		orgID, email, invitedBy, string(role), tokenHash, now, expiresAt))
	if err != nil {
		if isUnique(err, "organization_invitations_one_pending_idx") {
			return Invitation{}, ErrPendingInvitationExist
		}
		return Invitation{}, fmt.Errorf("insert invitation: %w", err)
	}
	return i, nil
}

// ListPendingInvitations returns unexpired, unaccepted, unrevoked invitations of one organization.
func ListPendingInvitations(ctx context.Context, db database.Querier, orgID string, now time.Time, limit, offset int) ([]Invitation, int, error) {
	var total int
	if err := db.QueryRow(ctx,
		`SELECT count(*) FROM organization_invitations
		  WHERE organization_id = $1 AND accepted_at IS NULL AND revoked_at IS NULL AND expires_at > $2`,
		orgID, now).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count invitations: %w", err)
	}
	rows, err := db.Query(ctx,
		`SELECT `+invitationColumns+` FROM organization_invitations
		  WHERE organization_id = $1 AND accepted_at IS NULL AND revoked_at IS NULL AND expires_at > $2
		  ORDER BY created_at DESC, id LIMIT $3 OFFSET $4`, orgID, now, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("list invitations: %w", err)
	}
	defer rows.Close()
	out := []Invitation{}
	for rows.Next() {
		i, err := scanInvitation(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("scan invitation: %w", err)
		}
		out = append(out, i)
	}
	return out, total, rows.Err()
}

// RevokePendingInvitation revokes one pending invitation of the given organization.
// The organization scope prevents revoking another tenant's invitation by ID.
func RevokePendingInvitation(ctx context.Context, db database.Querier, orgID, invitationID string, now time.Time) (Invitation, error) {
	i, err := scanInvitation(db.QueryRow(ctx,
		`UPDATE organization_invitations SET revoked_at = $3
		  WHERE id = $2 AND organization_id = $1 AND accepted_at IS NULL AND revoked_at IS NULL
		  RETURNING `+invitationColumns, orgID, invitationID, now))
	if errors.Is(err, pgx.ErrNoRows) {
		return Invitation{}, ErrInvitationNotFound
	}
	if err != nil {
		return Invitation{}, fmt.Errorf("revoke invitation: %w", err)
	}
	return i, nil
}

// InvitationByTokenHashForUpdate loads and row-locks an invitation by token hash.
func InvitationByTokenHashForUpdate(ctx context.Context, db database.Querier, tokenHash []byte) (Invitation, error) {
	i, err := scanInvitation(db.QueryRow(ctx,
		"SELECT "+invitationColumns+" FROM organization_invitations WHERE token_hash = $1 FOR UPDATE", tokenHash))
	if errors.Is(err, pgx.ErrNoRows) {
		return Invitation{}, ErrInvitationNotFound
	}
	if err != nil {
		return Invitation{}, fmt.Errorf("select invitation: %w", err)
	}
	return i, nil
}

// MarkInvitationAccepted records acceptance. It only succeeds for a pending
// invitation, which makes acceptance one-time even under concurrency.
func MarkInvitationAccepted(ctx context.Context, db database.Querier, invitationID string, now time.Time) error {
	tag, err := db.Exec(ctx,
		`UPDATE organization_invitations SET accepted_at = $2
		  WHERE id = $1 AND accepted_at IS NULL AND revoked_at IS NULL`, invitationID, now)
	if err != nil {
		return fmt.Errorf("accept invitation: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrInvitationNotFound
	}
	return nil
}
