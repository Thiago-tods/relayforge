import { describe, expect, it } from 'vitest'
import {
  attemptItem,
  DEST_A,
  deliverySummary,
  destinationPage,
  failureItem,
  feedPage,
  json,
  stubFetch,
} from '../test/fetchStub'
import { listDestinations } from './destinations'
import { isApiError } from './errors'
import { getAttempts, getDeliverySummary, getFailures } from './operations'

const ORG = '/api/v1/organizations/org-1/operations'

/** The path and the parsed query of a recorded call, so a test does not depend on parameter order. */
function parts(path: string) {
  const url = new URL(path, 'http://localhost')
  return { path: url.pathname, query: Object.fromEntries(url.searchParams) }
}

async function rejection(promise: Promise<unknown>) {
  try {
    await promise
  } catch (error) {
    return error
  }
  throw new Error('expected a rejection')
}

describe('delivery summary request', () => {
  it('sends no range at all for the default window, so the server picks it', async () => {
    const calls = stubFetch(() => json(200, deliverySummary()))
    await getDeliverySummary('org-1', {})
    expect(calls[0]?.path).toBe(`${ORG}/deliveries`)
  })

  it('sends an explicit range as from and to', async () => {
    const calls = stubFetch(() => json(200, deliverySummary()))
    await getDeliverySummary('org-1', { from: '2026-11-01T00:00:00Z', to: '2026-11-02T00:00:00Z' })
    expect(parts(calls[0]!.path)).toEqual({
      path: `${ORG}/deliveries`,
      query: { from: '2026-11-01T00:00:00Z', to: '2026-11-02T00:00:00Z' },
    })
  })

  it('keeps the numbers and the echoed window', async () => {
    stubFetch(() => json(200, deliverySummary()))
    const summary = await getDeliverySummary('org-1', {})
    expect(summary.created.byStatus.FAILED).toBe(6)
    expect(summary.completed.failureRate).toBeCloseTo(1 / 6)
    expect(summary.from).toBe('2026-11-21T17:30:00.123456Z')
  })

  it('accepts a null failure rate', async () => {
    stubFetch(() =>
      json(
        200,
        deliverySummary({ completed: { ...deliverySummary().completed, failureRate: null } }),
      ),
    )
    expect((await getDeliverySummary('org-1', {})).completed.failureRate).toBeNull()
  })
})

describe('failure feed request', () => {
  it('sends only the parameters it was given', async () => {
    const calls = stubFetch(() => json(200, feedPage([failureItem()])))
    await getFailures('org-1', { limit: 25 })
    expect(parts(calls[0]!.path)).toEqual({ path: `${ORG}/failures`, query: { limit: '25' } })
  })

  it('sends the filters, the range and the cursor exactly as given', async () => {
    const calls = stubFetch(() => json(200, feedPage([])))
    await getFailures('org-1', {
      limit: 25,
      cursor: 'opaque+/=cursor',
      failureReason: 'UNRECORDED',
      destinationId: DEST_A,
      from: '2026-11-01T00:00:00Z',
      to: '2026-11-08T00:00:00Z',
    })
    expect(parts(calls[0]!.path).query).toEqual({
      limit: '25',
      cursor: 'opaque+/=cursor',
      failureReason: 'UNRECORDED',
      destinationId: DEST_A,
      from: '2026-11-01T00:00:00Z',
      to: '2026-11-08T00:00:00Z',
    })
  })

  it('skips empty values and a null cursor', async () => {
    const calls = stubFetch(() => json(200, feedPage([])))
    await getFailures('org-1', { cursor: null, failureReason: '', destinationId: '' })
    expect(calls[0]?.path).toBe(`${ORG}/failures`)
  })

  it('reads items, absent optional fields as null, and the next cursor', async () => {
    // undefined fields are left out of the JSON body, like the backend's omitempty.
    const bare = failureItem({ lastHttpStatus: undefined, lastErrorClass: undefined })
    stubFetch(() => json(200, feedPage([bare], 'next-1')))
    const feed = await getFailures('org-1', {})
    expect(feed.items[0]).toMatchObject({ lastHttpStatus: null, lastErrorClass: null })
    expect(feed.nextCursor).toBe('next-1')
  })
})

describe('attempt feed request', () => {
  it('sends outcome and delivery filters, and never a destination filter', async () => {
    const calls = stubFetch(() => json(200, feedPage([attemptItem()])))
    await getAttempts('org-1', { limit: 25, outcome: 'ABANDONED', deliveryId: 'd-1' })
    const { path, query } = parts(calls[0]!.path)
    expect(path).toBe(`${ORG}/attempts`)
    expect(query).toEqual({ limit: '25', outcome: 'ABANDONED', deliveryId: 'd-1' })
    expect(Object.keys(query)).not.toContain('destinationId')
  })

  it('reads an attempt without an HTTP status and an episode marker', async () => {
    const item = attemptItem({
      outcome: 'ABANDONED',
      httpStatus: undefined,
      errorClass: undefined,
      terminalReason: 'RETRIES_EXHAUSTED',
    })
    stubFetch(() => json(200, feedPage([item])))
    const feed = await getAttempts('org-1', {})
    expect(feed.items[0]).toMatchObject({
      outcome: 'ABANDONED',
      httpStatus: null,
      errorClass: null,
      terminalReason: 'RETRIES_EXHAUSTED',
    })
  })

  it('keeps an outcome it does not know instead of rejecting the page', async () => {
    stubFetch(() => json(200, feedPage([attemptItem({ outcome: 'SOMETHING_NEW' })])))
    expect((await getAttempts('org-1', {})).items[0]?.outcome).toBe('SOMETHING_NEW')
  })
})

describe('malformed responses', () => {
  const bad: [string, unknown][] = [
    ['not an object', 'text'],
    ['null', null],
    ['an array', []],
    ['items is not an array', { ...feedPage([]), items: {} }],
    ['nextCursor is a number', { ...feedPage([]), nextCursor: 5 }],
    ['nextCursor is an empty string', { ...feedPage([]), nextCursor: '' }],
    ['an item without a delivery id', feedPage([{ ...failureItem(), deliveryId: undefined }])],
    ['a count that is a string', feedPage([failureItem({ attemptCount: '3' })])],
    ['a negative count', feedPage([failureItem({ attemptCount: -1 })])],
    ['no window', { items: [], nextCursor: null }],
  ]

  it.each(bad)('rejects a failure feed where %s, with a safe error', async (_name, body) => {
    stubFetch(() => json(200, body))
    const error = await rejection(getFailures('org-1', {}))
    expect(isApiError(error) && error.code).toBe('invalid_response')
    expect((error as Error).message).not.toMatch(/undefined|TypeError/)
  })

  it('rejects a summary with a missing cohort', async () => {
    stubFetch(() => json(200, { ...deliverySummary(), created: undefined }))
    const error = await rejection(getDeliverySummary('org-1', {}))
    expect(isApiError(error) && error.code).toBe('invalid_response')
  })

  it('rejects a summary with a missing failure reason', async () => {
    const summary = deliverySummary()
    stubFetch(() =>
      json(200, {
        ...summary,
        completed: { ...summary.completed, byFailureReason: { DESTINATION_REJECTED: 1 } },
      }),
    )
    expect(isApiError(await rejection(getDeliverySummary('org-1', {})))).toBe(true)
  })

  it('rejects an attempt without an outcome', async () => {
    stubFetch(() => json(200, feedPage([attemptItem({ outcome: undefined })])))
    expect(isApiError(await rejection(getAttempts('org-1', {})))).toBe(true)
  })
})

describe('destination list request', () => {
  it('asks for one page by limit and offset', async () => {
    const calls = stubFetch(() => json(200, destinationPage([{ id: DEST_A, name: 'A' }], 1)))
    await listDestinations('org-1', { limit: 100, offset: 200 })
    expect(parts(calls[0]!.path)).toEqual({
      path: '/api/v1/organizations/org-1/destinations',
      query: { limit: '100', offset: '200' },
    })
  })

  it('keeps id, name and status and drops the url and every other field', async () => {
    stubFetch(() => json(200, destinationPage([{ id: DEST_A, name: 'A', status: 'PAUSED' }], 1)))
    const page = await listDestinations('org-1', { limit: 100, offset: 0 })
    expect(page.items).toEqual([{ id: DEST_A, name: 'A', status: 'PAUSED' }])
    expect(JSON.stringify(page)).not.toContain('secret-host')
    expect(page.total).toBe(1)
  })

  it('rejects a body without a total', async () => {
    stubFetch(() => json(200, { items: [], limit: 100, offset: 0 }))
    expect(isApiError(await rejection(listDestinations('org-1', { limit: 100, offset: 0 })))).toBe(
      true,
    )
  })
})
