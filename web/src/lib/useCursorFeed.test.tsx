import { act, renderHook, waitFor } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import type { FeedPage } from '../api/operationsTypes'
import { useCursorFeed } from './useCursorFeed'
import type { Window } from './useCursorFeed'

function page(items: string[], nextCursor: string | null, from = 'F', to = 'T'): FeedPage<string> {
  return { generatedAt: 'G', from, to, items, nextCursor }
}

/** A loader that answers from a table keyed by cursor and records every call. */
function loader(table: Record<string, FeedPage<string>>) {
  const calls: { cursor: string | null; pinned: Window | null; signal: AbortSignal }[] = []
  const load = vi.fn((cursor: string | null, pinned: Window | null, signal: AbortSignal) => {
    calls.push({ cursor, pinned, signal })
    const answer = table[cursor ?? '']
    return answer ? Promise.resolve(answer) : Promise.reject(new Error(`no page for ${cursor}`))
  })
  return { load, calls }
}

function gate<T>() {
  let resolve: (value: T) => void = () => undefined
  const promise = new Promise<T>((res) => {
    resolve = res
  })
  return { promise, resolve }
}

describe('useCursorFeed', () => {
  it('loads the first page with no cursor and no pinned window', async () => {
    const { load, calls } = loader({ '': page(['a'], 'c1') })
    const { result } = renderHook(() => useCursorFeed('org', 'q', load))
    expect(result.current.loading).toBe(true)
    await waitFor(() => expect(result.current.loading).toBe(false))
    expect(calls[0]).toMatchObject({ cursor: null, pinned: null })
    expect(result.current.page?.items).toEqual(['a'])
    expect(result.current).toMatchObject({ pageNumber: 1, hasPrevious: false, hasNext: true })
  })

  it('continues with the cursor the server issued and the window of the first page', async () => {
    const { load, calls } = loader({
      '': page(['a'], 'c1', 'F1', 'T1'),
      c1: page(['b'], null, 'F2', 'T2'),
    })
    const { result } = renderHook(() => useCursorFeed('org', 'q', load))
    await waitFor(() => expect(result.current.page).not.toBeNull())
    act(() => result.current.next())
    await waitFor(() => expect(result.current.page?.items).toEqual(['b']))
    expect(calls[1]).toMatchObject({ cursor: 'c1', pinned: { from: 'F1', to: 'T1' } })
    expect(result.current).toMatchObject({ pageNumber: 2, hasPrevious: true, hasNext: false })
  })

  it('goes back and forward over held pages without another request', async () => {
    const { load } = loader({ '': page(['a'], 'c1'), c1: page(['b'], null) })
    const { result } = renderHook(() => useCursorFeed('org', 'q', load))
    await waitFor(() => expect(result.current.page).not.toBeNull())
    act(() => result.current.next())
    await waitFor(() => expect(result.current.pageNumber).toBe(2))
    expect(load).toHaveBeenCalledTimes(2)

    act(() => result.current.previous())
    expect(result.current.page?.items).toEqual(['a'])
    expect(result.current.hasNext).toBe(true)
    act(() => result.current.next())
    expect(result.current.page?.items).toEqual(['b'])
    expect(load).toHaveBeenCalledTimes(2)
  })

  it('drops every held page and cursor when the query changes, and requests the first page', async () => {
    const { load, calls } = loader({ '': page(['a'], 'c1'), c1: page(['b'], null) })
    const { result, rerender } = renderHook(({ q }) => useCursorFeed('org', q, load), {
      initialProps: { q: 'one' },
    })
    await waitFor(() => expect(result.current.page).not.toBeNull())
    act(() => result.current.next())
    await waitFor(() => expect(result.current.pageNumber).toBe(2))

    rerender({ q: 'two' })
    // Nothing of the old query is shown, and no old cursor is reused.
    expect(result.current).toMatchObject({ page: null, pageNumber: 0, loading: true })
    await waitFor(() => expect(result.current.page?.items).toEqual(['a']))
    expect(calls.at(-1)).toMatchObject({ cursor: null, pinned: null })
    expect(result.current).toMatchObject({ pageNumber: 1, hasPrevious: false })
  })

  it('hides another scope at once and cancels its request', async () => {
    const answer = gate<FeedPage<string>>()
    const signals: AbortSignal[] = []
    const load = (_cursor: string | null, _pinned: Window | null, signal: AbortSignal) => {
      signals.push(signal)
      return signals.length === 1 ? answer.promise : Promise.resolve(page(['other'], null))
    }
    const { result, rerender } = renderHook(({ scope }) => useCursorFeed(scope, 'q', load), {
      initialProps: { scope: 'org-1' },
    })
    rerender({ scope: 'org-2' })
    expect(signals[0]?.aborted).toBe(true)
    await act(async () => answer.resolve(page(['late'], null)))
    await waitFor(() => expect(result.current.page?.items).toEqual(['other']))
  })

  it('refresh starts again from the first page and asks the server for a fresh window', async () => {
    const { load, calls } = loader({ '': page(['a'], 'c1', 'F1', 'T1'), c1: page(['b'], null) })
    const { result } = renderHook(() => useCursorFeed('org', 'q', load))
    await waitFor(() => expect(result.current.page).not.toBeNull())
    act(() => result.current.next())
    await waitFor(() => expect(result.current.pageNumber).toBe(2))

    act(() => result.current.refresh())
    await waitFor(() => expect(result.current.pageNumber).toBe(1))
    expect(calls.at(-1)).toMatchObject({ cursor: null, pinned: null })
  })

  it('keeps the page shown while a refresh runs', async () => {
    const second = gate<FeedPage<string>>()
    let reads = 0
    const load = () => {
      reads += 1
      return reads === 1 ? Promise.resolve(page(['old'], null)) : second.promise
    }
    const { result } = renderHook(() => useCursorFeed('org', 'q', load))
    await waitFor(() => expect(result.current.page).not.toBeNull())
    act(() => result.current.refresh())
    expect(result.current).toMatchObject({ loading: true })
    expect(result.current.page?.items).toEqual(['old'])
    await act(async () => second.resolve(page(['new'], null)))
    expect(result.current.page?.items).toEqual(['new'])
  })

  it('keeps the current page when the next page fails, and retries the same cursor', async () => {
    let failNext = true
    const calls: (string | null)[] = []
    const load = (cursor: string | null) => {
      calls.push(cursor)
      if (cursor === 'c1' && failNext) return Promise.reject(new Error('boom'))
      return Promise.resolve(cursor === null ? page(['a'], 'c1') : page(['b'], null))
    }
    const { result } = renderHook(() => useCursorFeed('org', 'q', load))
    await waitFor(() => expect(result.current.page).not.toBeNull())
    act(() => result.current.next())
    await waitFor(() => expect(result.current.error).not.toBeNull())
    expect(result.current.page?.items).toEqual(['a'])
    expect(result.current.loading).toBe(false)

    failNext = false
    act(() => result.current.retry())
    await waitFor(() => expect(result.current.page?.items).toEqual(['b']))
    expect(result.current.error).toBeNull()
    expect(calls).toEqual([null, 'c1', 'c1'])
  })

  it('shows an error and no page when the first page fails, and retry loads it', async () => {
    let fail = true
    const load = () =>
      fail ? Promise.reject(new Error('boom')) : Promise.resolve(page(['a'], null))
    const { result } = renderHook(() => useCursorFeed('org', 'q', load))
    await waitFor(() => expect(result.current.error).not.toBeNull())
    expect(result.current.page).toBeNull()
    fail = false
    act(() => result.current.retry())
    await waitFor(() => expect(result.current.page?.items).toEqual(['a']))
  })

  it('cancels the request on unmount', () => {
    let signal: AbortSignal | undefined
    const { unmount } = renderHook(() =>
      useCursorFeed('org', 'q', (_c, _p, s) => {
        signal = s
        return new Promise<FeedPage<string>>(() => undefined)
      }),
    )
    unmount()
    expect(signal?.aborted).toBe(true)
  })

  it('gives each feed its own cursors', async () => {
    const failures = loader({ '': page(['f1'], 'v1-cursor') })
    const attempts = loader({ '': page(['a1'], 'a1-cursor'), 'a1-cursor': page(['a2'], null) })
    const first = renderHook(() => useCursorFeed('org', 'q', failures.load))
    const second = renderHook(() => useCursorFeed('org', 'q', attempts.load))
    await waitFor(() => expect(first.result.current.page).not.toBeNull())
    await waitFor(() => expect(second.result.current.page).not.toBeNull())
    act(() => second.result.current.next())
    await waitFor(() => expect(second.result.current.pageNumber).toBe(2))
    expect(failures.calls.map((c) => c.cursor)).toEqual([null])
    expect(attempts.calls.map((c) => c.cursor)).toEqual([null, 'a1-cursor'])
  })
})
