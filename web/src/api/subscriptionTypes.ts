import type { DestinationStatus } from './destinationTypes'

// Subscription management types (ADR 26 and 27). A subscription is one lifecycle interval [createdAt, revokedAt)
// linking an ingestion endpoint to a destination. Its `status` describes only that interval: it is independent of
// the destination's status, which the API exposes separately as `destinationStatus`. The API does not return the
// endpoint's name or status, so the UI looks the endpoint up separately.

export const SUBSCRIPTION_STATUSES = ['ACTIVE', 'REVOKED'] as const
export type SubscriptionStatus = (typeof SUBSCRIPTION_STATUSES)[number]

export interface Subscription {
  id: string
  organizationId: string
  endpointId: string
  destinationId: string
  /** The destination's current name and status (a snapshot at read time, not part of the subscription). */
  destinationName: string
  destinationStatus: DestinationStatus
  status: SubscriptionStatus
  createdAt: string
  /** Set exactly when the subscription is REVOKED. The interval is [createdAt, revokedAt). */
  revokedAt: string | null
}

export interface SubscriptionFilters {
  limit: number
  offset: number
  status?: SubscriptionStatus | undefined
  endpointId?: string | undefined
  destinationId?: string | undefined
}

/** One page of subscriptions, newest first, with the exact total of the filtered set (offset pagination). */
export interface SubscriptionPage {
  items: Subscription[]
  total: number
  limit: number
  offset: number
}

export interface CreateSubscriptionInput {
  endpointId: string
  destinationId: string
}
