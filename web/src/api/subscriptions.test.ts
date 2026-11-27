import { describe, expect, it } from 'vitest'
import { setCsrfToken } from './client'
import { UNREADABLE_SUCCESS } from './deliveries'
import { isApiError } from './errors'
import {
  createSubscription,
  getSubscription,
  listSubscriptions,
  parseSubscription,
  revokeSubscription,
} from './subscriptions'
import { apiError, json, stubFetch } from '../test/fetchStub'
import {
  DEST_ID,
  DEST_ID_2,
  EP_ID,
  EP_ID_2,
  revokedSubscriptionBody,
  SUB_ID,
  SUB_ID_2,
  subscriptionBody,
  subscriptionList,
} from '../test/subscriptionFixtures'

// A stubbed fetch: these tests prove how the client builds requests and parses answers, not the real backend.

const ORG = 'o1'
const BASE = `/api/v1/organizations/${ORG}/subscriptions`
const ONE = `${BASE}/${SUB_ID}`
const INPUT = { endpointId: EP_ID, destinationId: DEST_ID }

async function rejection(promise: Promise<unknown>): Promise<unknown> {
  return promise.then(
    () => {
      throw new Error('expected a rejection')
    },
    (error: unknown) => error,
  )
}

describe('listSubscriptions', () => {
  it('asks for one page with limit, offset and only the filters that are set', async () => {
    const calls = stubFetch(() => json(200, subscriptionList([subscriptionBody()], 1)))
    await listSubscriptions(ORG, {
      limit: 25,
      offset: 50,
      status: 'REVOKED',
      endpointId: EP_ID,
      destinationId: DEST_ID,
    })
    const url = new URL(calls[0]!.path, 'http://x')
    expect(url.pathname).toBe(BASE)
    expect(Object.fromEntries(url.searchParams)).toEqual({
      limit: '25',
      offset: '50',
      status: 'REVOKED',
      endpointId: EP_ID,
      destinationId: DEST_ID,
    })
    expect(calls[0]!.method).toBe('GET')
  })

  it('sends no filter parameters for the unfiltered list', async () => {
    const calls = stubFetch(() => json(200, subscriptionList([])))
    await listSubscriptions(ORG, { limit: 25, offset: 0 })
    expect([...new URL(calls[0]!.path, 'http://x').searchParams.keys()]).toEqual([
      'limit',
      'offset',
    ])
  })

  it('parses items, the exact total and the page window, keeping both statuses apart', async () => {
    stubFetch(() =>
      json(
        200,
        subscriptionList(
          [
            subscriptionBody({ destinationStatus: 'REVOKED' }),
            revokedSubscriptionBody({ id: SUB_ID_2, destinationStatus: 'ACTIVE' }),
          ],
          61,
          50,
          25,
        ),
      ),
    )
    const page = await listSubscriptions(ORG, { limit: 25, offset: 50 })
    expect(page.total).toBe(61)
    expect(page.offset).toBe(50)
    expect(page.items.map((s) => [s.status, s.destinationStatus])).toEqual([
      ['ACTIVE', 'REVOKED'],
      ['REVOKED', 'ACTIVE'],
    ])
    expect(page.items[1]!.revokedAt).toBe('2026-11-22T09:00:00Z')
    expect(page.items[0]!.revokedAt).toBeNull()
  })

  it.each([
    ['a missing items array', { total: 0, limit: 25, offset: 0 }],
    ['a negative total', { items: [], total: -1, limit: 25, offset: 0 }],
    [
      'more items than the limit',
      subscriptionList([subscriptionBody(), subscriptionBody()], 2, 0, 1),
    ],
    [
      'an item of another organization',
      subscriptionList([subscriptionBody({ organizationId: 'o2' })]),
    ],
    ['a non-object body', 'text'],
  ])('rejects %s', async (_name, body) => {
    stubFetch(() => json(200, body))
    expect(await rejection(listSubscriptions(ORG, { limit: 25, offset: 0 }))).toMatchObject({
      code: 'invalid_response',
    })
  })

  it('passes a 403 and an invalid filter answer through as an ApiError', async () => {
    stubFetch(() => apiError(403, 'forbidden', 'you do not have permission to perform this action'))
    expect(await rejection(listSubscriptions(ORG, { limit: 25, offset: 0 }))).toMatchObject({
      status: 403,
      code: 'forbidden',
    })
  })
})

describe('getSubscription', () => {
  it('reads the organization-scoped subscription and encodes the path', async () => {
    const calls = stubFetch(() => json(200, { subscription: subscriptionBody() }))
    const s = await getSubscription(ORG, SUB_ID)
    expect(calls[0]!.path).toBe(ONE)
    expect(s).toMatchObject({ id: SUB_ID, endpointId: EP_ID, destinationName: 'Billing hook' })
    const encoded = stubFetch(() => json(200, { subscription: subscriptionBody({ id: 'a/b c' }) }))
    await getSubscription(ORG, 'a/b c')
    expect(encoded[0]!.path).toBe(`${BASE}/a%2Fb%20c`)
  })

  it('rejects an answer for another subscription or organization', async () => {
    stubFetch(() => json(200, { subscription: subscriptionBody({ id: SUB_ID_2 }) }))
    expect(await rejection(getSubscription(ORG, SUB_ID))).toMatchObject({
      code: 'invalid_response',
    })
    stubFetch(() => json(200, { subscription: subscriptionBody({ organizationId: 'o2' }) }))
    expect(await rejection(getSubscription(ORG, SUB_ID))).toMatchObject({
      code: 'invalid_response',
    })
  })

  it('passes a 404 through', async () => {
    stubFetch(() => apiError(404, 'not_found', 'resource not found'))
    expect(await rejection(getSubscription(ORG, SUB_ID))).toMatchObject({ status: 404 })
  })
})

describe('subscription parsing', () => {
  const parse = (over: Record<string, unknown>) =>
    parseSubscription(subscriptionBody(over), ORG, null)

  it('does not keep createdByUserId', () => {
    expect(parse({})).not.toHaveProperty('createdByUserId')
  })

  it.each([
    ['an unknown subscription status', { status: 'PAUSED' }],
    ['an unknown destination status', { destinationStatus: 'DELETED' }],
    ['an empty destination name', { destinationName: '' }],
    ['a missing endpoint id', { endpointId: undefined }],
    ['a missing destination id', { destinationId: undefined }],
    ['a missing creator', { createdByUserId: undefined }],
    ['a createdAt that is not a date', { createdAt: 'yesterday' }],
    ['a revokedAt on an ACTIVE subscription', { revokedAt: '2026-11-22T09:00:00Z' }],
    ['a REVOKED subscription without revokedAt', { status: 'REVOKED' }],
    ['a null revokedAt on a REVOKED subscription', { status: 'REVOKED', revokedAt: null }],
    [
      'an interval that ends before it starts',
      { status: 'REVOKED', revokedAt: '2026-11-19T00:00:00Z' },
    ],
  ])('rejects %s', (_name, over) => {
    expect(() => parse(over)).toThrow(expect.objectContaining({ code: 'invalid_response' }))
  })

  it('accepts a destination that is revoked while the subscription is still active', () => {
    expect(parse({ destinationStatus: 'REVOKED' })).toMatchObject({
      status: 'ACTIVE',
      destinationStatus: 'REVOKED',
    })
  })

  it('does not echo body values in error messages', () => {
    try {
      parse({ status: 'TOP-SECRET-VALUE' })
      throw new Error('expected a throw')
    } catch (error) {
      expect(isApiError(error)).toBe(true)
      expect(JSON.stringify(error, Object.getOwnPropertyNames(error))).not.toContain(
        'TOP-SECRET-VALUE',
      )
    }
  })
})

describe('subscription mutations', () => {
  const calls = (answer: () => Response) => {
    setCsrfToken('tok')
    return stubFetch(answer)
  }

  it('creates with exactly endpointId and destinationId and returns the new ACTIVE subscription', async () => {
    const recorded = calls(() => json(201, { subscription: subscriptionBody() }))
    const s = await createSubscription(ORG, INPUT)
    expect(recorded).toHaveLength(1)
    expect(recorded[0]).toMatchObject({ method: 'POST', path: BASE })
    expect(recorded[0]!.body).toEqual(INPUT)
    expect(recorded[0]!.headers.get('X-CSRF-Token')).toBe('tok')
    expect(s.status).toBe('ACTIVE')
  })

  it('revokes with a bodyless DELETE and expects the revoked subscription, not an empty answer', async () => {
    const recorded = calls(() => json(200, { subscription: revokedSubscriptionBody() }))
    const s = await revokeSubscription(ORG, SUB_ID)
    expect(recorded[0]).toMatchObject({ method: 'DELETE', path: ONE, body: undefined })
    expect(s).toMatchObject({ status: 'REVOKED', revokedAt: '2026-11-22T09:00:00Z' })
    calls(() => new Response(null, { status: 204 }))
    expect(await rejection(revokeSubscription(ORG, SUB_ID))).toMatchObject({
      code: UNREADABLE_SUCCESS,
    })
  })

  const unreadable: [string, () => Promise<unknown>, unknown][] = [
    [
      'a create answer for another endpoint',
      () => createSubscription(ORG, INPUT),
      subscriptionBody({ endpointId: EP_ID_2 }),
    ],
    [
      'a create answer for another destination',
      () => createSubscription(ORG, INPUT),
      subscriptionBody({ destinationId: DEST_ID_2 }),
    ],
    [
      'a create answer that is already REVOKED',
      () => createSubscription(ORG, INPUT),
      revokedSubscriptionBody(),
    ],
    [
      'a create answer of another organization',
      () => createSubscription(ORG, INPUT),
      subscriptionBody({ organizationId: 'o2' }),
    ],
    [
      'a revoke answer that is still ACTIVE',
      () => revokeSubscription(ORG, SUB_ID),
      subscriptionBody(),
    ],
    [
      'a revoke answer for another subscription',
      () => revokeSubscription(ORG, SUB_ID),
      revokedSubscriptionBody({ id: SUB_ID_2 }),
    ],
    [
      'an inconsistent revokedAt',
      () => revokeSubscription(ORG, SUB_ID),
      subscriptionBody({ status: 'REVOKED' }),
    ],
  ]
  it.each(unreadable)(
    'reports %s as unreadable_success without repeating',
    async (_n, run, body) => {
      const recorded = calls(() => json(200, { subscription: body }))
      const error = await rejection(run())
      expect(error).toMatchObject({ status: 200, code: UNREADABLE_SUCCESS })
      expect(recorded.filter((c) => c.method !== 'GET')).toHaveLength(1)
    },
  )

  it.each([
    ['a missing subscription key', {}],
    ['an extra key', { subscription: subscriptionBody(), extra: true }],
    ['a body that is not an object', 'text'],
  ])('reports %s as unreadable_success', async (_name, body) => {
    calls(() => json(201, body))
    expect(await rejection(createSubscription(ORG, INPUT))).toMatchObject({
      code: UNREADABLE_SUCCESS,
    })
  })

  it('reports a 2xx body that is not JSON as unreadable_success', async () => {
    const recorded = calls(() => new Response('<html>', { status: 200 }))
    expect(await rejection(revokeSubscription(ORG, SUB_ID))).toMatchObject({
      code: UNREADABLE_SUCCESS,
    })
    expect(recorded).toHaveLength(1)
  })

  it.each([
    [409, 'already_subscribed'],
    [409, 'endpoint_revoked'],
    [409, 'destination_revoked'],
    [409, 'subscription_revoked'],
    [404, 'not_found'],
    [403, 'forbidden'],
    [400, 'validation_failed'],
  ])('keeps the %s %s rejection distinct and does not repeat the request', async (status, code) => {
    const recorded = calls(() => apiError(status, code, 'rejected'))
    expect(await rejection(createSubscription(ORG, INPUT))).toMatchObject({ status, code })
    expect(recorded).toHaveLength(1)
  })

  it('reports a lost connection as a status 0 error without repeating the request', async () => {
    const recorded = calls(() => {
      throw new TypeError('Failed to fetch')
    })
    expect(await rejection(revokeSubscription(ORG, SUB_ID))).toMatchObject({
      status: 0,
      code: 'network_error',
    })
    expect(recorded).toHaveLength(1)
  })

  it('repeats a mutation only after csrf_invalid, once', async () => {
    setCsrfToken('stale')
    let posts = 0
    const recorded = stubFetch((call) => {
      if (call.path.endsWith('/auth/csrf')) return json(200, { session: { csrfToken: 'fresh' } })
      posts += 1
      return posts === 1
        ? apiError(403, 'csrf_invalid', 'bad token')
        : json(201, { subscription: subscriptionBody() })
    })
    await createSubscription(ORG, INPUT)
    expect(recorded.filter((c) => c.path === BASE)).toHaveLength(2)
  })
})

describe('re-subscribing after a revocation (ADR 26)', () => {
  const OLD_STARTED = '2026-11-20T10:00:00Z'
  const OLD_ENDED = '2026-11-22T09:00:00Z'
  const NEW_STARTED = '2026-11-23T10:00:00Z'
  const oldRecord = () => revokedSubscriptionBody({ createdAt: OLD_STARTED, revokedAt: OLD_ENDED })
  const newRecord = () => subscriptionBody({ id: SUB_ID_2, createdAt: NEW_STARTED })

  it('creates a new ACTIVE record next to the revoked one and never addresses the old record', async () => {
    setCsrfToken('tok')
    const calls = stubFetch((call) => {
      if (call.method === 'GET') {
        const status = new URL(call.path, 'http://x').searchParams.get('status')
        // The server filters by status: an ACTIVE-only check does not match the revoked record.
        return status === 'ACTIVE'
          ? json(200, subscriptionList([], 0, 0, 1))
          : json(200, subscriptionList([newRecord(), oldRecord()], 2, 0, 100))
      }
      return json(201, { subscription: newRecord() })
    })

    const active = await listSubscriptions(ORG, { limit: 1, offset: 0, status: 'ACTIVE', ...INPUT })
    expect(active.items).toEqual([])
    expect(active.total).toBe(0)

    const created = await createSubscription(ORG, INPUT)
    expect(created.id).toBe(SUB_ID_2)
    expect(created.id).not.toBe(SUB_ID)
    expect(created).toMatchObject({ status: 'ACTIVE', revokedAt: null, createdAt: NEW_STARTED })

    const history = await listSubscriptions(ORG, { limit: 100, offset: 0, ...INPUT })
    expect(history.items.map((s) => [s.id, s.status])).toEqual([
      [SUB_ID_2, 'ACTIVE'],
      [SUB_ID, 'REVOKED'],
    ])
    // The previous record is exactly as it was: still REVOKED, with its original interval.
    expect(history.items.find((s) => s.id === SUB_ID)).toMatchObject({
      status: 'REVOKED',
      createdAt: OLD_STARTED,
      revokedAt: OLD_ENDED,
    })

    // One write, to the collection. Nothing was sent to the old record, by id or in a body.
    const writes = calls.filter((c) => c.method !== 'GET' && !c.path.includes('/auth/'))
    expect(writes.map((c) => `${c.method} ${c.path}`)).toEqual([`POST ${BASE}`])
    expect(JSON.stringify(writes[0]!.body)).not.toContain(SUB_ID)
    expect(calls.some((c) => c.path.includes(`/${SUB_ID}`))).toBe(false)
  })

  it('does not accept a create answer that is the revoked record (no reactivation)', async () => {
    setCsrfToken('tok')
    stubFetch(() => json(201, { subscription: oldRecord() }))
    expect(await rejection(createSubscription(ORG, INPUT))).toMatchObject({
      status: 200,
      code: UNREADABLE_SUCCESS,
    })
  })
})
