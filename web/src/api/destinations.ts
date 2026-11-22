import { request } from './client'
import { parseDestinationPage } from './operationsParse'
import type { DestinationPage } from './operationsTypes'

/**
 * One page of the organization's destinations, newest first (limit 1 to 100, offset-based, with an exact total).
 * Used only to name a destination in a filter: the result carries id, name and status, never the URL.
 */
export async function listDestinations(
  orgId: string,
  page: { limit: number; offset: number },
  signal?: AbortSignal,
): Promise<DestinationPage> {
  const query = new URLSearchParams({ limit: String(page.limit), offset: String(page.offset) })
  const body = await request<unknown>(
    `/organizations/${encodeURIComponent(orgId)}/destinations?${query.toString()}`,
    { signal },
  )
  return parseDestinationPage(body)
}
