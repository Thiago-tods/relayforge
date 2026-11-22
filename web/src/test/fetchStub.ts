import { vi } from 'vitest'
import type { QueueAge, QueueSnapshot } from '../api/operationsTypes'
import type { OrganizationWithRole, User } from '../api/types'

// Tests use a stubbed fetch. Nothing here talks to a real backend, so these tests are not end-to-end evidence.

export interface Recorded {
  path: string
  method: string
  headers: Headers
  credentials: RequestCredentials | undefined
  body: unknown
  /** The abort signal of the request, so a test can check that an obsolete request was cancelled. */
  signal: AbortSignal | null | undefined
}

export type Handler = (call: Recorded) => Response | Promise<Response>

/** Replaces fetch with a handler and returns the list of calls it received. */
export function stubFetch(handler: Handler): Recorded[] {
  const calls: Recorded[] = []
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: string, init: RequestInit = {}) => {
      const call: Recorded = {
        path: input,
        method: init.method ?? 'GET',
        headers: new Headers(init.headers),
        credentials: init.credentials,
        body: typeof init.body === 'string' ? (JSON.parse(init.body) as unknown) : undefined,
        signal: init.signal,
      }
      calls.push(call)
      return handler(call)
    }),
  )
  return calls
}

export function json(
  status: number,
  body: unknown,
  headers: Record<string, string> = {},
): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json', ...headers },
  })
}

export function noContent(): Response {
  return new Response(null, { status: 204 })
}

export function apiError(
  status: number,
  code: string,
  message: string,
  headers: Record<string, string> = {},
): Response {
  return json(status, { error: { code, message } }, headers)
}

export const USER: User = {
  id: 'u-1',
  email: 'ada@example.com',
  displayName: 'Ada',
  status: 'ACTIVE',
  createdAt: '2026-01-01T00:00:00Z',
}

export function org(
  id: string,
  role: OrganizationWithRole['role'] = 'OWNER',
): OrganizationWithRole {
  return {
    id,
    name: `Org ${id}`,
    slug: `org-${id}`,
    role,
    createdAt: '2026-01-01T00:00:00Z',
    updatedAt: '2026-01-01T00:00:00Z',
  }
}

/** The labels and ranges of the seven fixed queue-age buckets (internal/delivery/queue_age.go). */
const AGE_BUCKETS: [string, number, number | null][] = [
  ['<1m', 0, 60],
  ['1-5m', 60, 300],
  ['5-15m', 300, 900],
  ['15-60m', 900, 3600],
  ['1-6h', 3600, 21600],
  ['6-24h', 21600, 86400],
  ['>24h', 86400, null],
]

export function queueSnapshot(over: Partial<QueueSnapshot> = {}): QueueSnapshot {
  return {
    generatedAt: '2026-11-22T17:27:03Z',
    pending: 12,
    due: 9,
    delayed: 3,
    oldestDueAgeSeconds: 3661,
    inFlight: 4,
    expiredLeases: 1,
    capped: false,
    ...over,
  }
}

/** A queue-age response with one count per bucket, in the API's bucket order. */
export function queueAge(counts: number[] = [5, 2, 1, 0, 1, 0, 0], capped = false): QueueAge {
  return {
    generatedAt: '2026-11-22T17:27:04Z',
    buckets: AGE_BUCKETS.map(([label, minSeconds, maxSeconds], i) => ({
      label,
      minSeconds,
      maxSeconds,
      count: counts[i] ?? 0,
    })),
    capped,
  }
}

/** The body of GET /auth/me, expiring far in the future unless told otherwise. */
export function meBody(organizations: OrganizationWithRole[], expiresAt = '2099-01-01T00:00:00Z') {
  return { user: USER, session: { expiresAt }, organizations }
}
