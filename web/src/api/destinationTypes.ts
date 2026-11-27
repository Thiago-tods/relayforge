// Destination management types (ADR 27). They describe what the API returns for a destination: the URL is always
// the display form without its query string, and the signing secret exists only in the create and rotate answers.

export const DESTINATION_STATUSES = ['ACTIVE', 'PAUSED', 'REVOKED'] as const
export type DestinationStatus = (typeof DESTINATION_STATUSES)[number]

export interface Destination {
  id: string
  organizationId: string
  name: string
  status: DestinationStatus
  /** The URL without its query string. The original query is never available to the client. */
  url: string
  /** True when the stored URL has a query string that the API hides. */
  urlHasQuery: boolean
  createdByUserId: string
  createdAt: string
  updatedAt: string
  /** Set exactly when the destination is REVOKED. */
  revokedAt: string | null
}

export interface DestinationRecordFilters {
  limit: number
  offset: number
  status?: DestinationStatus | undefined
}

/** One page of destinations with the exact total of the filtered set (offset pagination). */
export interface DestinationRecordPage {
  items: Destination[]
  total: number
  limit: number
  offset: number
}

export interface CreateDestinationInput {
  name: string
  url: string
}

/** At least one field. A URL is always sent in full: the client never sees the stored query, so it cannot edit it. */
export interface DestinationChanges {
  name?: string
  url?: string
}

/** A destination together with its raw signing secret, shown once (create and rotate only). */
export interface DestinationWithSecret {
  destination: Destination
  signingSecret: string
}
