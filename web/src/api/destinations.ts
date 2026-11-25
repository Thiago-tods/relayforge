import { request } from './client'
import { UNREADABLE_SUCCESS } from './deliveries'
import { DESTINATION_STATUSES } from './destinationTypes'
import type {
  CreateDestinationInput,
  Destination,
  DestinationChanges,
  DestinationRecordFilters,
  DestinationRecordPage,
  DestinationStatus,
  DestinationWithSecret,
} from './destinationTypes'
import { ApiError, isApiError } from './errors'
import { parseDestinationPage } from './operationsParse'
import type { DestinationPage } from './operationsTypes'

/**
 * One page of the organization's destinations, newest first (limit 1 to 100, offset-based, with an exact total).
 * Used only to name a destination in a filter: the result carries id, name and status, never the URL.
 */
export async function listDestinations(
  orgId: string,
  page: { limit: number; offset: number },
  signal?: AbortSignal,
): Promise<DestinationPage> {
  const query = new URLSearchParams({ limit: String(page.limit), offset: String(page.offset) })
  const body = await request<unknown>(
    `/organizations/${encodeURIComponent(orgId)}/destinations?${query.toString()}`,
    { signal },
  )
  return parseDestinationPage(body)
}

// ---------------------------------------------------------------------------------------------------------------
// Destination management (Phase F3a, ADR 27). The function above belongs to F1 and stays as it is.
// ---------------------------------------------------------------------------------------------------------------

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

function invalidDestination(field: string): ApiError {
  return new ApiError(
    500,
    'invalid_response',
    `The server returned an invalid destination response (${field}).`,
  )
}

/**
 * An HTTP 2xx whose body cannot be trusted. The operation may have succeeded, so callers must treat it as an
 * ambiguous outcome and must never repeat it on their own.
 */
function unreadableDestination(field: string): ApiError {
  return new ApiError(
    200,
    UNREADABLE_SUCCESS,
    `The server accepted the request but sent a response that could not be read (${field}).`,
  )
}

function timestamp(value: unknown, field: string): string {
  if (typeof value !== 'string' || value === '' || Number.isNaN(Date.parse(value))) {
    throw invalidDestination(field)
  }
  return value
}

function nonEmpty(value: unknown, field: string): string {
  if (typeof value !== 'string' || value === '') throw invalidDestination(field)
  return value
}

function isStatus(value: unknown): value is DestinationStatus {
  return (DESTINATION_STATUSES as readonly unknown[]).includes(value)
}

type SecretRule = 'absent' | 'required'

/**
 * Validates one destination. `expectedId` ties the answer to the destination the request named (null for a list
 * item or a creation). The signing secret must be absent everywhere except the create and rotate answers, where it
 * is required. An error never contains a value from the body.
 */
export function parseDestination(
  value: unknown,
  orgId: string,
  expectedId: string | null,
  secret: SecretRule = 'absent',
): { destination: Destination; signingSecret: string | null } {
  if (!isRecord(value)) throw invalidDestination('destination')
  const id = nonEmpty(value.id, 'id')
  if (expectedId !== null && id !== expectedId) throw invalidDestination('id')
  if (nonEmpty(value.organizationId, 'organizationId') !== orgId) {
    throw invalidDestination('organizationId')
  }
  if (!isStatus(value.status)) throw invalidDestination('status')
  const status = value.status
  const url = nonEmpty(value.url, 'url')
  // The API shows the URL without its query (and a URL never has a fragment): a URL that still has either would
  // expose something the contract hides.
  if (url.includes('?') || url.includes('#')) throw invalidDestination('url')
  if (typeof value.urlHasQuery !== 'boolean') throw invalidDestination('urlHasQuery')
  const revokedAt = value.revokedAt === undefined ? null : timestamp(value.revokedAt, 'revokedAt')
  if ((status === 'REVOKED') !== (revokedAt !== null)) throw invalidDestination('revokedAt')

  let signingSecret: string | null = null
  if (secret === 'required') signingSecret = nonEmpty(value.signingSecret, 'signingSecret')
  else if (value.signingSecret !== undefined) throw invalidDestination('signingSecret')

  return {
    destination: {
      id,
      organizationId: orgId,
      name: nonEmpty(value.name, 'name'),
      status,
      url,
      urlHasQuery: value.urlHasQuery,
      createdByUserId: nonEmpty(value.createdByUserId, 'createdByUserId'),
      createdAt: timestamp(value.createdAt, 'createdAt'),
      updatedAt: timestamp(value.updatedAt, 'updatedAt'),
      revokedAt,
    },
    signingSecret,
  }
}

function integer(value: unknown, field: string): number {
  if (typeof value !== 'number' || !Number.isInteger(value) || value < 0) {
    throw invalidDestination(field)
  }
  return value
}

export function parseDestinationRecordPage(value: unknown, orgId: string): DestinationRecordPage {
  if (!isRecord(value) || !Array.isArray(value.items)) throw invalidDestination('page')
  const limit = integer(value.limit, 'limit')
  const items = value.items.map((item) => parseDestination(item, orgId, null).destination)
  if (items.length > limit) throw invalidDestination('items exceed limit')
  return {
    items,
    total: integer(value.total, 'total'),
    limit,
    offset: integer(value.offset, 'offset'),
  }
}

function base(orgId: string): string {
  return `/organizations/${encodeURIComponent(orgId)}/destinations`
}

function one(orgId: string, destinationId: string): string {
  return `${base(orgId)}/${encodeURIComponent(destinationId)}`
}

/** One page of full destination records, newest first (offset pagination, limit 1 to 100, optional status). */
export async function listDestinationRecords(
  orgId: string,
  filters: DestinationRecordFilters,
  signal?: AbortSignal,
): Promise<DestinationRecordPage> {
  const query = new URLSearchParams({
    limit: String(filters.limit),
    offset: String(filters.offset),
  })
  if (filters.status !== undefined) query.set('status', filters.status)
  return parseDestinationRecordPage(
    await request<unknown>(`${base(orgId)}?${query.toString()}`, { signal }),
    orgId,
  )
}

export async function getDestination(
  orgId: string,
  destinationId: string,
  signal?: AbortSignal,
): Promise<Destination> {
  const body = await request<unknown>(one(orgId, destinationId), { signal })
  if (!isRecord(body)) throw invalidDestination('response')
  return parseDestination(body.destination, orgId, destinationId).destination
}

/**
 * Sends one mutation and parses the `{destination}` answer. A success whose body cannot be read or does not match
 * the request is reported as `unreadable_success` (the operation may have been applied). The request is never
 * repeated here; the client's own single retry after csrf_invalid is the only repeat.
 */
async function mutate(
  path: string,
  method: 'POST' | 'PATCH' | 'DELETE',
  body: unknown,
  orgId: string,
  destinationId: string | null,
  secret: SecretRule,
  accept: (destination: Destination) => boolean,
  signal?: AbortSignal,
): Promise<{ destination: Destination; signingSecret: string | null }> {
  let raw: unknown
  try {
    raw = await request<unknown>(path, { method, body, signal })
  } catch (error) {
    // res.json() of an HTTP 2xx whose body is not JSON.
    if (error instanceof SyntaxError) throw unreadableDestination('body')
    throw error
  }
  try {
    if (!isRecord(raw) || Object.keys(raw).length !== 1) throw invalidDestination('response')
    const parsed = parseDestination(raw.destination, orgId, destinationId, secret)
    if (!accept(parsed.destination)) throw invalidDestination('status')
    return parsed
  } catch (error) {
    if (isApiError(error) && error.code === 'invalid_response') {
      throw unreadableDestination('destination')
    }
    throw error
  }
}

/** Creates an ACTIVE destination. The answer carries the signing secret, which the server never shows again. */
export async function createDestination(
  orgId: string,
  input: CreateDestinationInput,
  signal?: AbortSignal,
): Promise<DestinationWithSecret> {
  const result = await mutate(
    base(orgId),
    'POST',
    { name: input.name, url: input.url },
    orgId,
    null,
    'required',
    (d) => d.status === 'ACTIVE',
    signal,
  )
  return { destination: result.destination, signingSecret: result.signingSecret ?? '' }
}

export async function updateDestination(
  orgId: string,
  destinationId: string,
  changes: DestinationChanges,
  signal?: AbortSignal,
): Promise<Destination> {
  const body: DestinationChanges = {}
  if (changes.name !== undefined) body.name = changes.name
  if (changes.url !== undefined) body.url = changes.url
  const result = await mutate(
    one(orgId, destinationId),
    'PATCH',
    body,
    orgId,
    destinationId,
    'absent',
    (d) => d.status !== 'REVOKED',
    signal,
  )
  return result.destination
}

export async function pauseDestination(
  orgId: string,
  destinationId: string,
  signal?: AbortSignal,
): Promise<Destination> {
  const result = await mutate(
    `${one(orgId, destinationId)}/pause`,
    'POST',
    undefined,
    orgId,
    destinationId,
    'absent',
    (d) => d.status === 'PAUSED',
    signal,
  )
  return result.destination
}

export async function activateDestination(
  orgId: string,
  destinationId: string,
  signal?: AbortSignal,
): Promise<Destination> {
  const result = await mutate(
    `${one(orgId, destinationId)}/activate`,
    'POST',
    undefined,
    orgId,
    destinationId,
    'absent',
    (d) => d.status === 'ACTIVE',
    signal,
  )
  return result.destination
}

/** Replaces the signing secret. The new secret is in the answer only; a lost answer means a lost secret. */
export async function rotateDestinationSecret(
  orgId: string,
  destinationId: string,
  signal?: AbortSignal,
): Promise<DestinationWithSecret> {
  const result = await mutate(
    `${one(orgId, destinationId)}/rotate-secret`,
    'POST',
    undefined,
    orgId,
    destinationId,
    'required',
    (d) => d.status !== 'REVOKED',
    signal,
  )
  return { destination: result.destination, signingSecret: result.signingSecret ?? '' }
}

/** Permanently revokes a destination (DELETE; the answer is the revoked destination, not an empty 204). */
export async function revokeDestination(
  orgId: string,
  destinationId: string,
  signal?: AbortSignal,
): Promise<Destination> {
  const result = await mutate(
    one(orgId, destinationId),
    'DELETE',
    undefined,
    orgId,
    destinationId,
    'absent',
    (d) => d.status === 'REVOKED',
    signal,
  )
  return result.destination
}
