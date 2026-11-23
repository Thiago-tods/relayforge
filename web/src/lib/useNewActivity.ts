import { useEffect, useEffectEvent, useState } from 'react'
import { isAbortError } from '../api/errors'
import type { FeedPage } from '../api/operationsTypes'
import { useAutoRefresh } from './useAutoRefresh'

// Detects newer entries of a feed without touching what the user is looking at. On a schedule it reads only the
// newest entry of the same query (limit 1, no cursor). It never replaces a held page, never drops a cursor and never
// moves the user: it only reports that something newer exists. The user decides to load it.

interface Probe<T> {
  /** The query the answer belongs to. An answer for another query is ignored. */
  base: string
  seq: number
  page: FeedPage<T> | null
  failed: boolean
}

export interface NewActivity {
  /** A newer entry than the first page held now exists. */
  available: boolean
  /** The latest check failed. What is shown stays valid. */
  failed: boolean
}

/**
 * True when the newest entry of `probe` is newer than everything held in `first`: it is not one of the held entries
 * and it is not older than the newest held one. An entry that only moved up because a held one left the feed (for
 * example a redriven delivery) is already held, so it is not reported.
 */
export function hasNewer<T>(
  first: FeedPage<T> | null,
  probe: FeedPage<T> | null,
  id: (item: T) => string,
  time: (item: T) => string,
): boolean {
  const top = probe?.items[0]
  if (first === null || top === undefined) return false
  if (first.items.some((item) => id(item) === id(top))) return false
  const held = first.items[0]
  if (held === undefined) return true
  const a = Date.parse(time(top))
  const b = Date.parse(time(held))
  return Number.isNaN(a) || Number.isNaN(b) ? true : a >= b
}

/**
 * `base` identifies the applied query (organization and filters). `first` is the first page held for it. `busy` is
 * true while the feed itself loads a page. `probe` reads the newest entry of the same query with a fresh window
 * where the server picks its own, and `id` and `time` name an entry and its ordering instant.
 */
export function useNewActivity<T>({
  base,
  first,
  busy,
  probe,
  id,
  time,
}: {
  base: string
  first: FeedPage<T> | null
  busy: boolean
  probe: (signal: AbortSignal) => Promise<FeedPage<T>>
  id: (item: T) => string
  time: (item: T) => string
}): NewActivity {
  const [asked, setAsked] = useState({ base, seq: 0 })
  const [answer, setAnswer] = useState<Probe<T> | null>(null)
  const seq = asked.base === base ? asked.seq : 0

  // The effect event reads the latest `probe` without making it a dependency: the request belongs to the query
  // and to the request counter, not to the identity of a function that is rebuilt on every render.
  const read = useEffectEvent((signal: AbortSignal) => probe(signal))

  useEffect(() => {
    if (seq === 0) return
    const controller = new AbortController()
    read(controller.signal).then(
      (page) => {
        if (!controller.signal.aborted) setAnswer({ base, seq, page, failed: false })
      },
      (error: unknown) => {
        if (controller.signal.aborted || isAbortError(error)) return
        setAnswer({ base, seq, page: null, failed: true })
      },
    )
    return () => controller.abort()
  }, [base, seq])

  const current = answer !== null && answer.base === base ? answer : null
  const probing = seq > 0 && (current === null || current.seq !== seq)

  useAutoRefresh({
    busy: busy || probing,
    enabled: first !== null,
    run: () => setAsked({ base, seq: seq + 1 }),
  })

  return {
    available: current !== null && hasNewer(first, current.page, id, time),
    failed: current?.failed ?? false,
  }
}
