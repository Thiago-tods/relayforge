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

/** A delivery summary. Mocked data: the scratch database holds no deliveries. */
export function deliverySummary(over: Record<string, unknown> = {}) {
  return {
    generatedAt: '2026-11-22T17:30:00.123456Z',
    from: '2026-11-21T17:30:00.123456Z',
    to: '2026-11-22T17:30:00.123456Z',
    created: {
      total: 40,
      byStatus: { PENDING: 2, IN_FLIGHT: 1, SUCCEEDED: 30, FAILED: 6, CANCELLED: 1 },
    },
    completed: {
      succeeded: 30,
      failed: 6,
      cancelled: 1,
      failureRate: 6 / 36,
      byFailureReason: {
        DESTINATION_REJECTED: 3,
        REDIRECT_REFUSED: 0,
        EGRESS_BLOCKED: 1,
        RETRIES_EXHAUSTED: 1,
        INTERNAL_ERROR: 0,
        UNRECORDED: 1,
      },
    },
    capped: false,
    ...over,
  }
}

export const DELIVERY_A = '11111111-1111-4111-8111-111111111111'
export const DELIVERY_B = '22222222-2222-4222-8222-222222222222'
export const DEST_A = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa'
export const DEST_B = 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb'

export function failureItem(over: Record<string, unknown> = {}) {
  return {
    deliveryId: DELIVERY_A,
    eventId: 'eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee',
    destinationId: DEST_A,
    destinationName: 'Billing hook',
    failureReason: 'DESTINATION_REJECTED',
    attemptCount: 3,
    lastHttpStatus: 410,
    lastErrorClass: 'http_4xx',
    createdAt: '2026-11-22T10:00:00Z',
    completedAt: '2026-11-22T11:00:00.5Z',
    ...over,
  }
}

export function attemptItem(over: Record<string, unknown> = {}) {
  return {
    attemptId: 'cccccccc-cccc-4ccc-8ccc-cccccccccccc',
    deliveryId: DELIVERY_A,
    eventId: 'eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee',
    destinationId: DEST_A,
    destinationName: 'Billing hook',
    attemptNumber: 3,
    startedAt: '2026-11-22T11:00:00Z',
    finishedAt: '2026-11-22T11:00:01Z',
    durationMs: 1000,
    outcome: 'HTTP_ERROR',
    httpStatus: 410,
    errorClass: 'http_4xx',
    ...over,
  }
}

/** A feed page. `nextCursor` is an opaque stand-in: the app must pass it back untouched. */
export function feedPage(
  items: unknown[],
  nextCursor: string | null = null,
  window = { from: '2026-11-15T17:30:00.5Z', to: '2026-11-22T17:30:00.5Z' },
) {
  return { generatedAt: '2026-11-22T17:30:01Z', ...window, items, nextCursor }
}

/** A destinations page. The url is included on purpose: the app must never keep or show it. */
export function destinationPage(
  items: { id: string; name: string; status?: string }[],
  total: number,
  offset = 0,
) {
  return {
    items: items.map((item) => ({
      id: item.id,
      organizationId: 'o1',
      name: item.name,
      status: item.status ?? 'ACTIVE',
      url: 'https://secret-host.example/hook',
      urlHasQuery: true,
      createdAt: '2026-01-01T00:00:00Z',
      updatedAt: '2026-01-01T00:00:00Z',
    })),
    total,
    limit: 100,
    offset,
  }
}

/** The body of GET /auth/me, expiring far in the future unless told otherwise. */
export function meBody(organizations: OrganizationWithRole[], expiresAt = '2099-01-01T00:00:00Z') {
  return { user: USER, session: { expiresAt }, organizations }
}
