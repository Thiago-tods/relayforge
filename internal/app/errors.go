// Package app implements RelayForge's use cases: registration, authentication,
// organizations, memberships, invitations and audit queries. It owns the
// transaction boundaries and the authorization checks; HTTP concerns live in
// internal/httpapi.
package app

import (
	"net/http"

	"github.com/Thiago-tods/relayforge/internal/platform/apperror"
)

// Errors returned by the service. They carry the HTTP status and a stable code,
// so handlers render them directly. Messages are safe to show to clients.
var (
	ErrUnauthenticated = apperror.New(http.StatusUnauthorized, "unauthenticated", "authentication is required")
	// ErrInvalidCredentials is used for every login failure so the response never
	// reveals whether the email address exists.
	ErrInvalidCredentials = apperror.New(http.StatusUnauthorized, "invalid_credentials", "invalid email or password")
	ErrForbidden          = apperror.New(http.StatusForbidden, "forbidden", "you do not have permission to perform this action")
	// ErrNotFound is returned both for resources that do not exist and for
	// organizations the caller is not a member of.
	ErrNotFound         = apperror.New(http.StatusNotFound, "not_found", "resource not found")
	ErrMemberNotFound   = apperror.New(http.StatusNotFound, "member_not_found", "member not found")
	ErrEmailTaken       = apperror.New(http.StatusConflict, "email_taken", "an account with this email already exists")
	ErrSlugTaken        = apperror.New(http.StatusConflict, "slug_taken", "this organization slug is already taken")
	ErrAlreadyMember    = apperror.New(http.StatusConflict, "already_member", "the user is already a member of this organization")
	ErrInvitationExists = apperror.New(http.StatusConflict, "invitation_exists", "a pending invitation already exists for this email")
	ErrSelfModification = apperror.New(http.StatusConflict, "cannot_modify_self", "you cannot change or remove your own membership")
	ErrUseTransfer      = apperror.New(http.StatusConflict, "use_ownership_transfer", "the owner role can only be changed with an ownership transfer")
	ErrAlreadyOwner     = apperror.New(http.StatusConflict, "already_owner", "the target member is already the owner")
	// ErrInvalidInvitation is returned for unknown, expired, revoked, used and
	// wrong-recipient invitations alike. The wrapped cause tells them apart for
	// tests and logs but is never sent to clients.
	ErrInvalidInvitation = apperror.New(http.StatusBadRequest, "invalid_invitation", "the invitation is invalid, expired or already used")
)

// Reasons wrapped inside ErrInvalidInvitation. Tests and logs can tell them
// apart with errors.Is; clients cannot.
var (
	ReasonInvitationUnknown   = apperror.New(0, "unknown", "invitation token not recognized")
	ReasonInvitationExpired   = apperror.New(0, "expired", "invitation expired")
	ReasonInvitationRevoked   = apperror.New(0, "revoked", "invitation revoked")
	ReasonInvitationUsed      = apperror.New(0, "used", "invitation already accepted")
	ReasonInvitationRecipient = apperror.New(0, "wrong_recipient", "invitation addressed to another email")
)

func invalidInvitation(reason *apperror.Error) error {
	return apperror.Wrap(ErrInvalidInvitation.Status, ErrInvalidInvitation.Code, ErrInvalidInvitation.Message, reason)
}

// Validation builds a 400 error describing a client input problem.
func Validation(message string) *apperror.Error {
	return apperror.New(http.StatusBadRequest, "validation_failed", message)
}
