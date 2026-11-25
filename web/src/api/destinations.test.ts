import { describe, expect, it } from 'vitest'
import {
  activateDestination,
  createDestination,
  getDestination,
  listDestinationRecords,
  parseDestination,
  pauseDestination,
  revokeDestination,
  rotateDestinationSecret,
  updateDestination,
} from './destinations'
import { setCsrfToken } from './client'
import { UNREADABLE_SUCCESS } from './deliveries'
import { isApiError } from './errors'
import {
  destinationBody,
  destinationList,
  DEST_ID,
  revokedBody,
  SECRET,
} from '../test/destinationFixtures'
import { apiError, json, stubFetch } from '../test/fetchStub'

// A stubbed fetch: these tests prove how the client builds requests and parses answers, not the real backend.

const ORG = 'o1'
const BASE = `/api/v1/organizations/${ORG}/destinations`
const ONE = `${BASE}/${DEST_ID}`

async function rejection(promise: Promise<unknown>): Promise<unknown> {
  return promise.then(
    () => {
      throw new Error('expected a rejection')
    },
    (error: unknown) => error,
  )
}

describe('listDestinationRecords', () => {
  it('asks for one page with limit, offset and an optional status', async () => {
    const calls = stubFetch(() => json(200, destinationList([destinationBody()], 1)))
    await listDestinationRecords(ORG, { limit: 25, offset: 50, status: 'PAUSED' })
    const url = new URL(calls[0]!.path, 'http://x')
    expect(url.pathname).toBe(BASE)
    expect(Object.fromEntries(url.searchParams)).toEqual({
      limit: '25',
      offset: '50',
      status: 'PAUSED',
    })
    expect(calls[0]!.method).toBe('GET')
  })

  it('sends no status for the all filter', async () => {
    const calls = stubFetch(() => json(200, destinationList([])))
    await listDestinationRecords(ORG, { limit: 25, offset: 0 })
    expect(new URL(calls[0]!.path, 'http://x').searchParams.has('status')).toBe(false)
  })

  it('parses items, the exact total and the page window', async () => {
    stubFetch(() =>
      json(
        200,
        destinationList([destinationBody(), revokedBody({ id: 'x2', name: 'Old' })], 7, 25),
      ),
    )
    const page = await listDestinationRecords(ORG, { limit: 25, offset: 25 })
    expect(page.total).toBe(7)
    expect(page.offset).toBe(25)
    expect(page.items.map((d) => d.status)).toEqual(['ACTIVE', 'REVOKED'])
    expect(page.items[1]?.revokedAt).toBe('2026-11-22T09:00:00Z')
  })

  const malformed: [string, unknown][] = [
    ['no items', { total: 0, limit: 25, offset: 0 }],
    ['no total', { items: [], limit: 25, offset: 0 }],
    [
      'more items than the limit',
      destinationList([destinationBody(), destinationBody({ id: 'b' })], 2, 0, 1),
    ],
    [
      'an item of another organization',
      destinationList([destinationBody({ organizationId: 'o2' })]),
    ],
    ['an item with an unknown status', destinationList([destinationBody({ status: 'DELETED' })])],
    ['a secret in a list item', destinationList([destinationBody({ signingSecret: SECRET })])],
    ['an array body', []],
  ]
  it.each(malformed)('rejects a page with %s', async (_name, body) => {
    stubFetch(() => json(200, body))
    const error = await rejection(listDestinationRecords(ORG, { limit: 25, offset: 0 }))
    expect(isApiError(error)).toBe(true)
    expect(error).toMatchObject({ code: 'invalid_response' })
    expect(JSON.stringify(error, Object.getOwnPropertyNames(error))).not.toContain(SECRET)
  })
})

describe('getDestination', () => {
  it('reads the organization-scoped destination', async () => {
    const calls = stubFetch(() => json(200, { destination: destinationBody() }))
    const destination = await getDestination(ORG, DEST_ID)
    expect(calls[0]!.path).toBe(ONE)
    expect(destination.name).toBe('Billing hook')
    expect(destination.urlHasQuery).toBe(false)
  })

  it('rejects an answer for another destination or organization', async () => {
    stubFetch(() => json(200, { destination: destinationBody({ id: 'other' }) }))
    expect(await rejection(getDestination(ORG, DEST_ID))).toMatchObject({
      code: 'invalid_response',
    })
    stubFetch(() => json(200, { destination: destinationBody({ organizationId: 'o2' }) }))
    expect(await rejection(getDestination(ORG, DEST_ID))).toMatchObject({
      code: 'invalid_response',
    })
  })

  it('rejects a secret in a normal answer', async () => {
    stubFetch(() => json(200, { destination: destinationBody({ signingSecret: SECRET }) }))
    expect(await rejection(getDestination(ORG, DEST_ID))).toMatchObject({
      code: 'invalid_response',
    })
  })

  it('passes a 404 through as an ApiError', async () => {
    stubFetch(() => apiError(404, 'not_found', 'resource not found'))
    expect(await rejection(getDestination(ORG, DEST_ID))).toMatchObject({ status: 404 })
  })

  it('encodes the path segments', async () => {
    const calls = stubFetch(() => apiError(404, 'not_found', 'x'))
    await getDestination('o/1', 'a b').catch(() => undefined)
    expect(calls[0]!.path).toBe('/api/v1/organizations/o%2F1/destinations/a%20b')
  })
})

describe('destination parsing', () => {
  it('rejects timestamps that are not dates and an inconsistent revokedAt', () => {
    expect(() => parseDestination(destinationBody({ createdAt: 'yesterday' }), ORG, null)).toThrow()
    expect(() => parseDestination(destinationBody({ updatedAt: '' }), ORG, null)).toThrow()
    expect(() =>
      parseDestination(destinationBody({ revokedAt: '2026-11-22T09:00:00Z' }), ORG, null),
    ).toThrow()
    expect(() => parseDestination(destinationBody({ status: 'REVOKED' }), ORG, null)).toThrow()
    expect(parseDestination(revokedBody(), ORG, null).destination.status).toBe('REVOKED')
  })

  it('rejects a URL that still carries a query or fragment, and a non-boolean urlHasQuery', () => {
    expect(() =>
      parseDestination(destinationBody({ url: 'https://h.example/x?token=abc' }), ORG, null),
    ).toThrow()
    expect(() =>
      parseDestination(destinationBody({ url: 'https://h.example/x#frag' }), ORG, null),
    ).toThrow()
    expect(() => parseDestination(destinationBody({ urlHasQuery: 'yes' }), ORG, null)).toThrow()
    expect(() => parseDestination(destinationBody({ urlHasQuery: undefined }), ORG, null)).toThrow()
  })

  it('accepts a hidden query (urlHasQuery true with a bare URL)', () => {
    const parsed = parseDestination(destinationBody({ urlHasQuery: true }), ORG, null)
    expect(parsed.destination.urlHasQuery).toBe(true)
    expect(parsed.destination.url).toBe('https://hooks.example.com/billing')
  })

  it('requires the secret only where it is expected', () => {
    expect(() => parseDestination(destinationBody(), ORG, null, 'required')).toThrow()
    expect(() =>
      parseDestination(destinationBody({ signingSecret: '' }), ORG, null, 'required'),
    ).toThrow()
    expect(
      parseDestination(destinationBody({ signingSecret: SECRET }), ORG, null, 'required')
        .signingSecret,
    ).toBe(SECRET)
    expect(() =>
      parseDestination(destinationBody({ signingSecret: SECRET }), ORG, null, 'absent'),
    ).toThrow()
  })

  it('does not echo body values in error messages', () => {
    try {
      parseDestination(destinationBody({ signingSecret: SECRET }), ORG, null, 'absent')
      expect.fail('expected an error')
    } catch (error) {
      expect((error as Error).message).not.toContain(SECRET)
    }
  })
})

describe('destination mutations', () => {
  const calls = (answer: () => Response) => {
    setCsrfToken('tok')
    return stubFetch(answer)
  }

  it('creates with exactly name and url and returns the one-time secret', async () => {
    const recorded = calls(() =>
      json(201, { destination: destinationBody({ signingSecret: SECRET }) }),
    )
    const result = await createDestination(ORG, {
      name: 'Billing hook',
      url: 'https://hooks.example.com/billing?token=t',
    })
    expect(recorded).toHaveLength(1)
    expect(recorded[0]!.method).toBe('POST')
    expect(recorded[0]!.path).toBe(BASE)
    expect(recorded[0]!.body).toEqual({
      name: 'Billing hook',
      url: 'https://hooks.example.com/billing?token=t',
    })
    expect(recorded[0]!.headers.get('X-CSRF-Token')).toBe('tok')
    expect(result.signingSecret).toBe(SECRET)
    expect(result.destination).not.toHaveProperty('signingSecret')
    expect(JSON.stringify(result.destination)).not.toContain(SECRET)
  })

  it('updates with only the fields given', async () => {
    const recorded = calls(() => json(200, { destination: destinationBody({ name: 'Renamed' }) }))
    const updated = await updateDestination(ORG, DEST_ID, { name: 'Renamed' })
    expect(recorded[0]!.method).toBe('PATCH')
    expect(recorded[0]!.path).toBe(ONE)
    expect(recorded[0]!.body).toEqual({ name: 'Renamed' })
    expect(updated.name).toBe('Renamed')
    const second = calls(() => json(200, { destination: destinationBody() }))
    await updateDestination(ORG, DEST_ID, { url: 'https://new.example.com/h' })
    expect(second[0]!.body).toEqual({ url: 'https://new.example.com/h' })
  })

  it('pauses and activates with a bodyless POST', async () => {
    const paused = calls(() => json(200, { destination: destinationBody({ status: 'PAUSED' }) }))
    expect((await pauseDestination(ORG, DEST_ID)).status).toBe('PAUSED')
    expect(paused[0]).toMatchObject({ method: 'POST', path: `${ONE}/pause`, body: undefined })
    const active = calls(() => json(200, { destination: destinationBody() }))
    expect((await activateDestination(ORG, DEST_ID)).status).toBe('ACTIVE')
    expect(active[0]).toMatchObject({ method: 'POST', path: `${ONE}/activate` })
  })

  it('rotates the secret and returns the new one once', async () => {
    const recorded = calls(() =>
      json(200, { destination: destinationBody({ signingSecret: SECRET }) }),
    )
    const result = await rotateDestinationSecret(ORG, DEST_ID)
    expect(recorded[0]).toMatchObject({ method: 'POST', path: `${ONE}/rotate-secret` })
    expect(result.signingSecret).toBe(SECRET)
  })

  it('revokes with DELETE and expects the revoked destination, not an empty answer', async () => {
    const recorded = calls(() => json(200, { destination: revokedBody() }))
    const revoked = await revokeDestination(ORG, DEST_ID)
    expect(recorded[0]).toMatchObject({ method: 'DELETE', path: ONE })
    expect(revoked.status).toBe('REVOKED')
    calls(() => new Response(null, { status: 204 }))
    expect(await rejection(revokeDestination(ORG, DEST_ID))).toMatchObject({
      code: UNREADABLE_SUCCESS,
    })
  })

  const unreadable: [string, () => Promise<unknown>, unknown][] = [
    [
      'create without a secret',
      () => createDestination(ORG, { name: 'n', url: 'https://h.example' }),
      { destination: destinationBody() },
    ],
    [
      'create that is not ACTIVE',
      () => createDestination(ORG, { name: 'n', url: 'https://h.example' }),
      { destination: destinationBody({ status: 'PAUSED', signingSecret: SECRET }) },
    ],
    [
      'rotate without a secret',
      () => rotateDestinationSecret(ORG, DEST_ID),
      { destination: destinationBody() },
    ],
    [
      'rotate of a revoked destination',
      () => rotateDestinationSecret(ORG, DEST_ID),
      { destination: revokedBody({ signingSecret: SECRET }) },
    ],
    [
      'pause that is not PAUSED',
      () => pauseDestination(ORG, DEST_ID),
      { destination: destinationBody() },
    ],
    [
      'activate that is not ACTIVE',
      () => activateDestination(ORG, DEST_ID),
      { destination: destinationBody({ status: 'PAUSED' }) },
    ],
    [
      'revoke that is not REVOKED',
      () => revokeDestination(ORG, DEST_ID),
      { destination: destinationBody() },
    ],
    [
      'update of a revoked destination',
      () => updateDestination(ORG, DEST_ID, { name: 'x' }),
      { destination: revokedBody() },
    ],
    [
      'a secret in a pause answer',
      () => pauseDestination(ORG, DEST_ID),
      { destination: destinationBody({ status: 'PAUSED', signingSecret: SECRET }) },
    ],
    [
      'another destination id',
      () => pauseDestination(ORG, DEST_ID),
      { destination: destinationBody({ id: 'other', status: 'PAUSED' }) },
    ],
    [
      'another organization',
      () => pauseDestination(ORG, DEST_ID),
      { destination: destinationBody({ organizationId: 'o2', status: 'PAUSED' }) },
    ],
    [
      'an extra top-level key',
      () => pauseDestination(ORG, DEST_ID),
      { destination: destinationBody({ status: 'PAUSED' }), extra: 1 },
    ],
    ['a missing destination key', () => pauseDestination(ORG, DEST_ID), {}],
    ['a body that is not an object', () => pauseDestination(ORG, DEST_ID), 'text'],
  ]
  it.each(unreadable)(
    'reports unreadable_success for %s and never repeats the request',
    async (_name, run, body) => {
      const recorded = calls(() => json(200, body))
      const error = await rejection(run())
      expect(error).toMatchObject({ status: 200, code: UNREADABLE_SUCCESS })
      expect(JSON.stringify(error, Object.getOwnPropertyNames(error))).not.toContain(SECRET)
      expect(recorded.filter((c) => c.method !== 'GET')).toHaveLength(1)
    },
  )

  it('reports unreadable_success for a 2xx body that is not JSON', async () => {
    const recorded = calls(() => new Response('<html>', { status: 200 }))
    expect(await rejection(pauseDestination(ORG, DEST_ID))).toMatchObject({
      code: UNREADABLE_SUCCESS,
    })
    expect(recorded).toHaveLength(1)
  })

  it('keeps ordinary rejections distinct and does not repeat the request', async () => {
    const recorded = calls(() =>
      apiError(409, 'destination_name_taken', 'a destination with this name already exists'),
    )
    expect(
      await rejection(createDestination(ORG, { name: 'n', url: 'https://h.example' })),
    ).toMatchObject({
      status: 409,
      code: 'destination_name_taken',
    })
    expect(recorded).toHaveLength(1)
  })

  it('reports a lost connection as a status 0 error without repeating the request', async () => {
    const recorded = calls(() => {
      throw new TypeError('Failed to fetch')
    })
    expect(await rejection(rotateDestinationSecret(ORG, DEST_ID))).toMatchObject({
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
        : json(200, { destination: destinationBody({ status: 'PAUSED' }) })
    })
    await pauseDestination(ORG, DEST_ID)
    expect(recorded.filter((c) => c.path === `${ONE}/pause`)).toHaveLength(2)
  })
})
