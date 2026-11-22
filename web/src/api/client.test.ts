import { describe, expect, it, vi } from 'vitest'
import { login, logout } from './auth'
import {
  request,
  resetClient,
  rotateCsrf,
  setCsrfToken,
  setIdentityGuard,
  setUnauthenticatedHandler,
} from './client'
import { ApiError, isApiError, userMessage } from './errors'
import { apiError, json, meBody, noContent, stubFetch } from '../test/fetchStub'

async function failure(promise: Promise<unknown>): Promise<ApiError> {
  try {
    await promise
  } catch (error) {
    if (isApiError(error)) return error
    throw error
  }
  throw new Error('expected the request to fail')
}

describe('error mapping', () => {
  it('maps the backend error envelope', async () => {
    stubFetch(() => apiError(409, 'delivery_changed', 'The delivery changed.'))
    const error = await failure(request('/x'))
    expect(error).toMatchObject({
      status: 409,
      code: 'delivery_changed',
      message: 'The delivery changed.',
    })
  })

  it('shows the validation message of a 400', async () => {
    stubFetch(() => apiError(400, 'validation_failed', 'reason must be 1 to 500 characters'))
    expect((await failure(request('/x'))).message).toBe('reason must be 1 to 500 characters')
  })

  it('never shows a server message for a 5xx other than the two deliberate 503 codes', async () => {
    stubFetch(() => apiError(500, 'internal_error', 'db password leaked'))
    const error = await failure(request('/x'))
    expect(error.message).not.toContain('leaked')
    expect(error.message).toContain('could not complete')
  })

  it('keeps the message of lock_timeout and read_model_timeout', async () => {
    stubFetch(() =>
      apiError(503, 'lock_timeout', 'Another operation is in progress. Retry shortly.'),
    )
    expect((await failure(request('/x'))).message).toContain('Retry shortly')
  })

  it('reads Retry-After on a 429 and puts it in the user message', async () => {
    stubFetch(() => apiError(429, 'rate_limited', 'too many requests', { 'Retry-After': '30' }))
    const error = await failure(request('/x'))
    expect(error.retryAfter).toBe(30)
    expect(userMessage(error)).toContain('30 seconds')
  })

  it('uses a generic message when the body is not the error envelope', async () => {
    stubFetch(() => new Response('<html>proxy error</html>', { status: 502 }))
    const error = await failure(request('/x'))
    expect(error.status).toBe(502)
    expect(error.message).not.toContain('html')
  })

  it('reports a transport failure as status 0', async () => {
    stubFetch(() => Promise.reject(new TypeError('network down')))
    expect(await failure(request('/x'))).toMatchObject({ status: 0, code: 'network_error' })
  })

  it('rethrows an abort unchanged so callers can ignore obsolete requests', async () => {
    stubFetch(() => Promise.reject(new DOMException('aborted', 'AbortError')))
    await expect(request('/x', { signal: new AbortController().signal })).rejects.toMatchObject({
      name: 'AbortError',
    })
  })
})

describe('requests', () => {
  it('sends same-origin credentials and no CSRF header on a GET', async () => {
    const calls = stubFetch(() => json(200, { ok: true }))
    await request('/organizations')
    expect(calls[0]?.path).toBe('/api/v1/organizations')
    expect(calls[0]?.credentials).toBe('same-origin')
    expect(calls[0]?.headers.has('X-CSRF-Token')).toBe(false)
  })

  it('returns undefined for 204', async () => {
    stubFetch(() => noContent())
    expect(await request('/x', { method: 'DELETE', csrf: false })).toBeUndefined()
  })

  it('sends login without a CSRF token and keeps the token it returns', async () => {
    const calls = stubFetch((call) =>
      call.path.endsWith('/auth/login')
        ? json(200, {
            ...meBody([]),
            session: { expiresAt: '2099-01-01T00:00:00Z', csrfToken: 'tok-login' },
          })
        : json(200, {}),
    )
    await login('ada@example.com', 'a long enough password')
    expect(calls).toHaveLength(1)
    expect(calls[0]?.headers.has('X-CSRF-Token')).toBe(false)
    await request('/organizations', { method: 'POST', body: { name: 'x', slug: '' } })
    expect(calls[1]?.headers.get('X-CSRF-Token')).toBe('tok-login')
  })

  it('does not treat a 401 from login as a lost session', async () => {
    const handler = vi.fn()
    setUnauthenticatedHandler(handler)
    stubFetch(() => apiError(401, 'invalid_credentials', 'invalid email or password'))
    const error = await failure(login('a@example.com', 'x'))
    expect(error.code).toBe('invalid_credentials')
    expect(handler).not.toHaveBeenCalled()
  })
})

describe('CSRF handling (ADR 10)', () => {
  it('fetches a token before the first mutation after a reload', async () => {
    const calls = stubFetch((call) =>
      call.path.endsWith('/auth/csrf')
        ? json(200, { session: { csrfToken: 'tok-1' } })
        : json(200, {}),
    )
    await request('/x', { method: 'POST', body: {} })
    expect(calls.map((c) => c.path)).toEqual(['/api/v1/auth/csrf', '/api/v1/x'])
    expect(calls[0]?.headers.get('X-Requested-With')).toBe('XMLHttpRequest')
    expect(calls[1]?.headers.get('X-CSRF-Token')).toBe('tok-1')
  })

  it('replaces a rejected token once and sends the request once more', async () => {
    setCsrfToken('stale')
    const calls = stubFetch((call) => {
      if (call.path.endsWith('/auth/csrf')) return json(200, { session: { csrfToken: 'fresh' } })
      return call.headers.get('X-CSRF-Token') === 'fresh'
        ? json(200, { done: true })
        : apiError(403, 'csrf_invalid', 'missing or invalid CSRF token')
    })
    expect(await request('/x', { method: 'POST', body: {} })).toEqual({ done: true })
    expect(calls.map((c) => c.path)).toEqual(['/api/v1/x', '/api/v1/auth/csrf', '/api/v1/x'])
  })

  it('does not retry more than once', async () => {
    setCsrfToken('stale')
    const calls = stubFetch((call) =>
      call.path.endsWith('/auth/csrf')
        ? json(200, { session: { csrfToken: 'fresh' } })
        : apiError(403, 'csrf_invalid', 'missing or invalid CSRF token'),
    )
    expect((await failure(request('/x', { method: 'POST', body: {} }))).code).toBe('csrf_invalid')
    expect(calls.filter((c) => c.path === '/api/v1/x')).toHaveLength(2)
    expect(calls.filter((c) => c.path.endsWith('/auth/csrf'))).toHaveLength(1)
  })

  it('does not rotate for a 403 that is not csrf_invalid', async () => {
    setCsrfToken('good')
    const calls = stubFetch(() => apiError(403, 'forbidden', 'you do not have permission'))
    expect((await failure(request('/x', { method: 'POST', body: {} }))).code).toBe('forbidden')
    expect(calls).toHaveLength(1)
  })

  it('shares one rotation between concurrent failures in a tab', async () => {
    setCsrfToken('stale')
    let release: () => void = () => undefined
    const gate = new Promise<void>((resolve) => {
      release = resolve
    })
    const calls = stubFetch(async (call) => {
      if (call.path.endsWith('/auth/csrf')) {
        await gate
        return json(200, { session: { csrfToken: 'fresh' } })
      }
      return call.headers.get('X-CSRF-Token') === 'fresh'
        ? json(200, {})
        : apiError(403, 'csrf_invalid', 'missing or invalid CSRF token')
    })
    const both = Promise.all([
      request('/a', { method: 'POST', body: {} }),
      request('/b', { method: 'POST', body: {} }),
    ])
    await vi.waitFor(() => expect(calls.some((c) => c.path.endsWith('/auth/csrf'))).toBe(true))
    await new Promise((resolve) => setTimeout(resolve, 20))
    release()
    await both
    expect(calls.filter((c) => c.path.endsWith('/auth/csrf'))).toHaveLength(1)
  })

  it('does not store a token that arrives after the client was reset', async () => {
    let release: () => void = () => undefined
    const gate = new Promise<void>((resolve) => {
      release = resolve
    })
    const calls = stubFetch(async (call) => {
      if (call.path.endsWith('/auth/csrf')) {
        await gate
        return json(200, { session: { csrfToken: 'old-session' } })
      }
      return json(200, {})
    })
    const pending = rotateCsrf()
    resetClient()
    release()
    await pending
    await request('/x', { method: 'POST', body: {} })
    expect(calls.filter((c) => c.path.endsWith('/auth/csrf'))).toHaveLength(2)
  })
})

// Tabs share one session cookie. If another tab signed in as someone else, a token this tab obtains belongs to that
// other user's session, so the mutation must not be sent. The guard is how the app refuses it.
describe('identity guard', () => {
  const paths = (calls: { path: string }[]) => calls.map((c) => c.path)

  it('runs after a token is fetched and before the mutation is sent', async () => {
    const order: string[] = []
    setIdentityGuard(async () => {
      order.push('guard')
    })
    stubFetch((call) => {
      order.push(call.path)
      return call.path.endsWith('/auth/csrf')
        ? json(200, { session: { csrfToken: 'tok' } })
        : json(200, {})
    })
    await request('/x', { method: 'POST', body: {} })
    expect(order).toEqual(['/api/v1/auth/csrf', 'guard', '/api/v1/x'])
  })

  it('never sends the mutation when the guard refuses after the first token fetch', async () => {
    setIdentityGuard(() => Promise.reject(new ApiError(409, 'session_changed', 'changed')))
    const calls = stubFetch(() => json(200, { session: { csrfToken: 'tok' } }))
    expect((await failure(request('/x', { method: 'POST', body: {} }))).code).toBe(
      'session_changed',
    )
    expect(paths(calls)).toEqual(['/api/v1/auth/csrf'])
  })

  it('never sends the retry when the guard refuses after a rejected token was replaced', async () => {
    setCsrfToken('stale')
    setIdentityGuard(() => Promise.reject(new ApiError(409, 'session_changed', 'changed')))
    const calls = stubFetch((call) =>
      call.path.endsWith('/auth/csrf')
        ? json(200, { session: { csrfToken: 'fresh' } })
        : apiError(403, 'csrf_invalid', 'missing or invalid CSRF token'),
    )
    expect((await failure(request('/x', { method: 'POST', body: {} }))).code).toBe(
      'session_changed',
    )
    expect(paths(calls)).toEqual(['/api/v1/x', '/api/v1/auth/csrf'])
    expect(calls.filter((c) => c.path === '/api/v1/x')).toHaveLength(1)
  })

  it('is not consulted when a held token is accepted, for reads, or for login', async () => {
    const guard = vi.fn(() => Promise.resolve())
    setIdentityGuard(guard)
    setCsrfToken('good')
    stubFetch(() => json(200, {}))
    await request('/x', { method: 'POST', body: {} })
    await request('/y')
    await request('/auth/login', { method: 'POST', body: {}, csrf: false, quiet401: true })
    expect(guard).not.toHaveBeenCalled()
  })
})

describe('lost sessions', () => {
  it('calls the unauthenticated handler on a 401', async () => {
    const handler = vi.fn()
    setUnauthenticatedHandler(handler)
    stubFetch(() => apiError(401, 'unauthenticated', 'authentication is required'))
    await failure(request('/x'))
    expect(handler).toHaveBeenCalledTimes(1)
  })

  it('does not call it when the caller asked for a quiet 401', async () => {
    const handler = vi.fn()
    setUnauthenticatedHandler(handler)
    stubFetch(() => apiError(401, 'unauthenticated', 'authentication is required'))
    await failure(request('/x', { quiet401: true }))
    expect(handler).not.toHaveBeenCalled()
  })

  it('logout sends the CSRF token and resolves on 204', async () => {
    setCsrfToken('tok')
    const calls = stubFetch(() => noContent())
    await logout()
    expect(calls[0]?.headers.get('X-CSRF-Token')).toBe('tok')
  })
})
