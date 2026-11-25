import type { OrganizationWithRole } from '../api/types'
import { apiError, json, meBody, org, stubFetch } from './fetchStub'
import type { Handler, Recorded } from './fetchStub'
import { fakeChannel, renderApp } from './renderApp'

// A stubbed fetch for the destination pages. Routes are "METHOD /pathname" (the query is not part of the key);
// anything else is a 404. Nothing here talks to a real backend.

export interface HarnessSetup {
  role?: OrganizationWithRole['role']
  routes?: Record<string, Handler>
}

export function mountDestinations(path: string, setup: HarnessSetup = {}) {
  fakeChannel()
  const routes = setup.routes ?? {}
  const calls = stubFetch((call) => {
    if (call.path === '/api/v1/auth/me') {
      return json(200, meBody([org('o1', setup.role), org('o2', setup.role)]))
    }
    if (call.path === '/api/v1/auth/csrf') return json(200, { session: { csrfToken: 'tok' } })
    const route = routes[`${call.method} ${call.path.split('?')[0]}`]
    return route ? route(call) : apiError(404, 'not_found', 'resource not found')
  })
  return { calls, ...renderApp(path) }
}

export const requests = (calls: Recorded[], method: string, pathname: string) =>
  calls.filter((call) => call.method === method && call.path.split('?')[0] === pathname)
