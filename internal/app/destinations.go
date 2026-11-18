package app

import (
	"context"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/Thiago-tods/relayforge/internal/audit"
	"github.com/Thiago-tods/relayforge/internal/delivery"
	"github.com/Thiago-tods/relayforge/internal/egress"
	"github.com/Thiago-tods/relayforge/internal/ingest"
	"github.com/Thiago-tods/relayforge/internal/tenancy"
)

// DestinationWithSecret is a destination together with its raw signing secret.
// The secret is returned only when the destination is created or its secret is
// rotated; it cannot be retrieved again.
type DestinationWithSecret struct {
	Destination   delivery.Destination
	SigningSecret string
}

// NormalizeDestinationName trims and validates a destination name: 1 to 100
// valid UTF-8 characters without control characters.
func NormalizeDestinationName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" || !utf8.ValidString(name) || utf8.RuneCountInString(name) > 100 || strings.ContainsFunc(name, unicode.IsControl) {
		return "", Validation("destination name must be 1 to 100 printable characters")
	}
	return name, nil
}

// normalizeDestinationURL applies the SSRF-safe URL policy. The error carries only
// a stable reason code, never the submitted URL (its query may hold a secret).
func normalizeDestinationURL(raw string) (egress.ValidatedURL, error) {
	v, err := egress.ValidateDestinationURL(raw, egress.DefaultURLPolicy())
	if err != nil {
		var ue *egress.URLError
		if errors.As(err, &ue) {
			return egress.ValidatedURL{}, Validation("destination URL is not allowed: " + ue.Code)
		}
		return egress.ValidatedURL{}, Validation("destination URL is not allowed")
	}
	return v, nil
}

// newDestinationSecret generates a destination secret sealed with the
// destination-specific associated data of ADR 22.
func (s *Service) newDestinationSecret(destinationID string) (secret string, sealed []byte, err error) {
	if secret, err = delivery.GenerateDestinationSecret(); err != nil {
		return "", nil, err
	}
	if sealed, err = s.cfg.Box.Seal([]byte(secret), delivery.DestinationSecretAAD(destinationID)); err != nil {
		return "", nil, errors.New("encrypt destination secret")
	}
	return secret, sealed, nil
}

func destinationErr(err error) error {
	switch {
	case errors.Is(err, delivery.ErrDestinationNotFound):
		return ErrNotFound
	case errors.Is(err, delivery.ErrDestinationNameTaken):
		return ErrDestinationNameTaken
	default:
		return err
	}
}

// CreateDestination creates an ACTIVE destination and returns its signing secret
// exactly once. The URL is validated and normalized with the SSRF-safe policy.
func (s *Service) CreateDestination(ctx context.Context, p Principal, orgID, name, rawURL string) (DestinationWithSecret, error) {
	if err := s.requireBox(); err != nil {
		return DestinationWithSecret{}, err
	}
	// Authorize before looking at the input, so a caller without permission learns
	// nothing about validation rules. The check is repeated under the organization
	// lock inside the transaction below.
	if _, err := authorize(ctx, s.mustQuerier(), p, orgID, tenancy.PermDestinationsCreate); err != nil {
		return DestinationWithSecret{}, err
	}
	cleanName, err := NormalizeDestinationName(name)
	if err != nil {
		return DestinationWithSecret{}, err
	}
	u, err := normalizeDestinationURL(rawURL)
	if err != nil {
		return DestinationWithSecret{}, err
	}
	id, err := ingest.NewUUID()
	if err != nil {
		return DestinationWithSecret{}, err
	}
	secret, sealed, err := s.newDestinationSecret(id)
	if err != nil {
		return DestinationWithSecret{}, err
	}

	var created delivery.Destination
	err = s.pool.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := lockAndAuthorize(ctx, tx, p, orgID, tenancy.PermDestinationsCreate); err != nil {
			return err
		}
		var err error
		created, err = delivery.InsertDestination(ctx, tx, delivery.Destination{
			ID: id, OrganizationID: orgID, Name: cleanName, URL: u.Normalized,
			SigningSecretCiphertext: sealed, EncryptionKeyVersion: s.cfg.Box.Version(), CreatedByUserID: p.User.ID,
		})
		if err != nil {
			return destinationErr(err)
		}
		return audit.Record(ctx, tx, audit.Event{
			OrganizationID: orgID, ActorUserID: p.User.ID, Action: audit.DestinationCreated,
			ResourceType: "destination", ResourceID: id, Metadata: map[string]any{"name": cleanName, "url": u.Display},
		})
	})
	if err != nil {
		return DestinationWithSecret{}, err
	}
	return DestinationWithSecret{Destination: created, SigningSecret: secret}, nil
}

// ListDestinations returns a page of the organization's destinations.
func (s *Service) ListDestinations(ctx context.Context, p Principal, orgID, status string, limit, offset int) ([]delivery.Destination, int, error) {
	q := s.mustQuerier()
	if _, err := authorize(ctx, q, p, orgID, tenancy.PermDestinationsRead); err != nil {
		return nil, 0, err
	}
	switch status {
	case "", delivery.DestinationActive, delivery.DestinationPaused, delivery.DestinationRevoked:
	default:
		return nil, 0, Validation("status must be ACTIVE, PAUSED or REVOKED")
	}
	return delivery.ListDestinations(ctx, q, orgID, status, limit, offset)
}

// GetDestination returns one destination of the organization.
func (s *Service) GetDestination(ctx context.Context, p Principal, orgID, destinationID string) (delivery.Destination, error) {
	q := s.mustQuerier()
	if _, err := authorize(ctx, q, p, orgID, tenancy.PermDestinationsRead); err != nil {
		return delivery.Destination{}, err
	}
	if !IsUUID(destinationID) {
		return delivery.Destination{}, ErrNotFound
	}
	d, err := delivery.DestinationByID(ctx, q, orgID, destinationID, false)
	return d, destinationErr(err)
}

// mutateDestination runs fn on a row-locked, non-revoked destination inside a
// transaction after authorizing perm. The organization lock serializes it with
// every other mutation of the organization, and the row lock with the delivery
// worker's reads of the destination. Revoked destinations are immutable.
func (s *Service) mutateDestination(ctx context.Context, p Principal, orgID, destinationID string, perm tenancy.Permission,
	fn func(tx pgx.Tx, d delivery.Destination) (delivery.Destination, error)) (delivery.Destination, error) {
	var out delivery.Destination
	err := s.pool.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := lockAndAuthorize(ctx, tx, p, orgID, perm); err != nil {
			return err
		}
		if !IsUUID(destinationID) {
			return ErrNotFound
		}
		d, err := delivery.DestinationByID(ctx, tx, orgID, destinationID, true)
		if err != nil {
			return destinationErr(err)
		}
		if d.Status == delivery.DestinationRevoked {
			return ErrDestinationRevoked
		}
		out, err = fn(tx, d)
		return err
	})
	return out, err
}

// DestinationChanges lists the fields UpdateDestination may change. Nil means unchanged.
type DestinationChanges struct {
	Name *string
	URL  *string
}

// UpdateDestination renames a destination and/or changes its URL. A new URL goes
// through the same SSRF-safe validation as at creation. Deliveries that are still
// pending use the new URL on their next attempt; attempts already recorded keep
// their history.
func (s *Service) UpdateDestination(ctx context.Context, p Principal, orgID, destinationID string, ch DestinationChanges) (delivery.Destination, error) {
	if _, err := authorize(ctx, s.mustQuerier(), p, orgID, tenancy.PermDestinationsUpdate); err != nil {
		return delivery.Destination{}, err
	}
	if ch.Name == nil && ch.URL == nil {
		return delivery.Destination{}, Validation("at least one of name or url is required")
	}
	var name string
	var u egress.ValidatedURL
	var err error
	if ch.Name != nil {
		if name, err = NormalizeDestinationName(*ch.Name); err != nil {
			return delivery.Destination{}, err
		}
	}
	if ch.URL != nil {
		if u, err = normalizeDestinationURL(*ch.URL); err != nil {
			return delivery.Destination{}, err
		}
	}
	return s.mutateDestination(ctx, p, orgID, destinationID, tenancy.PermDestinationsUpdate, func(tx pgx.Tx, d delivery.Destination) (delivery.Destination, error) {
		meta := map[string]any{}
		out := d
		if ch.Name != nil && name != d.Name {
			if out, err = delivery.RenameDestination(ctx, tx, orgID, d.ID, name); err != nil {
				return delivery.Destination{}, destinationErr(err)
			}
			meta["name"] = name
		}
		if ch.URL != nil && u.Normalized != d.URL {
			if out, err = delivery.SetDestinationURL(ctx, tx, orgID, d.ID, u.Normalized); err != nil {
				return delivery.Destination{}, destinationErr(err)
			}
			meta["url"] = u.Display // never the query string
		}
		if len(meta) == 0 {
			return out, nil // nothing changed: nothing to audit
		}
		return out, audit.Record(ctx, tx, audit.Event{
			OrganizationID: orgID, ActorUserID: p.User.ID, Action: audit.DestinationUpdated,
			ResourceType: "destination", ResourceID: d.ID, Metadata: meta,
		})
	})
}

func (s *Service) setDestinationStatus(ctx context.Context, p Principal, orgID, destinationID, status, action string) (delivery.Destination, error) {
	return s.mutateDestination(ctx, p, orgID, destinationID, tenancy.PermDestinationsPause, func(tx pgx.Tx, d delivery.Destination) (delivery.Destination, error) {
		if d.Status == status {
			return d, nil // already in the requested state: nothing to change or audit
		}
		out, err := delivery.SetDestinationStatus(ctx, tx, orgID, d.ID, status, s.now())
		if err != nil {
			return delivery.Destination{}, destinationErr(err)
		}
		return out, audit.Record(ctx, tx, audit.Event{
			OrganizationID: orgID, ActorUserID: p.User.ID, Action: action,
			ResourceType: "destination", ResourceID: d.ID, Metadata: map[string]any{"from": d.Status, "to": status},
		})
	})
}

// PauseDestination stops new attempts: the worker does not claim deliveries of a
// paused destination, and they stay PENDING. An attempt already in flight finishes.
func (s *Service) PauseDestination(ctx context.Context, p Principal, orgID, destinationID string) (delivery.Destination, error) {
	return s.setDestinationStatus(ctx, p, orgID, destinationID, delivery.DestinationPaused, audit.DestinationPaused)
}

// ActivateDestination resumes a paused destination. Revoked destinations cannot be activated.
func (s *Service) ActivateDestination(ctx context.Context, p Principal, orgID, destinationID string) (delivery.Destination, error) {
	return s.setDestinationStatus(ctx, p, orgID, destinationID, delivery.DestinationActive, audit.DestinationActivated)
}

// RotateDestinationSecret replaces the signing secret and returns the new one
// exactly once. The new secret is bound to the destination by its associated
// data, secret_version is incremented, and the old secret stops being used for
// the next attempt. Requests already signed with the old secret may still arrive.
func (s *Service) RotateDestinationSecret(ctx context.Context, p Principal, orgID, destinationID string) (DestinationWithSecret, error) {
	if err := s.requireBox(); err != nil {
		return DestinationWithSecret{}, err
	}
	var secret string
	d, err := s.mutateDestination(ctx, p, orgID, destinationID, tenancy.PermDestinationsRotate, func(tx pgx.Tx, d delivery.Destination) (delivery.Destination, error) {
		var sealed []byte
		var err error
		if secret, sealed, err = s.newDestinationSecret(d.ID); err != nil {
			return delivery.Destination{}, err
		}
		out, err := delivery.ReplaceDestinationSecret(ctx, tx, orgID, d.ID, sealed, s.cfg.Box.Version())
		if err != nil {
			return delivery.Destination{}, destinationErr(err)
		}
		return out, audit.Record(ctx, tx, audit.Event{
			OrganizationID: orgID, ActorUserID: p.User.ID, Action: audit.DestinationSecretRotated,
			ResourceType: "destination", ResourceID: d.ID, Metadata: map[string]any{"secretVersion": out.SecretVersion},
		})
	})
	if err != nil {
		return DestinationWithSecret{}, err
	}
	return DestinationWithSecret{Destination: d, SigningSecret: secret}, nil
}

// RevokeDestination permanently disables a destination. The record, its
// subscriptions and its delivery history are kept; PENDING deliveries are
// cancelled in the same transaction (an attempt already in flight is checked by
// the worker before it sends). A revoked destination cannot be reactivated,
// renamed, re-targeted or rotated. Revoking twice is a no-op.
func (s *Service) RevokeDestination(ctx context.Context, p Principal, orgID, destinationID string) (delivery.Destination, error) {
	var out delivery.Destination
	err := s.pool.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := lockAndAuthorize(ctx, tx, p, orgID, tenancy.PermDestinationsRevoke); err != nil {
			return err
		}
		if !IsUUID(destinationID) {
			return ErrNotFound
		}
		d, err := delivery.DestinationByID(ctx, tx, orgID, destinationID, true)
		if err != nil {
			return destinationErr(err)
		}
		if d.Status == delivery.DestinationRevoked {
			out = d
			return nil
		}
		now := s.now()
		if out, err = delivery.SetDestinationStatus(ctx, tx, orgID, d.ID, delivery.DestinationRevoked, now); err != nil {
			return destinationErr(err)
		}
		cancelled, err := delivery.CancelPendingForDestination(ctx, tx, orgID, d.ID, now)
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Event{
			OrganizationID: orgID, ActorUserID: p.User.ID, Action: audit.DestinationRevoked,
			ResourceType: "destination", ResourceID: d.ID, Metadata: map[string]any{"from": d.Status, "cancelledDeliveries": cancelled},
		})
	})
	return out, err
}
