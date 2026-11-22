import { request } from './client'
import type { QueueAge, QueueSnapshot } from './operationsTypes'

// Operational read models (ADR 31 and 32). Read-only, organization-scoped, need deliveries:read.

function base(orgId: string): string {
  return `/organizations/${encodeURIComponent(orgId)}/operations`
}

export function getQueue(orgId: string, signal?: AbortSignal): Promise<QueueSnapshot> {
  return request<QueueSnapshot>(`${base(orgId)}/queue`, { signal })
}

export function getQueueAge(orgId: string, signal?: AbortSignal): Promise<QueueAge> {
  return request<QueueAge>(`${base(orgId)}/queue/age`, { signal })
}
