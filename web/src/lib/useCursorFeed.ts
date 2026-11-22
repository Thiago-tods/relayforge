import { useEffect, useEffectEvent, useState } from 'react'
import { isAbortError } from '../api/errors'
import type { FeedPage } from '../api/operationsTypes'

// A feed read with opaque cursors. The server issues a cursor with each page; this hook only stores them. It never
// decodes a cursor, builds one, or sends one with a query other than the one that produced it: changing the query
// (or pressing Refresh) starts a new run from the first page, which drops every held page and cursor.

/** The window the first page of a run was read with, as the server echoed it. */
export interface Window {
  from: string
  to: string
}

interface Run<T> {
  /** Whose data this is: the scope (organization) and the applied query. */
  base: string
  /** The run that produced it: base plus the refresh count. */
  runKey: string
  /** The last request of the run that settled. */
  seq: number
  pages: FeedPage<T>[]
  index: number
  /** The window of the first page. Continuation pages of a rolling (default) window are requested with it. */
  pinned: Window | null
  error: unknown
}

interface Request {
  runKey: string
  cursor: string | null
  seq: number
}

export interface CursorFeed<T> {
  /** The page being shown, or null before the first page of this query has loaded. */
  page: FeedPage<T> | null
  /** 1-based number of the page being shown (0 when none). */
  pageNumber: number
  /** True while a request runs. A page that is already shown stays visible meanwhile. */
  loading: boolean
  /** The error of the latest request of this run, or null. */
  error: unknown
  hasPrevious: boolean
  hasNext: boolean
  next: () => void
  previous: () => void
  /** Starts again from the first page. */
  refresh: () => void
  /** Repeats the request that failed. */
  retry: () => void
}

function settle<T>(
  previous: Run<T> | null,
  base: string,
  runKey: string,
  seq: number,
  cursor: string | null,
  page: FeedPage<T>,
): Run<T> {
  if (cursor === null || previous === null || previous.base !== base) {
    return {
      base,
      runKey,
      seq,
      pages: [page],
      index: 0,
      pinned: { from: page.from, to: page.to },
      error: null,
    }
  }
  const pages = [...previous.pages.slice(0, previous.index + 1), page]
  return { ...previous, runKey, seq, pages, index: pages.length - 1, error: null }
}

function fail<T>(
  previous: Run<T> | null,
  base: string,
  runKey: string,
  seq: number,
  error: unknown,
): Run<T> {
  // A failed reload keeps the pages already shown for this same query.
  if (previous === null || previous.base !== base) {
    return { base, runKey, seq, pages: [], index: 0, pinned: null, error }
  }
  return { ...previous, runKey, seq, error }
}

/**
 * `scope` says whose data this is (the organization id) and `queryKey` identifies the applied filters: when either
 * changes, what was shown is hidden at once and its requests are aborted. `load` receives the cursor to continue
 * from (null for the first page), the window of the first page for continuation pages (null for the first page, so
 * the server picks its own default window), and an abort signal.
 */
export function useCursorFeed<T>(
  scope: string,
  queryKey: string,
  load: (cursor: string | null, pinned: Window | null, signal: AbortSignal) => Promise<FeedPage<T>>,
): CursorFeed<T> {
  const base = `${scope}|${queryKey}`
  const [attempt, setAttempt] = useState(0)
  const runKey = `${base}#${attempt}`
  const [request, setRequest] = useState<Request>({ runKey: '', cursor: null, seq: 0 })
  const [run, setRun] = useState<Run<T> | null>(null)

  const active: Request = request.runKey === runKey ? request : { runKey, cursor: null, seq: 0 }
  const cursor = active.cursor
  const seq = active.seq
  const shown = run !== null && run.base === base ? run : null

  const start = useEffectEvent((signal: AbortSignal) =>
    load(cursor, cursor === null ? null : (shown?.pinned ?? null), signal),
  )

  useEffect(() => {
    const controller = new AbortController()
    start(controller.signal).then(
      (page) => {
        if (!controller.signal.aborted) {
          setRun((previous) => settle(previous, base, runKey, seq, cursor, page))
        }
      },
      (error: unknown) => {
        if (controller.signal.aborted || isAbortError(error)) return
        setRun((previous) => fail(previous, base, runKey, seq, error))
      },
    )
    return () => controller.abort()
  }, [base, runKey, cursor, seq])

  const settled = run !== null && run.runKey === runKey && run.seq === seq
  const page = shown !== null && shown.pages.length > 0 ? (shown.pages[shown.index] ?? null) : null
  const heldNext = shown !== null && shown.index < shown.pages.length - 1

  return {
    page,
    pageNumber: page === null || shown === null ? 0 : shown.index + 1,
    loading: !settled,
    error: settled ? run.error : null,
    hasPrevious: shown !== null && shown.index > 0,
    hasNext: heldNext || (page !== null && page.nextCursor !== null),
    next: () => {
      if (heldNext) {
        setRun((previous) =>
          previous ? { ...previous, index: previous.index + 1, error: null } : previous,
        )
      } else if (page?.nextCursor) setRequest({ runKey, cursor: page.nextCursor, seq: seq + 1 })
    },
    previous: () =>
      setRun((previous) =>
        previous && previous.index > 0
          ? { ...previous, index: previous.index - 1, error: null }
          : previous,
      ),
    refresh: () => setAttempt((n) => n + 1),
    retry: () => setRequest({ runKey, cursor, seq: seq + 1 }),
  }
}
