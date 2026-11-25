import { request } from './client'
import { UNREADABLE_SUCCESS } from './deliveries'
import { DESTINATION_STATUSES } from './destinationTypes'
import type { DestinationStatus } from './destinationTypes'
import { ApiError, isApiError } from './errors'
import { SUBSCRIPTION_STATUSES } from './subscriptionTypes'
import type {
  CreateSubscriptionInput,
  Subscription,
  SubscriptionFilters,
  SubscriptionPage,
  SubscriptionStatus,
} from './subscriptionTypes'

// Subscription management (ADR 26 and 27). Every function takes the organization id explicitly and validates that
// the answer belongs to it. Mutations are sent once: a success whose body cannot be trusted is reported as
// `unreadable_success` (the operation may have been applied) and callers must read the state, never repeat it.

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

function invalid(field: string): ApiError {
  return new ApiError(
    500,
    'invalid_response',
    `The server returned an invalid subscription response (${field}).`,
  )
}

function unreadable(field: string): ApiError {
  return new ApiError(
    200,
    UNREADABLE_SUCCESS,
    `The server accepted the request but sent a response that could not be read (${field}).`,
  )
}

function nonEmpty(value: unknown, field: string): string {
  if (typeof value !== 'string' || value === '') throw invalid(field)
  return value
}

function timestamp(value: unknown, field: string): string {
  if (typeof value !== 'string' || value === '' || Number.isNaN(Date.parse(value))) {
    throw invalid(field)
  }
  return value
}

function integer(value: unknown, field: string): number {
  if (typeof value !== 'number' || !Number.isInteger(value) || value < 0) throw invalid(field)
  return value
}

function isDestinationStatus(value: unknown): value is DestinationStatus {
  return (DESTINATION_STATUSES as readonly unknown[]).includes(value)
}

function isSubscriptionStatus(value: unknown): value is SubscriptionStatus {
  return (SUBSCRIPTION_STATUSES as readonly unknown[]).includes(value)
}

/**
 * Validates one subscription. `expectedId` ties the answer to the subscription the request named (null for a list
 * item or a creation). The subscription status and the destination status are validated separately and never
 * derived from each other. `createdByUserId` is required by the contract but not kept: the UI does not show it.
 * An error never contains a value from the body.
 */
export function parseSubscription(
  value: unknown,
  orgId: string,
  expectedId: string | null,
): Subscription {
  if (!isRecord(value)) throw invalid('subscription')
  const id = nonEmpty(value.id, 'id')
  if (expectedId !== null && id !== expectedId) throw invalid('id')
  if (nonEmpty(value.organizationId, 'organizationId') !== orgId) throw invalid('organizationId')
  if (!isSubscriptionStatus(value.status)) throw invalid('status')
  const status = value.status
  if (!isDestinationStatus(value.destinationStatus)) throw invalid('destinationStatus')
  const createdAt = timestamp(value.createdAt, 'createdAt')
  const revokedAt = value.revokedAt === undefined ? null : timestamp(value.revokedAt, 'revokedAt')
  if ((status === 'REVOKED') !== (revokedAt !== null)) throw invalid('revokedAt')
  // The interval is [createdAt, revokedAt): it cannot end before it starts.
  if (revokedAt !== null && Date.parse(revokedAt) < Date.parse(createdAt))
    throw invalid('revokedAt')
  nonEmpty(value.createdByUserId, 'createdByUserId')
  return {
    id,
    organizationId: orgId,
    endpointId: nonEmpty(value.endpointId, 'endpointId'),
    destinationId: nonEmpty(value.destinationId, 'destinationId'),
    destinationName: nonEmpty(value.destinationName, 'destinationName'),
    destinationStatus: value.destinationStatus,
    status,
    createdAt,
    revokedAt,
  }
}

export function parseSubscriptionPage(value: unknown, orgId: string): SubscriptionPage {
  if (!isRecord(value) || !Array.isArray(value.items)) throw invalid('page')
  const limit = integer(value.limit, 'limit')
  const items = value.items.map((item) => parseSubscription(item, orgId, null))
  if (items.length > limit) throw invalid('items exceed limit')
  return {
    items,
    total: integer(value.total, 'total'),
    limit,
    offset: integer(value.offset, 'offset'),
  }
}

function base(orgId: string): string {
  return `/organizations/${encodeURIComponent(orgId)}/subscriptions`
}

function one(orgId: string, subscriptionId: string): string {
  return `${base(orgId)}/${encodeURIComponent(subscriptionId)}`
}

/** One page of subscriptions, newest first (offset pagination, limit 1 to 100, optional filters). */
export async function listSubscriptions(
  orgId: string,
  filters: SubscriptionFilters,
  signal?: AbortSignal,
): Promise<SubscriptionPage> {
  const query = new URLSearchParams({
    limit: String(filters.limit),
    offset: String(filters.offset),
  })
  if (filters.status !== undefined) query.set('status', filters.status)
  if (filters.endpointId !== undefined) query.set('endpointId', filters.endpointId)
  if (filters.destinationId !== undefined) query.set('destinationId', filters.destinationId)
  return parseSubscriptionPage(
    await request<unknown>(`${base(orgId)}?${query.toString()}`, { signal }),
    orgId,
  )
}

export async function getSubscription(
  orgId: string,
  subscriptionId: string,
  signal?: AbortSignal,
): Promise<Subscription> {
  const body = await request<unknown>(one(orgId, subscriptionId), { signal })
  if (!isRecord(body)) throw invalid('response')
  return parseSubscription(body.subscription, orgId, subscriptionId)
}

/**
 * Sends one mutation and parses the `{subscription}` answer. A success whose body cannot be read or does not match
 * the request is `unreadable_success`. The request is never repeated here; the client's own single retry after
 * csrf_invalid (which the backend rejects before the handler runs) is the only repeat.
 */
async function mutate(
  path: string,
  method: 'POST' | 'DELETE',
  body: unknown,
  orgId: string,
  subscriptionId: string | null,
  accept: (subscription: Subscription) => boolean,
  signal?: AbortSignal,
): Promise<Subscription> {
  let raw: unknown
  try {
    raw = await request<unknown>(path, { method, body, signal })
  } catch (error) {
    // res.json() of an HTTP 2xx whose body is not JSON.
    if (error instanceof SyntaxError) throw unreadable('body')
    throw error
  }
  try {
    if (!isRecord(raw) || Object.keys(raw).length !== 1) throw invalid('response')
    const parsed = parseSubscription(raw.subscription, orgId, subscriptionId)
    if (!accept(parsed)) throw invalid('subscription')
    return parsed
  } catch (error) {
    if (isApiError(error) && error.code === 'invalid_response') throw unreadable('subscription')
    throw error
  }
}

/**
 * Starts a new subscription interval (always a new record, never a reactivation). The answer must be an ACTIVE
 * subscription for exactly the requested endpoint and destination.
 */
export async function createSubscription(
  orgId: string,
  input: CreateSubscriptionInput,
  signal?: AbortSignal,
): Promise<Subscription> {
  return mutate(
    base(orgId),
    'POST',
    { endpointId: input.endpointId, destinationId: input.destinationId },
    orgId,
    null,
    (s) =>
      s.status === 'ACTIVE' &&
      s.endpointId === input.endpointId &&
      s.destinationId === input.destinationId,
    signal,
  )
}

/** Permanently ends a subscription (DELETE; the answer is the revoked subscription, not an empty 204). */
export async function revokeSubscription(
  orgId: string,
  subscriptionId: string,
  signal?: AbortSignal,
): Promise<Subscription> {
  return mutate(
    one(orgId, subscriptionId),
    'DELETE',
    undefined,
    orgId,
    subscriptionId,
    (s) => s.status === 'REVOKED',
    signal,
  )
}
