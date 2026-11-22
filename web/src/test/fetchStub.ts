import { vi } from 'vitest'
import type { OrganizationWithRole, User } from '../api/types'

// Tests use a stubbed fetch. Nothing here talks to a real backend, so these tests are not end-to-end evidence.

export interface Recorded {
  path: string
  method: string
  headers: Headers
  credentials: RequestCredentials | undefined
  body: unknown
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

/** The body of GET /auth/me, expiring far in the future unless told otherwise. */
export function meBody(organizations: OrganizationWithRole[], expiresAt = '2099-01-01T00:00:00Z') {
  return { user: USER, session: { expiresAt }, organizations }
}
