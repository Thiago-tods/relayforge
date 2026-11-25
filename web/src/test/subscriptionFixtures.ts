// Mocked subscription and ingestion endpoint data for tests. The shape follows ADR 26 and 27
// (internal/delivery/subscription_read.go, internal/ingest/store.go): the subscription answer has the endpoint id
// and the destination's name and status, but not the endpoint's name.

export const SUB_ID = 'cccccccc-cccc-4ccc-8ccc-cccccccccccc'
export const SUB_ID_2 = 'dddddddd-dddd-4ddd-8ddd-dddddddddddd'
export const EP_ID = 'eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee'
export const EP_ID_2 = 'ffffffff-ffff-4fff-8fff-ffffffffffff'
export const DEST_ID = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa'
export const DEST_ID_2 = 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb'

export function subscriptionBody(over: Record<string, unknown> = {}) {
  return {
    id: SUB_ID,
    organizationId: 'o1',
    endpointId: EP_ID,
    destinationId: DEST_ID,
    destinationName: 'Billing hook',
    destinationStatus: 'ACTIVE',
    status: 'ACTIVE',
    createdByUserId: 'u-1',
    createdAt: '2026-11-20T10:00:00Z',
    ...over,
  }
}

export function revokedSubscriptionBody(over: Record<string, unknown> = {}) {
  return subscriptionBody({ status: 'REVOKED', revokedAt: '2026-11-22T09:00:00Z', ...over })
}

export function subscriptionList(
  items: Record<string, unknown>[],
  total = items.length,
  offset = 0,
  limit = 25,
) {
  return { items, total, limit, offset }
}

/** An ingestion endpoint as the API returns it, including the public fields F3b ignores. */
export function endpointBody(over: Record<string, unknown> = {}) {
  return {
    id: EP_ID,
    organizationId: 'o1',
    publicId: 'pub_abc123',
    name: 'Orders intake',
    status: 'ACTIVE',
    createdByUserId: 'u-1',
    createdAt: '2026-11-19T10:00:00Z',
    updatedAt: '2026-11-19T10:00:00Z',
    ingestPath: '/api/v1/ingest/pub_abc123',
    ...over,
  }
}

export function endpointList(
  items: Record<string, unknown>[],
  total = items.length,
  offset = 0,
  limit = 100,
) {
  return { items, total, limit, offset }
}
