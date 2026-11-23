import { useCallback, useEffect, useEffectEvent, useState } from 'react'
import { isAbortError } from '../api/errors'

interface Settled<T> {
  /** Whose data this is (for example an organization id). */
  scope: string
  /** The request that produced it: the scope and the reload count. */
  key: string
  data: T | null
  error: unknown
}

export interface Resource<T> {
  /** The last data loaded for the current scope. It stays while a reload runs, and is null for another scope. */
  data: T | null
  /** The error of the latest request, or null while it runs or after it succeeded. */
  error: unknown
  /** True from the start of a request until it settles. */
  loading: boolean
  /**
   * True while a scheduled (quiet) reload runs over data that is already shown. Nothing should look busy then: the
   * user did not ask for it and the shown data stays valid until the new answer arrives.
   */
  background: boolean
  /** Starts a new request on behalf of the user. The previous one is aborted and its answer is ignored. */
  reload: () => void
  /** Starts a new request on a schedule. Same as `reload`, but it is reported as `background`. */
  reloadQuietly: () => void
}

/**
 * Loads one resource. `scope` says whose data it is: when it changes, the previous scope's data is hidden at once
 * (nothing from another organization is ever shown) and its request is aborted. A request that was aborted, or
 * superseded, never changes state.
 *
 * Loading is derived while rendering (the key of the request that settled differs from the current key), so no state
 * is set synchronously inside an effect.
 */
export function useResource<T>(
  scope: string,
  load: (signal: AbortSignal) => Promise<T>,
): Resource<T> {
  const [reloads, setReloads] = useState({ count: 0, quiet: false })
  const [settled, setSettled] = useState<Settled<T> | null>(null)
  const key = `${scope}#${reloads.count}`

  // The effect event always calls the latest `load` without making it a dependency of the effect.
  const run = useEffectEvent((signal: AbortSignal) => load(signal))

  useEffect(() => {
    const controller = new AbortController()
    run(controller.signal).then(
      (data) => {
        if (!controller.signal.aborted) setSettled({ scope, key, data, error: null })
      },
      (error: unknown) => {
        if (controller.signal.aborted || isAbortError(error)) return
        // A failed reload keeps what was already loaded for this scope.
        setSettled((previous) => ({
          scope,
          key,
          data: previous?.scope === scope ? previous.data : null,
          error,
        }))
      },
    )
    return () => controller.abort()
  }, [scope, key])

  const reload = useCallback(() => setReloads((r) => ({ count: r.count + 1, quiet: false })), [])
  const reloadQuietly = useCallback(
    () => setReloads((r) => ({ count: r.count + 1, quiet: true })),
    [],
  )
  const sameScope = settled !== null && settled.scope === scope
  const data = sameScope ? settled.data : null
  const loading = settled?.key !== key
  return {
    data,
    error: settled !== null && settled.key === key ? settled.error : null,
    loading,
    background: loading && reloads.quiet && data !== null,
    reload,
    reloadQuietly,
  }
}
