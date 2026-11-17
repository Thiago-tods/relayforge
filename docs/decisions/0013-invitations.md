# 13. Invitation model

Date: 2026-11-17

## Status

Accepted

## Context

No email provider can be paid for, so invitations cannot be delivered automatically.

## Decision

- `POST /organizations/{id}/invitations` creates an invitation for an email address and a role
  (ADMIN, DEVELOPER or VIEWER, within the inviter's limits) and returns the raw token **once** in the
  response. The caller is responsible for delivering it securely (for example through a channel the
  invitee trusts). The server never emails it, never logs it and cannot show it again.
- The token is 256 bits from `crypto/rand`; only its SHA-256 hash is stored.
- Invitations expire after 7 days, can be revoked, and can be accepted once.
- Acceptance (`POST /invitations/accept`) requires an authenticated user whose **normalized email
  equals the invited email**, then, in one transaction: locks the invitation row, rechecks state,
  locks the organization, marks the invitation accepted, creates (or reactivates) the membership and
  writes the audit event.
- Unknown, expired, revoked, used and wrong-recipient invitations all return the same
  `400 invalid_invitation`, so the endpoint cannot be used to probe tokens or recipients. The precise
  reason is kept in the error chain for tests and logs.
- At most one pending invitation exists per organization and email (partial unique index). An
  expired pending invitation is revoked automatically when a new one is created.
- Inviting someone who is already an active member returns `409 already_member`.
- Pending invitations are listed without tokens; accepted and revoked ones are not listed.

## Not implemented

Automated email delivery, resending, and invitation links. A future phase can add delivery without
changing the storage model.
