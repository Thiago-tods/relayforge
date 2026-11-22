import type { Role } from '../api/types'

// A client-side copy of the backend authorization matrix (internal/tenancy/role.go), used ONLY to decide which
// controls to show. The backend stays authoritative: every request is authorized again there, and a 403 always
// wins over what this table says. permissions.drift.test.ts fails when this table and role.go disagree.

export const PERMISSIONS = [
  'organization:view',
  'organization:update',
  'members:list',
  'members:invite',
  'invitations:list',
  'invitations:revoke',
  'members:change_role',
  'members:remove',
  'ownership:transfer',
  'audit:view',
  'endpoints:read',
  'endpoints:create',
  'endpoints:update',
  'endpoints:pause',
  'endpoints:rotate_secret',
  'endpoints:revoke',
  'events:list',
  'events:read_payload',
  'destinations:read',
  'destinations:create',
  'destinations:update',
  'destinations:pause',
  'destinations:rotate_secret',
  'destinations:revoke',
  'subscriptions:read',
  'subscriptions:write',
  'deliveries:read',
  'deliveries:redrive',
] as const

export type Permission = (typeof PERMISSIONS)[number]

const ALL: readonly Permission[] = PERMISSIONS

const DEVELOPER: readonly Permission[] = [
  'organization:view',
  'members:list',
  'endpoints:read',
  'endpoints:create',
  'endpoints:update',
  'endpoints:pause',
  'events:list',
  'events:read_payload',
  'destinations:read',
  'destinations:create',
  'destinations:update',
  'destinations:pause',
  'subscriptions:read',
  'subscriptions:write',
  'deliveries:read',
  'deliveries:redrive',
]

const VIEWER: readonly Permission[] = [
  'organization:view',
  'members:list',
  'endpoints:read',
  'events:list',
  'destinations:read',
  'subscriptions:read',
  'deliveries:read',
]

// OWNER holds every permission. ADMIN holds every permission except ownership:transfer.
export const ROLE_PERMISSIONS: Readonly<Record<Role, ReadonlySet<Permission>>> = {
  OWNER: new Set(ALL),
  ADMIN: new Set(ALL.filter((p) => p !== 'ownership:transfer')),
  DEVELOPER: new Set(DEVELOPER),
  VIEWER: new Set(VIEWER),
}

export function can(role: Role | undefined, permission: Permission): boolean {
  return role !== undefined && ROLE_PERMISSIONS[role].has(permission)
}
