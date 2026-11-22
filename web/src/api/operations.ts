import { request } from './client'
import { parseAttemptFeed, parseDeliverySummary, parseFailureFeed } from './operationsParse'
import type {
  AttemptFeed,
  DeliverySummary,
  FailureFeed,
  QueueAge,
  QueueSnapshot,
} from './operationsTypes'

// Operational read models (ADR 31, 32 and 33). Read-only, organization-scoped, need deliveries:read.

function base(orgId: string): string {
  return `/organizations/${encodeURIComponent(orgId)}/operations`
}

/** A query string from the parameters that have a value, in the order given. Nothing else is ever sent. */
function query(params: [string, string | number | undefined][]): string {
  const search = new URLSearchParams()
  for (const [name, value] of params) {
    if (value !== undefined && value !== '') search.set(name, String(value))
  }
  const text = search.toString()
  return text === '' ? '' : `?${text}`
}

export function getQueue(orgId: string, signal?: AbortSignal): Promise<QueueSnapshot> {
  return request<QueueSnapshot>(`${base(orgId)}/queue`, { signal })
}

export function getQueueAge(orgId: string, signal?: AbortSignal): Promise<QueueAge> {
  return request<QueueAge>(`${base(orgId)}/queue/age`, { signal })
}

/** A window of UTC instants (RFC 3339). Both are omitted to get the backend's default window. */
export interface WindowParams {
  from?: string | undefined
  to?: string | undefined
}

export async function getDeliverySummary(
  orgId: string,
  window: WindowParams,
  signal?: AbortSignal,
): Promise<DeliverySummary> {
  const path = `${base(orgId)}/deliveries${query([
    ['from', window.from],
    ['to', window.to],
  ])}`
  return parseDeliverySummary(await request<unknown>(path, { signal }))
}

export interface FailureParams extends WindowParams {
  limit?: number | undefined
  /** An opaque cursor the server issued for this same query. */
  cursor?: string | null | undefined
  failureReason?: string | undefined
  destinationId?: string | undefined
}

export async function getFailures(
  orgId: string,
  params: FailureParams,
  signal?: AbortSignal,
): Promise<FailureFeed> {
  const path = `${base(orgId)}/failures${query([
    ['limit', params.limit],
    ['cursor', params.cursor ?? undefined],
    ['failureReason', params.failureReason],
    ['destinationId', params.destinationId],
    ['from', params.from],
    ['to', params.to],
  ])}`
  return parseFailureFeed(await request<unknown>(path, { signal }))
}

/** There is deliberately no destination filter: the attempt feed does not support one. */
export interface AttemptParams extends WindowParams {
  limit?: number | undefined
  cursor?: string | null | undefined
  outcome?: string | undefined
  deliveryId?: string | undefined
}

export async function getAttempts(
  orgId: string,
  params: AttemptParams,
  signal?: AbortSignal,
): Promise<AttemptFeed> {
  const path = `${base(orgId)}/attempts${query([
    ['limit', params.limit],
    ['cursor', params.cursor ?? undefined],
    ['outcome', params.outcome],
    ['deliveryId', params.deliveryId],
    ['from', params.from],
    ['to', params.to],
  ])}`
  return parseAttemptFeed(await request<unknown>(path, { signal }))
}
