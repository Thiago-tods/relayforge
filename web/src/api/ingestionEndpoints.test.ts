import { describe, expect, it } from 'vitest'
import {
  getIngestionEndpoint,
  listIngestionEndpoints,
  parseIngestionEndpoint,
} from './ingestionEndpoints'
import { apiError, json, stubFetch } from '../test/fetchStub'
import { endpointBody, endpointList, EP_ID, EP_ID_2 } from '../test/subscriptionFixtures'

// A stubbed fetch: these tests prove how the client builds requests and parses answers, not the real backend.

const ORG = 'o1'
const BASE = `/api/v1/organizations/${ORG}/ingestion-endpoints`

async function rejection(promise: Promise<unknown>): Promise<unknown> {
  return promise.then(
    () => {
      throw new Error('expected a rejection')
    },
    (error: unknown) => error,
  )
}

describe('listIngestionEndpoints', () => {
  it('asks for one page with limit and offset only', async () => {
    const calls = stubFetch(() => json(200, endpointList([endpointBody()])))
    const page = await listIngestionEndpoints(ORG, { limit: 100, offset: 100 })
    const url = new URL(calls[0]!.path, 'http://x')
    expect(url.pathname).toBe(BASE)
    expect(Object.fromEntries(url.searchParams)).toEqual({ limit: '100', offset: '100' })
    expect(calls[0]!.method).toBe('GET')
    expect(page.items[0]).toEqual({
      id: EP_ID,
      organizationId: 'o1',
      name: 'Orders intake',
      status: 'ACTIVE',
      createdAt: '2026-11-19T10:00:00Z',
      updatedAt: '2026-11-19T10:00:00Z',
      revokedAt: null,
    })
  })

  it('keeps neither the public id nor the ingest path', async () => {
    stubFetch(() => json(200, endpointList([endpointBody()])))
    const page = await listIngestionEndpoints(ORG, { limit: 100, offset: 0 })
    expect(JSON.stringify(page)).not.toContain('pub_abc123')
    expect(JSON.stringify(page)).not.toContain('ingest')
  })

  it.each([
    ['an item of another organization', endpointList([endpointBody({ organizationId: 'o2' })])],
    ['an unknown status', endpointList([endpointBody({ status: 'DELETED' })])],
    ['a signing secret', endpointList([endpointBody({ signingSecret: 'whsec_x' })])],
    [
      'more items than the limit',
      endpointList([endpointBody(), endpointBody({ id: EP_ID_2 })], 2, 0, 1),
    ],
    ['a missing items array', { total: 0, limit: 100, offset: 0 }],
  ])('rejects %s', async (_name, body) => {
    stubFetch(() => json(200, body))
    expect(await rejection(listIngestionEndpoints(ORG, { limit: 100, offset: 0 }))).toMatchObject({
      code: 'invalid_response',
    })
  })
})

describe('getIngestionEndpoint', () => {
  it('reads one organization-scoped endpoint', async () => {
    const calls = stubFetch(() => json(200, { endpoint: endpointBody({ status: 'PAUSED' }) }))
    const e = await getIngestionEndpoint(ORG, EP_ID)
    expect(calls[0]!.path).toBe(`${BASE}/${EP_ID}`)
    expect(e).toMatchObject({ id: EP_ID, name: 'Orders intake', status: 'PAUSED' })
  })

  it('rejects an answer for another endpoint, another organization or with a secret', async () => {
    for (const over of [{ id: EP_ID_2 }, { organizationId: 'o2' }, { signingSecret: 'x' }]) {
      stubFetch(() => json(200, { endpoint: endpointBody(over) }))
      expect(await rejection(getIngestionEndpoint(ORG, EP_ID))).toMatchObject({
        code: 'invalid_response',
      })
    }
  })

  it('passes a 404 through', async () => {
    stubFetch(() => apiError(404, 'not_found', 'resource not found'))
    expect(await rejection(getIngestionEndpoint(ORG, EP_ID))).toMatchObject({ status: 404 })
  })
})

describe('endpoint parsing', () => {
  it('requires revokedAt exactly for a REVOKED endpoint', () => {
    expect(() => parseIngestionEndpoint(endpointBody({ status: 'REVOKED' }), ORG, null)).toThrow()
    expect(() =>
      parseIngestionEndpoint(endpointBody({ revokedAt: '2026-11-22T09:00:00Z' }), ORG, null),
    ).toThrow()
    expect(
      parseIngestionEndpoint(
        endpointBody({ status: 'REVOKED', revokedAt: '2026-11-22T09:00:00Z' }),
        ORG,
        null,
      ).revokedAt,
    ).toBe('2026-11-22T09:00:00Z')
  })
})
