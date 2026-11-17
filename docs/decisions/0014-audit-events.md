# 14. Audit events

Date: 2026-11-17

## Status

Accepted

## Decision

`audit_events` records security-sensitive and administrative actions: `user.registered`,
`user.login.succeeded`, `user.logout`, `organization.created`, `organization.updated`,
`organization.invitation.created`, `.revoked` and `.accepted`, `organization.member.role_changed`,
`organization.member.removed` and `organization.ownership.transferred`.

- **Same transaction.** Each event is written with the transaction that performs the mutation
  (`audit.Record` takes the transaction), so a change and its event commit or roll back together. A
  test forces the audit insert to fail and verifies the mutation is rolled back.
- **Append-only.** Application code only inserts. A database trigger rejects `UPDATE` and `DELETE`,
  so a bug or injected statement cannot rewrite history. A superuser can still disable the trigger;
  this is tamper resistance against application defects, not against a database administrator.
- **No secrets.** Metadata is a small structured object (target user ID, old and new role, slug,
  invited email and role). Passwords, hashes, session, CSRF and invitation tokens and request bodies
  are never recorded; a test scans the table for them.
- **Tenant isolation.** `GET /organizations/{id}/audit-events` is permission-gated (OWNER and ADMIN)
  and the query filters by organization. Events without an organization (registration, login,
  logout) are never returned by it.
- **Failed logins are not audited.** Without rate limiting, any anonymous caller could grow the table
  without bound. Recording failures will be revisited together with login throttling.

## Consequences

The log is queryable by organization and time through an index. Retention and export are future work.
