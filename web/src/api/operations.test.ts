import { describe, expect, it } from 'vitest'
import { isApiError } from './errors'
import { getQueue, getQueueAge } from './operations'
import { apiError, json, queueAge, queueSnapshot, stubFetch } from '../test/fetchStub'
import type { ApiError } from './errors'

async function failure(promise: Promise<unknown>): Promise<ApiError> {
  try {
    await promise
  } catch (error) {
    if (isApiError(error)) return error
    throw error
  }
  throw new Error('expected the request to fail')
}

describe('operations API', () => {
  it('reads the queue with a plain GET: no body, no CSRF header, same-origin credentials', async () => {
    const calls = stubFetch(() => json(200, queueSnapshot()))
    const result = await getQueue('org-1')
    expect(result.pending).toBe(12)
    expect(calls).toHaveLength(1)
    expect(calls[0]).toMatchObject({
      path: '/api/v1/organizations/org-1/operations/queue',
      method: 'GET',
      credentials: 'same-origin',
      body: undefined,
    })
    expect(calls[0]?.headers.has('X-CSRF-Token')).toBe(false)
  })

  it('reads the queue age from its own path', async () => {
    const calls = stubFetch(() => json(200, queueAge()))
    const result = await getQueueAge('org-1')
    expect(result.buckets).toHaveLength(7)
    expect(calls[0]?.path).toBe('/api/v1/organizations/org-1/operations/queue/age')
  })

  it('encodes the organization id so it cannot change the path', async () => {
    const calls = stubFetch(() => json(200, queueSnapshot()))
    await getQueue('a/b?c')
    expect(calls[0]?.path).toBe('/api/v1/organizations/a%2Fb%3Fc/operations/queue')
  })

  it('forwards the abort signal', async () => {
    const calls = stubFetch(() => json(200, queueSnapshot()))
    const controller = new AbortController()
    await getQueue('org-1', controller.signal)
    expect(calls[0]?.signal).toBe(controller.signal)
  })

  it.each([
    [403, 'forbidden', 'you do not have permission to perform this action'],
    [404, 'not_found', 'resource not found'],
    [503, 'read_model_timeout', 'the query took too long; try a shorter time window'],
  ])('maps a %d answer to its code and message', async (status, code, message) => {
    stubFetch(() => apiError(status, code, message))
    expect(await failure(getQueue('org-1'))).toMatchObject({ status, code, message })
  })

  it('hides the message of an unexpected server error', async () => {
    stubFetch(() => apiError(500, 'internal_error', 'connection string leaked'))
    expect((await failure(getQueueAge('org-1'))).message).not.toContain('leaked')
  })
})
