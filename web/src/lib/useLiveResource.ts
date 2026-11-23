import { useAutoRefresh } from './useAutoRefresh'
import { useResource } from './useResource'
import type { Resource } from './useResource'

/**
 * A resource that reloads itself on a schedule (see `useAutoRefresh`) and can still be reloaded by hand. The first
 * read is made on entry, and a change of `scope` aborts the request and hides the previous scope's data, exactly as
 * with `useResource`. A failed reload keeps the data that was already shown.
 */
export function useLiveResource<T>(
  scope: string,
  load: (signal: AbortSignal) => Promise<T>,
): Resource<T> {
  const resource = useResource(scope, load)
  useAutoRefresh({ busy: resource.loading, run: resource.reloadQuietly })
  return resource
}
