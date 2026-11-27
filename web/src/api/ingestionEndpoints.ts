import { request } from './client'
import { ApiError } from './errors'
import { ENDPOINT_STATUSES } from './ingestionEndpointTypes'
import type { EndpointPage, EndpointStatus, IngestionEndpoint } from './ingestionEndpointTypes'

// Read-only access to the existing ingestion endpoint routes. Needs endpoints:read (every role has it).

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

function invalid(field: string): ApiError {
  return new ApiError(
    500,
    'invalid_response',
    `The server returned an invalid ingestion endpoint response (${field}).`,
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

function isStatus(value: unknown): value is EndpointStatus {
  return (ENDPOINT_STATUSES as readonly unknown[]).includes(value)
}

/**
 * Validates one endpoint. `expectedId` ties the answer to the endpoint the request named (null for a list item).
 * A signing secret is rejected: these routes never return one, so its presence means the answer cannot be trusted.
 * An error never contains a value from the body.
 */
export function parseIngestionEndpoint(
  value: unknown,
  orgId: string,
  expectedId: string | null,
): IngestionEndpoint {
  if (!isRecord(value)) throw invalid('endpoint')
  const id = nonEmpty(value.id, 'id')
  if (expectedId !== null && id !== expectedId) throw invalid('id')
  if (nonEmpty(value.organizationId, 'organizationId') !== orgId) throw invalid('organizationId')
  if (!isStatus(value.status)) throw invalid('status')
  const status = value.status
  const revokedAt = value.revokedAt === undefined ? null : timestamp(value.revokedAt, 'revokedAt')
  if ((status === 'REVOKED') !== (revokedAt !== null)) throw invalid('revokedAt')
  if (value.signingSecret !== undefined) throw invalid('signingSecret')
  return {
    id,
    organizationId: orgId,
    name: nonEmpty(value.name, 'name'),
    status,
    createdAt: timestamp(value.createdAt, 'createdAt'),
    updatedAt: timestamp(value.updatedAt, 'updatedAt'),
    revokedAt,
  }
}

export function parseEndpointPage(value: unknown, orgId: string): EndpointPage {
  if (!isRecord(value) || !Array.isArray(value.items)) throw invalid('page')
  const limit = integer(value.limit, 'limit')
  const items = value.items.map((item) => parseIngestionEndpoint(item, orgId, null))
  if (items.length > limit) throw invalid('items exceed limit')
  return {
    items,
    total: integer(value.total, 'total'),
    limit,
    offset: integer(value.offset, 'offset'),
  }
}

function base(orgId: string): string {
  return `/organizations/${encodeURIComponent(orgId)}/ingestion-endpoints`
}

/** One page of endpoints of the organization, newest first (limit 1 to 100). The API has no status filter. */
export async function listIngestionEndpoints(
  orgId: string,
  page: { limit: number; offset: number },
  signal?: AbortSignal,
): Promise<EndpointPage> {
  const query = new URLSearchParams({ limit: String(page.limit), offset: String(page.offset) })
  return parseEndpointPage(
    await request<unknown>(`${base(orgId)}?${query.toString()}`, { signal }),
    orgId,
  )
}

export async function getIngestionEndpoint(
  orgId: string,
  endpointId: string,
  signal?: AbortSignal,
): Promise<IngestionEndpoint> {
  const body = await request<unknown>(`${base(orgId)}/${encodeURIComponent(endpointId)}`, {
    signal,
  })
  if (!isRecord(body)) throw invalid('response')
  return parseIngestionEndpoint(body.endpoint, orgId, endpointId)
}
