package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/Thiago-tods/relayforge/internal/audit"
	"github.com/Thiago-tods/relayforge/internal/ingest"
	"github.com/Thiago-tods/relayforge/internal/tenancy"
)

// EndpointWithSecret is an endpoint together with its raw signing secret. It is
// returned only when the endpoint is created or its secret is rotated; the
// secret cannot be retrieved again.
type EndpointWithSecret struct {
	Endpoint      ingest.Endpoint
	SigningSecret string
}

// EventFilter narrows an event listing.
type EventFilter struct {
	EndpointID string
	Status     string
	From, To   time.Time
}

// NormalizeEndpointName trims and validates an endpoint name: 1 to 100 valid
// UTF-8 characters without control characters.
func NormalizeEndpointName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" || !utf8.ValidString(name) || utf8.RuneCountInString(name) > 100 || strings.ContainsFunc(name, unicode.IsControl) {
		return "", Validation("endpoint name must be 1 to 100 printable characters")
	}
	return name, nil
}

func (s *Service) requireBox() error {
	if s.cfg.Box == nil {
		return fmt.Errorf("signing secret encryption is not configured")
	}
	return nil
}

// newSecretMaterial generates a signing secret and seals it for the endpoint.
func (s *Service) newSecretMaterial(endpointID string) (secret string, sealed []byte, err error) {
	if secret, err = ingest.GenerateSecret(); err != nil {
		return "", nil, err
	}
	if sealed, err = s.cfg.Box.Seal([]byte(secret), ingest.SecretAAD(endpointID)); err != nil {
		return "", nil, fmt.Errorf("encrypt signing secret: %w", err)
	}
	return secret, sealed, nil
}

// CreateEndpoint creates an ACTIVE ingestion endpoint and returns its signing
// secret exactly once.
func (s *Service) CreateEndpoint(ctx context.Context, p Principal, orgID, name string) (EndpointWithSecret, error) {
	if err := s.requireBox(); err != nil {
		return EndpointWithSecret{}, err
	}
	cleanName, err := NormalizeEndpointName(name)
	if err != nil {
		return EndpointWithSecret{}, err
	}
	id, err := ingest.NewUUID()
	if err != nil {
		return EndpointWithSecret{}, err
	}
	publicID, err := ingest.GeneratePublicID()
	if err != nil {
		return EndpointWithSecret{}, err
	}
	secret, sealed, err := s.newSecretMaterial(id)
	if err != nil {
		return EndpointWithSecret{}, err
	}

	var created ingest.Endpoint
	err = s.pool.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := lockAndAuthorize(ctx, tx, p, orgID, tenancy.PermEndpointsCreate); err != nil {
			return err
		}
		var err error
		created, err = ingest.InsertEndpoint(ctx, tx, ingest.Endpoint{
			ID: id, OrganizationID: orgID, PublicID: publicID, Name: cleanName,
			SigningSecretCiphertext: sealed, EncryptionKeyVersion: s.cfg.Box.Version(), CreatedByUserID: p.User.ID,
		})
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Event{
			OrganizationID: orgID, ActorUserID: p.User.ID, Action: audit.EndpointCreated,
			ResourceType: "ingestion_endpoint", ResourceID: id, Metadata: map[string]any{"name": cleanName},
		})
	})
	if err != nil {
		return EndpointWithSecret{}, err
	}
	return EndpointWithSecret{Endpoint: created, SigningSecret: secret}, nil
}

// ListEndpoints returns a page of the organization's endpoints.
func (s *Service) ListEndpoints(ctx context.Context, p Principal, orgID string, limit, offset int) ([]ingest.Endpoint, int, error) {
	q := s.mustQuerier()
	if _, err := authorize(ctx, q, p, orgID, tenancy.PermEndpointsRead); err != nil {
		return nil, 0, err
	}
	return ingest.ListEndpoints(ctx, q, orgID, limit, offset)
}

// GetEndpoint returns one endpoint of the organization.
func (s *Service) GetEndpoint(ctx context.Context, p Principal, orgID, endpointID string) (ingest.Endpoint, error) {
	q := s.mustQuerier()
	if _, err := authorize(ctx, q, p, orgID, tenancy.PermEndpointsRead); err != nil {
		return ingest.Endpoint{}, err
	}
	if !IsUUID(endpointID) {
		return ingest.Endpoint{}, ErrNotFound
	}
	e, err := ingest.EndpointByID(ctx, q, orgID, endpointID, false)
	if errors.Is(err, ingest.ErrEndpointNotFound) {
		return ingest.Endpoint{}, ErrNotFound
	}
	return e, err
}

// mutateEndpoint runs fn on a row-locked, non-revoked endpoint inside a transaction
// after authorizing perm. Revoked endpoints are immutable.
func (s *Service) mutateEndpoint(ctx context.Context, p Principal, orgID, endpointID string, perm tenancy.Permission,
	fn func(tx pgx.Tx, e ingest.Endpoint) (ingest.Endpoint, error)) (ingest.Endpoint, error) {
	var out ingest.Endpoint
	err := s.pool.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := lockAndAuthorize(ctx, tx, p, orgID, perm); err != nil {
			return err
		}
		if !IsUUID(endpointID) {
			return ErrNotFound
		}
		// FOR UPDATE waits for in-flight ingestion transactions, which hold FOR SHARE.
		e, err := ingest.EndpointByID(ctx, tx, orgID, endpointID, true)
		if errors.Is(err, ingest.ErrEndpointNotFound) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if e.Status == ingest.StatusRevoked {
			return ErrEndpointRevoked
		}
		out, err = fn(tx, e)
		return err
	})
	return out, err
}

// UpdateEndpoint renames an endpoint.
func (s *Service) UpdateEndpoint(ctx context.Context, p Principal, orgID, endpointID, name string) (ingest.Endpoint, error) {
	cleanName, err := NormalizeEndpointName(name)
	if err != nil {
		return ingest.Endpoint{}, err
	}
	return s.mutateEndpoint(ctx, p, orgID, endpointID, tenancy.PermEndpointsUpdate, func(tx pgx.Tx, e ingest.Endpoint) (ingest.Endpoint, error) {
		out, err := ingest.RenameEndpoint(ctx, tx, orgID, e.ID, cleanName)
		if err != nil {
			return ingest.Endpoint{}, err
		}
		return out, audit.Record(ctx, tx, audit.Event{
			OrganizationID: orgID, ActorUserID: p.User.ID, Action: audit.EndpointUpdated,
			ResourceType: "ingestion_endpoint", ResourceID: e.ID, Metadata: map[string]any{"name": cleanName},
		})
	})
}

func (s *Service) setStatus(ctx context.Context, p Principal, orgID, endpointID, status, action string) (ingest.Endpoint, error) {
	return s.mutateEndpoint(ctx, p, orgID, endpointID, tenancy.PermEndpointsPause, func(tx pgx.Tx, e ingest.Endpoint) (ingest.Endpoint, error) {
		if e.Status == status {
			return e, nil // already in the requested state: nothing to change or audit
		}
		out, err := ingest.SetEndpointStatus(ctx, tx, orgID, e.ID, status, s.now())
		if err != nil {
			return ingest.Endpoint{}, err
		}
		return out, audit.Record(ctx, tx, audit.Event{
			OrganizationID: orgID, ActorUserID: p.User.ID, Action: action,
			ResourceType: "ingestion_endpoint", ResourceID: e.ID, Metadata: map[string]any{"from": e.Status, "to": status},
		})
	})
}

// PauseEndpoint stops an endpoint from accepting events. It can be activated again.
func (s *Service) PauseEndpoint(ctx context.Context, p Principal, orgID, endpointID string) (ingest.Endpoint, error) {
	return s.setStatus(ctx, p, orgID, endpointID, ingest.StatusPaused, audit.EndpointPaused)
}

// ActivateEndpoint resumes a paused endpoint. Revoked endpoints cannot be activated.
func (s *Service) ActivateEndpoint(ctx context.Context, p Principal, orgID, endpointID string) (ingest.Endpoint, error) {
	return s.setStatus(ctx, p, orgID, endpointID, ingest.StatusActive, audit.EndpointActivated)
}

// RotateSecret replaces the signing secret and returns the new one exactly once.
// The old secret stops working when the transaction commits. Ingestion
// transactions hold a shared lock on the endpoint, so a request either commits
// before the rotation (and was valid under the old secret) or is rejected.
func (s *Service) RotateSecret(ctx context.Context, p Principal, orgID, endpointID string) (EndpointWithSecret, error) {
	if err := s.requireBox(); err != nil {
		return EndpointWithSecret{}, err
	}
	var secret string
	e, err := s.mutateEndpoint(ctx, p, orgID, endpointID, tenancy.PermEndpointsRotate, func(tx pgx.Tx, e ingest.Endpoint) (ingest.Endpoint, error) {
		var sealed []byte
		var err error
		if secret, sealed, err = s.newSecretMaterial(e.ID); err != nil {
			return ingest.Endpoint{}, err
		}
		out, err := ingest.ReplaceSecret(ctx, tx, orgID, e.ID, sealed, s.cfg.Box.Version())
		if err != nil {
			return ingest.Endpoint{}, err
		}
		return out, audit.Record(ctx, tx, audit.Event{
			OrganizationID: orgID, ActorUserID: p.User.ID, Action: audit.EndpointSecretRotated,
			ResourceType: "ingestion_endpoint", ResourceID: e.ID, Metadata: map[string]any{"secretVersion": out.SecretVersion},
		})
	})
	if err != nil {
		return EndpointWithSecret{}, err
	}
	return EndpointWithSecret{Endpoint: e, SigningSecret: secret}, nil
}

// RevokeEndpoint permanently disables an endpoint. The record and its events
// are kept; a revoked endpoint cannot be reactivated, paused, renamed or rotated.
// Revoking twice is a no-op.
func (s *Service) RevokeEndpoint(ctx context.Context, p Principal, orgID, endpointID string) (ingest.Endpoint, error) {
	var out ingest.Endpoint
	err := s.pool.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := lockAndAuthorize(ctx, tx, p, orgID, tenancy.PermEndpointsRevoke); err != nil {
			return err
		}
		if !IsUUID(endpointID) {
			return ErrNotFound
		}
		e, err := ingest.EndpointByID(ctx, tx, orgID, endpointID, true)
		if errors.Is(err, ingest.ErrEndpointNotFound) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if e.Status == ingest.StatusRevoked {
			out = e
			return nil
		}
		if out, err = ingest.SetEndpointStatus(ctx, tx, orgID, e.ID, ingest.StatusRevoked, s.now()); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Event{
			OrganizationID: orgID, ActorUserID: p.User.ID, Action: audit.EndpointRevoked,
			ResourceType: "ingestion_endpoint", ResourceID: e.ID, Metadata: map[string]any{"from": e.Status},
		})
	})
	return out, err
}

// ListEvents returns a page of event metadata (never payloads or outbox data).
func (s *Service) ListEvents(ctx context.Context, p Principal, orgID string, f EventFilter, limit, offset int) ([]ingest.Event, int, error) {
	q := s.mustQuerier()
	if _, err := authorize(ctx, q, p, orgID, tenancy.PermEventsList); err != nil {
		return nil, 0, err
	}
	if f.EndpointID != "" && !IsUUID(f.EndpointID) {
		return nil, 0, Validation("endpointId must be a UUID")
	}
	if f.Status != "" && f.Status != ingest.EventReceived {
		return nil, 0, Validation("status must be RECEIVED")
	}
	if !f.From.IsZero() && !f.To.IsZero() && !f.From.Before(f.To) {
		return nil, 0, Validation("from must be before to")
	}
	return ingest.ListEvents(ctx, q, orgID, ingest.EventFilter{EndpointID: f.EndpointID, Status: f.Status, From: f.From, To: f.To}, limit, offset)
}

// GetEvent returns one event including its payload. Reading payloads needs
// the events:read_payload permission (OWNER, ADMIN, DEVELOPER).
func (s *Service) GetEvent(ctx context.Context, p Principal, orgID, eventID string) (ingest.Event, error) {
	q := s.mustQuerier()
	if _, err := authorize(ctx, q, p, orgID, tenancy.PermEventsReadPayload); err != nil {
		return ingest.Event{}, err
	}
	if !IsUUID(eventID) {
		return ingest.Event{}, ErrNotFound
	}
	e, err := ingest.EventByID(ctx, q, orgID, eventID)
	if errors.Is(err, ingest.ErrEventNotFound) {
		return ingest.Event{}, ErrNotFound
	}
	return e, err
}
