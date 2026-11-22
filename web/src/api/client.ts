import { ApiError } from './errors'

// The single place that talks to the backend. All paths are same-origin and relative, so the session cookie
// (Path=/api, HttpOnly, SameSite=Lax) and the Origin/Host check of POST /auth/csrf behave as the backend expects.

const API_BASE = '/api/v1'

export interface RequestOptions {
  method?: 'GET' | 'POST' | 'PATCH' | 'DELETE'
  body?: unknown
  signal?: AbortSignal | undefined
  /** Set to false for the two unauthenticated POSTs (register, login): no CSRF token is sent or fetched. */
  csrf?: boolean
  /** Treat a 401 as an ordinary error instead of ending the session (bootstrap, login). */
  quiet401?: boolean
}

// CSRF state. The token is held in memory only: never in storage, the URL or a log.
// ADR 10: one token per session, and POST /auth/csrf replaces it, which invalidates the previous one in every tab.
let csrfToken: string | null = null
let rotation: Promise<string> | null = null
let generation = 0
let unauthenticatedHandler: (() => void) | null = null
let identityGuard: (() => Promise<void>) | null = null

/** Called when the server says the session is gone (401 unauthenticated). */
export function setUnauthenticatedHandler(handler: (() => void) | null): void {
  unauthenticatedHandler = handler
}

/**
 * Called, and awaited, whenever this tab has just obtained a CSRF token for the cookie's session and before the
 * state-changing request that needs it is sent. Tabs share one session cookie, so another tab may have signed in
 * as a different user; the guard must throw in that case, and the request is then never sent. Otherwise the
 * request would run as the other user while this tab still shows the first one.
 */
export function setIdentityGuard(guard: (() => Promise<void>) | null): void {
  identityGuard = guard
}

export function setCsrfToken(token: string | null): void {
  generation += 1
  csrfToken = token
}

/** Forgets the token. A rotation that is still in flight cannot store its result afterwards. */
export function resetClient(): void {
  setCsrfToken(null)
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null
}

function genericMessage(status: number): string {
  if (status === 0) return 'Could not reach the server. Check your connection and try again.'
  if (status === 429) return 'Too many requests. Wait a moment and try again.'
  if (status >= 500) return 'The server could not complete the request. Try again in a moment.'
  return 'The request could not be completed.'
}

// 503 codes the backend sends on purpose, with a message written for clients.
const SAFE_5XX_CODES = new Set(['lock_timeout', 'read_model_timeout'])

async function toApiError(res: Response): Promise<ApiError> {
  let code = 'unknown_error'
  let message = ''
  try {
    const data: unknown = await res.json()
    if (isRecord(data) && isRecord(data.error)) {
      if (typeof data.error.code === 'string') code = data.error.code
      if (typeof data.error.message === 'string') message = data.error.message
    }
  } catch {
    // A body that is not the error envelope is replaced by a generic message.
  }
  const retry = Number.parseInt(res.headers.get('Retry-After') ?? '', 10)
  const showServerMessage = res.status < 500 || SAFE_5XX_CODES.has(code)
  return new ApiError(
    res.status,
    code,
    (showServerMessage && message) || genericMessage(res.status),
    Number.isFinite(retry) && retry > 0 ? retry : undefined,
  )
}

async function fetchRaw(url: string, init: RequestInit): Promise<Response> {
  try {
    return await fetch(url, { ...init, credentials: 'same-origin', cache: 'no-store' })
  } catch (error) {
    if (error instanceof DOMException && error.name === 'AbortError') throw error
    throw new ApiError(0, 'network_error', genericMessage(0))
  }
}

/**
 * Replaces the CSRF token of the session. Concurrent callers share one request, so a tab never invalidates a
 * token it has just obtained itself.
 */
export function rotateCsrf(): Promise<string> {
  if (rotation) return rotation
  const startedIn = generation
  const current = (async () => {
    const res = await fetchRaw(`${API_BASE}/auth/csrf`, {
      method: 'POST',
      headers: { Accept: 'application/json', 'X-Requested-With': 'XMLHttpRequest' },
    })
    if (!res.ok) {
      const error = await toApiError(res)
      if (res.status === 401 && error.code === 'unauthenticated') unauthenticatedHandler?.()
      throw error
    }
    const data: unknown = await res.json()
    const session = isRecord(data) && isRecord(data.session) ? data.session : null
    const token = session !== null && typeof session.csrfToken === 'string' ? session.csrfToken : ''
    if (token === '') throw new ApiError(res.status, 'invalid_response', genericMessage(500))
    if (startedIn === generation) csrfToken = token
    return token
  })().finally(() => {
    rotation = null
  })
  rotation = current
  return current
}

async function send(
  path: string,
  method: string,
  options: RequestOptions,
  token: string | null,
): Promise<Response> {
  const headers: Record<string, string> = { Accept: 'application/json' }
  const init: RequestInit = { method, headers }
  if (options.body !== undefined) {
    headers['Content-Type'] = 'application/json'
    init.body = JSON.stringify(options.body)
  }
  if (token !== null) headers['X-CSRF-Token'] = token
  if (options.signal) init.signal = options.signal
  return fetchRaw(`${API_BASE}${path}`, init)
}

/**
 * Sends one API request and returns the parsed JSON (undefined for 204).
 *
 * Mutating requests carry the CSRF token. When none is held (first mutation after a reload) one is fetched. When
 * the server answers csrf_invalid the token is replaced once and the request is sent once more: the backend checks
 * the token before the handler runs, so a rejected request changed nothing and repeating it is safe.
 *
 * Whenever this call obtains a token itself, the identity guard runs before the request is sent, so a mutation is
 * never sent under a session that now belongs to someone else (see setIdentityGuard).
 */
export async function request<T>(path: string, options: RequestOptions = {}): Promise<T> {
  const method = options.method ?? 'GET'
  const needsCsrf = method !== 'GET' && options.csrf !== false
  let token: string | null = null
  if (needsCsrf) {
    if (csrfToken !== null) {
      token = csrfToken
    } else {
      token = await rotateCsrf()
      await identityGuard?.()
    }
  }

  let res = await send(path, method, options, token)
  if (needsCsrf && res.status === 403 && (await toApiError(res.clone())).code === 'csrf_invalid') {
    const fresh = await rotateCsrf()
    await identityGuard?.()
    res = await send(path, method, options, fresh)
  }

  if (!res.ok) {
    const error = await toApiError(res)
    if (res.status === 401 && error.code === 'unauthenticated' && options.quiet401 !== true) {
      unauthenticatedHandler?.()
    }
    throw error
  }
  if (res.status === 204) return undefined as T
  return (await res.json()) as T
}
