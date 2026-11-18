// Package audit records security-sensitive operations in an append-only table.
//
// Callers pass the same transaction as the mutation being audited, so the event
// and the change commit or roll back together. Metadata must never contain
// passwords, hashes, session, CSRF or invitation tokens, or request bodies.
package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Thiago-tods/relayforge/internal/database"
)

// Audited actions.
const (
	UserRegistered       = "user.registered"
	UserLoginSucceeded   = "user.login.succeeded"
	UserLogout           = "user.logout"
	OrganizationCreated  = "organization.created"
	OrganizationUpdated  = "organization.updated"
	InvitationCreated    = "organization.invitation.created"
	InvitationRevoked    = "organization.invitation.revoked"
	InvitationAccepted   = "organization.invitation.accepted"
	MemberRoleChanged    = "organization.member.role_changed"
	MemberRemoved        = "organization.member.removed"
	OwnershipTransferred = "organization.ownership.transferred"

	EndpointCreated       = "ingestion_endpoint.created"
	EndpointUpdated       = "ingestion_endpoint.updated"
	EndpointPaused        = "ingestion_endpoint.paused"
	EndpointActivated     = "ingestion_endpoint.activated"
	EndpointRevoked       = "ingestion_endpoint.revoked"
	EndpointSecretRotated = "ingestion_endpoint.secret_rotated"

	DestinationCreated       = "destination.created"
	DestinationUpdated       = "destination.updated"
	DestinationPaused        = "destination.paused"
	DestinationActivated     = "destination.activated"
	DestinationRevoked       = "destination.revoked"
	DestinationSecretRotated = "destination.secret_rotated"
	SubscriptionCreated      = "subscription.created"
	SubscriptionRevoked      = "subscription.revoked"
)

// Event is one audit record. OrganizationID, ActorUserID and ResourceID are
// empty when not applicable.
type Event struct {
	ID             string         `json:"id"`
	OrganizationID string         `json:"organizationId,omitempty"`
	ActorUserID    string         `json:"actorUserId,omitempty"`
	Action         string         `json:"action"`
	ResourceType   string         `json:"resourceType"`
	ResourceID     string         `json:"resourceId,omitempty"`
	Metadata       map[string]any `json:"metadata"`
	CreatedAt      time.Time      `json:"createdAt"`
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Record appends an event using db, which should be the active transaction.
func Record(ctx context.Context, db database.Querier, e Event) error {
	meta := e.Metadata
	if meta == nil {
		meta = map[string]any{}
	}
	raw, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("encode audit metadata: %w", err)
	}
	if _, err := db.Exec(ctx,
		`INSERT INTO audit_events (organization_id, actor_user_id, action, resource_type, resource_id, metadata)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		nullable(e.OrganizationID), nullable(e.ActorUserID), e.Action, e.ResourceType, nullable(e.ResourceID), raw); err != nil {
		return fmt.Errorf("insert audit event: %w", err)
	}
	return nil
}

// ListForOrganization returns a page of one organization's events, newest first.
// The organization filter is part of the query, so events of other tenants can
// never be returned.
func ListForOrganization(ctx context.Context, db database.Querier, orgID string, limit, offset int) ([]Event, int, error) {
	var total int
	if err := db.QueryRow(ctx, "SELECT count(*) FROM audit_events WHERE organization_id = $1", orgID).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count audit events: %w", err)
	}
	rows, err := db.Query(ctx,
		`SELECT id, coalesce(organization_id::text, ''), coalesce(actor_user_id::text, ''), action,
		        resource_type, coalesce(resource_id::text, ''), metadata, created_at
		   FROM audit_events WHERE organization_id = $1
		  ORDER BY created_at DESC, id DESC LIMIT $2 OFFSET $3`, orgID, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("list audit events: %w", err)
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var e Event
		var meta []byte
		if err := rows.Scan(&e.ID, &e.OrganizationID, &e.ActorUserID, &e.Action, &e.ResourceType, &e.ResourceID, &meta, &e.CreatedAt); err != nil {
			return nil, 0, fmt.Errorf("scan audit event: %w", err)
		}
		if err := json.Unmarshal(meta, &e.Metadata); err != nil {
			return nil, 0, fmt.Errorf("decode audit metadata: %w", err)
		}
		out = append(out, e)
	}
	return out, total, rows.Err()
}
