// Mocked destination data for tests. The shape follows ADR 27 and internal/httpapi/delivery_api.go: the URL is the
// display form without its query, and signingSecret exists only in create and rotate answers.

export const DEST_ID = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa'
export const DEST_ID_2 = 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb'
export const SECRET = 'rfd_TESTONLY_not_a_real_secret_0123456789'

export function destinationBody(over: Record<string, unknown> = {}) {
  return {
    id: DEST_ID,
    organizationId: 'o1',
    name: 'Billing hook',
    status: 'ACTIVE',
    url: 'https://hooks.example.com/billing',
    urlHasQuery: false,
    createdByUserId: 'u-1',
    createdAt: '2026-11-20T10:00:00Z',
    updatedAt: '2026-11-21T11:30:00Z',
    ...over,
  }
}

export function revokedBody(over: Record<string, unknown> = {}) {
  return destinationBody({ status: 'REVOKED', revokedAt: '2026-11-22T09:00:00Z', ...over })
}

export function destinationList(
  items: Record<string, unknown>[],
  total = items.length,
  offset = 0,
  limit = 25,
) {
  return { items, total, limit, offset }
}
