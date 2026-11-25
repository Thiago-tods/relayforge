import { describe, expect, it } from 'vitest'
import { redriveDelivery, UNREADABLE_SUCCESS } from './deliveries'
import { setCsrfToken } from './client'
import { isApiError } from './errors'
import { apiError, json, stubFetch } from '../test/fetchStub'

// A stubbed fetch: these tests prove how the client builds and parses the request, not how the backend behaves.

const ORG = 'o1'
const ID = '11111111-1111-4111-8111-111111111111'
const PATH = `/api/v1/organizations/${ORG}/deliveries/${ID}/redrive`
const body = { reason: 'Receiver fixed', expectedAttemptCount: 3 }

const delivery = {
  id: ID,
  organizationId: ORG,
  eventId: '22222222-2222-4222-8222-222222222222',
  destinationId: '33333333-3333-4333-8333-333333333333',
  destinationName: 'Orders webhook',
  status: 'PENDING',
  attemptCount: 3,
  maxAttempts: 13,
  nextAttemptAt: '2026-11-25T10:00:00Z',
  lastHttpStatus: 503,
  lastErrorClass: 'server_error',
  createdAt: '2026-11-20T10:00:00Z',
  updatedAt: '2026-11-25T10:00:00Z',
}
const ok = {
  delivery,
  redrive: { maxAttempts: 13, previousFailureReason: 'RETRIES_EXHAUSTED', previousMaxAttempts: 8 },
}

describe('redriveDelivery', () => {
  it('sends one POST to the organization-scoped endpoint with exactly the two body fields and the CSRF token', async () => {
    setCsrfToken('tok-1')
    const calls = stubFetch(() => json(200, ok))
    const result = await redriveDelivery(
      ORG,
      ID,
      {
        reason: body.reason,
        expectedAttemptCount: body.expectedAttemptCount,
      },
      8,
    )
    expect(calls).toHaveLength(1)
    expect(calls[0]?.method).toBe('POST')
    expect(calls[0]?.path).toBe(PATH)
    expect(calls[0]?.body).toEqual(body)
    expect(Object.keys(calls[0]?.body as object).sort()).toEqual(['expectedAttemptCount', 'reason'])
    expect(calls[0]?.headers.get('X-CSRF-Token')).toBe('tok-1')
    expect(calls[0]?.headers.get('Content-Type')).toBe('application/json')
    expect(result.maxAttempts).toBe(13)
    expect(result.previousMaxAttempts).toBe(8)
    expect(result.previousFailureReason).toBe('RETRIES_EXHAUSTED')
    expect(result.delivery.status).toBe('PENDING')
  })

  it('fetches a CSRF token first when none is held', async () => {
    const calls = stubFetch((call) =>
      call.path.endsWith('/auth/csrf')
        ? json(200, { session: { csrfToken: 'fresh' } })
        : json(200, ok),
    )
    await redriveDelivery(ORG, ID, body, 8)
    expect(calls.map((call) => `${call.method} ${call.path}`)).toEqual([
      'POST /api/v1/auth/csrf',
      `POST ${PATH}`,
    ])
    expect(calls[1]?.headers.get('X-CSRF-Token')).toBe('fresh')
  })

  it('repeats the POST only after csrf_invalid, which the backend answers before the handler runs', async () => {
    setCsrfToken('stale')
    let posts = 0
    const calls = stubFetch((call) => {
      if (call.path.endsWith('/auth/csrf')) return json(200, { session: { csrfToken: 'fresh' } })
      posts += 1
      return posts === 1 ? apiError(403, 'csrf_invalid', 'bad token') : json(200, ok)
    })
    await redriveDelivery(ORG, ID, body, 8)
    expect(calls.filter((call) => call.path === PATH)).toHaveLength(2)
  })

  it('accepts a legacy delivery whose previous failure reason is null', async () => {
    setCsrfToken('t')
    stubFetch(() => json(200, { ...ok, redrive: { ...ok.redrive, previousFailureReason: null } }))
    expect((await redriveDelivery(ORG, ID, body, 8)).previousFailureReason).toBeNull()
  })

  const malformed: [string, unknown][] = [
    ['missing redrive', { delivery }],
    ['missing delivery', { redrive: ok.redrive }],
    ['extra top-level key', { ...ok, extra: true }],
    ['extra redrive key', { ...ok, redrive: { ...ok.redrive, extra: 1 } }],
    ['missing redrive key', { ...ok, redrive: { maxAttempts: 13, previousMaxAttempts: 8 } }],
    ['redrive not an object', { ...ok, redrive: 'x' }],
    ['array body', []],
    ['null body', null],
    ['string budget', { ...ok, redrive: { ...ok.redrive, maxAttempts: '13' } }],
    ['fractional budget', { ...ok, redrive: { ...ok.redrive, maxAttempts: 13.5 } }],
    ['budget not raised', { ...ok, redrive: { ...ok.redrive, maxAttempts: 8 } }],
    ['budget lowered', { ...ok, redrive: { ...ok.redrive, maxAttempts: 5 } }],
    ['previous budget zero', { ...ok, redrive: { ...ok.redrive, previousMaxAttempts: 0 } }],
    [
      'previous reason of wrong type',
      { ...ok, redrive: { ...ok.redrive, previousFailureReason: 7 } },
    ],
    ['delivery not an object', { ...ok, delivery: 'x' }],
    ['delivery missing required field', { ...ok, delivery: { ...delivery, id: undefined } }],
    ['wrong organization', { ...ok, delivery: { ...delivery, organizationId: 'o2' } }],
    ['wrong delivery id', { ...ok, delivery: { ...delivery, id: 'other' } }],
    ['delivery not PENDING', { ...ok, delivery: { ...delivery, status: 'FAILED' } }],
    [
      'delivery budget differs from redrive budget',
      { ...ok, delivery: { ...delivery, maxAttempts: 14 } },
    ],
  ]
  it.each(malformed)(
    'treats an HTTP 200 with %s as unreadable, not as success',
    async (_name, answer) => {
      setCsrfToken('t')
      stubFetch(() => json(200, answer))
      const error = await redriveDelivery(ORG, ID, body, 8).catch((e: unknown) => e)
      expect(isApiError(error)).toBe(true)
      expect(error).toMatchObject({ status: 200, code: UNREADABLE_SUCCESS })
    },
  )

  it('treats an HTTP 200 whose body is not JSON as unreadable', async () => {
    setCsrfToken('t')
    stubFetch(
      () =>
        new Response('<html>proxy page</html>', {
          status: 200,
          headers: { 'Content-Type': 'text/html' },
        }),
    )
    const error = await redriveDelivery(ORG, ID, body, 8).catch((e: unknown) => e)
    expect(error).toMatchObject({ status: 200, code: UNREADABLE_SUCCESS })
  })

  it('treats an empty 204 answer as unreadable', async () => {
    setCsrfToken('t')
    stubFetch(() => new Response(null, { status: 204 }))
    const error = await redriveDelivery(ORG, ID, body, 8).catch((e: unknown) => e)
    expect(error).toMatchObject({ code: UNREADABLE_SUCCESS })
  })

  it('keeps an ordinary validation error distinct from an unreadable success', async () => {
    setCsrfToken('t')
    stubFetch(() =>
      apiError(400, 'validation_failed', 'reason must be 1 to 500 printable characters'),
    )
    const error = await redriveDelivery(ORG, ID, body, 8).catch((e: unknown) => e)
    expect(error).toMatchObject({ status: 400, code: 'validation_failed' })
    expect((error as Error).message).toBe('reason must be 1 to 500 printable characters')
  })

  it('reports a lost connection as a status 0 network error and does not repeat the POST', async () => {
    setCsrfToken('t')
    const calls = stubFetch(() => {
      throw new TypeError('Failed to fetch')
    })
    const error = await redriveDelivery(ORG, ID, body, 8).catch((e: unknown) => e)
    expect(error).toMatchObject({ status: 0, code: 'network_error' })
    expect(calls).toHaveLength(1)
  })

  it('passes known rejections through with their code', async () => {
    setCsrfToken('t')
    stubFetch(() => apiError(409, 'delivery_changed', 'the delivery changed'))
    expect(await redriveDelivery(ORG, ID, body, 8).catch((e: unknown) => e)).toMatchObject({
      status: 409,
      code: 'delivery_changed',
    })
  })

  it('encodes the path segments', async () => {
    setCsrfToken('t')
    const calls = stubFetch(() => apiError(404, 'not_found', 'x'))
    await redriveDelivery('o/1', 'a b', body, 8).catch(() => undefined)
    expect(calls[0]?.path).toBe('/api/v1/organizations/o%2F1/deliveries/a%20b/redrive')
  })
})

describe('redrive budget contract of a success response', () => {
  // previous and max are the response's figures; `snapshot` is the maxAttempts of the dialog snapshot.
  const answer = (previous: number, max: number, deliveryMax = max) =>
    json(200, {
      delivery: { ...delivery, maxAttempts: deliveryMax },
      redrive: { maxAttempts: max, previousFailureReason: null, previousMaxAttempts: previous },
    })

  it.each([
    [8, 13],
    [48, 50],
    [45, 50],
    [1, 6],
    [46, 50],
  ])('accepts %i to %i when the snapshot budget is the previous one', async (previous, max) => {
    setCsrfToken('t')
    stubFetch(() => answer(previous, max))
    const result = await redriveDelivery(ORG, ID, body, previous)
    expect(result.previousMaxAttempts).toBe(previous)
    expect(result.maxAttempts).toBe(max)
  })

  const invalid: [string, number, number, number, number?][] = [
    ['previousMaxAttempts above 50', 51, 50, 51],
    ['maxAttempts above 50', 48, 53, 48],
    ['both above 50', 55, 60, 55],
    ['an increase larger than five', 8, 14, 8],
    ['an increase of ten', 8, 18, 8],
    ['an increase smaller than five below the ceiling', 8, 12, 8],
    ['no increase', 8, 8, 8],
    ['a decrease', 13, 8, 13],
    ['a capped value that is not the cap', 48, 49, 48],
    ['maxAttempts zero', 1, 0, 1],
    ['a previous budget that differs from the snapshot', 13, 18, 8],
    [
      'a previous budget below the snapshot (capped response for another delivery state)',
      45,
      50,
      48,
    ],
    ['a delivery budget that differs from the redrive budget', 8, 13, 8, 14],
  ]
  it.each(invalid)(
    'reports unreadable_success for %s',
    async (_name, previous, max, snapshot, deliveryMax) => {
      setCsrfToken('t')
      const calls = stubFetch(() => answer(previous, max, deliveryMax ?? max))
      const error = await redriveDelivery(ORG, ID, body, snapshot).catch((e: unknown) => e)
      expect(error).toMatchObject({ status: 200, code: UNREADABLE_SUCCESS })
      expect(calls.filter((call) => call.method === 'POST' && call.path === PATH)).toHaveLength(1)
    },
  )

  it('keeps the request body to the two fields when the expected budget is passed', async () => {
    setCsrfToken('t')
    const calls = stubFetch(() => answer(8, 13))
    await redriveDelivery(ORG, ID, body, 8)
    expect(Object.keys(calls[0]?.body as object).sort()).toEqual(['expectedAttemptCount', 'reason'])
  })
})
