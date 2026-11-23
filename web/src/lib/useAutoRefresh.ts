import { useEffect, useEffectEvent, useRef, useSyncExternalStore } from 'react'

/** How often the operations pages refresh on their own while the tab is visible. */
export const REFRESH_INTERVAL_MS = 15_000

function subscribe(onChange: () => void): () => void {
  document.addEventListener('visibilitychange', onChange)
  return () => document.removeEventListener('visibilitychange', onChange)
}

/** Whether the browser tab is visible. Reads the document, so it follows the real tab state. */
export function useTabVisible(): boolean {
  return useSyncExternalStore(
    subscribe,
    () => document.visibilityState !== 'hidden',
    () => true,
  )
}

/**
 * Calls `run` on a schedule. The timer is chained, not periodic: it is armed only while no request is running
 * (`busy` is false) and the tab is visible, and it is cleared the moment a request starts. So two requests of the
 * same resource never overlap, and a tick that would have fallen while a request ran is dropped, never queued.
 * While the tab is hidden nothing is scheduled; when it becomes visible again `run` is called at once (unless a
 * request is already running), and the schedule resumes after that request settles.
 *
 * `run` must start a request that sets `busy`. The first read on entry is not made here: the resource hook does it.
 */
export function useAutoRefresh({
  busy,
  run,
  enabled = true,
  intervalMs = REFRESH_INTERVAL_MS,
}: {
  busy: boolean
  run: () => void
  enabled?: boolean
  intervalMs?: number
}): void {
  const visible = useTabVisible()
  const fire = useEffectEvent(run)

  useEffect(() => {
    if (!enabled || busy || !visible) return
    const timer = window.setTimeout(() => fire(), intervalMs)
    return () => window.clearTimeout(timer)
  }, [enabled, busy, visible, intervalMs])

  // A tab that comes back is refreshed at once instead of waiting out the rest of an interval.
  const wasVisible = useRef(visible)
  useEffect(() => {
    const returned = visible && !wasVisible.current
    wasVisible.current = visible
    if (returned && enabled && !busy) fire()
  }, [visible, enabled, busy])
}
