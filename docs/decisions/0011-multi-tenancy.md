# 11. Multi-tenancy

Date: 2026-11-17

## Status

Accepted

## Decision

Organizations are independent tenants in a **shared schema**. Every organization-scoped operation:

1. authenticates the session;
2. resolves the organization from the URL;
3. loads the caller's **ACTIVE membership** for that organization (`tenancy.ActiveMembership`);
4. checks the required permission (ADR 0012);
5. runs queries that include the organization ID as a filter or foreign key.

Organization IDs in request bodies are never trusted; invitations and member operations always carry
the organization from the already-authorized path.

**No existence leak.** A malformed ID, an organization that does not exist and an organization the
caller does not belong to all return the same `404 not_found`. A member who lacks a permission gets
`403 forbidden`. Tests assert that foreign and absent responses are byte-identical.

**Scoped writes.** Mutating queries repeat the organization ID (for example revoking an invitation
requires both its ID and the organization), so a valid ID from another tenant cannot be acted on
through your own organization.

**Concurrency.** Operations that change memberships take `SELECT ... FOR UPDATE` on the organization
row first, then read the caller's role. This serializes role changes, removals, invitation
acceptance and ownership transfers per organization, and means a just-demoted caller cannot act on
stale authority.

Users and organizations are never deleted in this phase. Organization deletion is intentionally not
offered: a safe lifecycle (data retention, audit history, webhook resources) needs later phases.
Removing a member marks the membership `REMOVED`, keeping history; the user can be invited again.

## Consequences

Row-level security in PostgreSQL is not used; isolation relies on the application gate above and is
covered by integration tests. Later phases must put tenant-owned tables behind the same gate.
