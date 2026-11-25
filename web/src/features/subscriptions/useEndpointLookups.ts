import { useEffect, useRef, useState } from 'react'
import { isAbortError } from '../../api/errors'
import { getIngestionEndpoint } from '../../api/ingestionEndpoints'
import type { IngestionEndpoint } from '../../api/ingestionEndpointTypes'

export type EndpointLookup =
  { state: 'loading' } | { state: 'loaded'; endpoint: IngestionEndpoint } | { state: 'failed' }

interface Store {
  orgId: string
  ok: Record<string, IngestionEndpoint>
  failed: Record<string, true>
}

/** At most this many endpoint reads run at the same time. A page has at most 25 distinct endpoints. */
const CONCURRENCY = 5

/** The first characters of an id: the fallback label when an endpoint cannot be read. */
export function shortId(id: string): string {
  return id.slice(0, 8)
}

/**
 * Looks up exactly the endpoints named by `ids` (the distinct endpoints of the rows on screen) with the endpoint
 * detail API, a few at a time. The subscription API does not return endpoint names, and nothing unrelated is
 * fetched. A failed lookup is shown as a short id and is tried again only when `reloadKey` changes (the owner's
 * page was reloaded), never in a loop. Results of another organization, or of a request that was aborted when the
 * organization or the ids changed, are never used.
 */
export function useEndpointLookups(
  orgId: string,
  ids: readonly string[],
  reloadKey: unknown,
): (id: string) => EndpointLookup {
  const [store, setStore] = useState<Store>({ orgId, ok: {}, failed: {} })
  const latest = useRef(store)
  useEffect(() => {
    latest.current = store
  })
  const idsKey = [...new Set(ids)].sort().join(',')

  useEffect(() => {
    const controller = new AbortController()
    const have = latest.current.orgId === orgId ? latest.current.ok : {}
    const todo = (idsKey === '' ? [] : idsKey.split(',')).filter((id) => !(id in have))
    let next = 0

    const record = (update: (previous: Store) => Store) =>
      setStore((previous) =>
        update(previous.orgId === orgId ? previous : { orgId, ok: {}, failed: {} }),
      )

    async function worker() {
      while (next < todo.length && !controller.signal.aborted) {
        const id = todo[next++]!
        try {
          const endpoint = await getIngestionEndpoint(orgId, id, controller.signal)
          if (controller.signal.aborted) return
          record((previous) => ({ ...previous, ok: { ...previous.ok, [id]: endpoint } }))
        } catch (caught) {
          if (controller.signal.aborted || isAbortError(caught)) return
          record((previous) => ({ ...previous, failed: { ...previous.failed, [id]: true } }))
        }
      }
    }
    for (let i = 0; i < Math.min(CONCURRENCY, todo.length); i += 1) void worker()
    return () => controller.abort()
  }, [orgId, idsKey, reloadKey])

  return (id: string): EndpointLookup => {
    if (store.orgId !== orgId) return { state: 'loading' }
    const endpoint = store.ok[id]
    if (endpoint !== undefined) return { state: 'loaded', endpoint }
    return store.failed[id] ? { state: 'failed' } : { state: 'loading' }
  }
}
