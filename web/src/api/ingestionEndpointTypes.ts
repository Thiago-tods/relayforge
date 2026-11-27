// Read-only types for ingestion endpoints (ADR 14 and the /ingestion-endpoints routes). F3b only reads them, to name
// the endpoint of a subscription and to offer endpoints in a picker. The public id and the ingest path are not kept
// here, and a signing secret is never accepted: it exists only in the create and rotate answers of that API.

export const ENDPOINT_STATUSES = ['ACTIVE', 'PAUSED', 'REVOKED'] as const
export type EndpointStatus = (typeof ENDPOINT_STATUSES)[number]

export interface IngestionEndpoint {
  id: string
  organizationId: string
  name: string
  status: EndpointStatus
  createdAt: string
  updatedAt: string
  /** Set exactly when the endpoint is REVOKED. */
  revokedAt: string | null
}

export interface EndpointPage {
  items: IngestionEndpoint[]
  total: number
  limit: number
  offset: number
}
