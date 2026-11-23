import { act, renderHook } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { FeedPage } from '../api/operationsTypes'
import { REFRESH_INTERVAL_MS } from './useAutoRefresh'
import { hasNewer, useNewActivity } from './useNewActivity'

interface Row {
  id: string
  at: string
}

const page = (...rows: Row[]): FeedPage<Row> => ({
  generatedAt: '2026-11-22T17:30:01Z',
  from: '2026-11-15T17:30:00Z',
  to: '2026-11-22T17:30:00Z',
  items: rows,
  nextCursor: null,
})
const row = (id: string, at: string): Row => ({ id, at })
const id = (item: Row) => item.id
const time = (item: Row) => item.at

describe('hasNewer', () => {
  const held = page(row('b', '2026-11-22T10:02:00Z'), row('a', '2026-11-22T10:01:00Z'))

  it('is true for an entry that is not held and is not older than the newest held one', () => {
    expect(hasNewer(held, page(row('c', '2026-11-22T10:03:00Z')), id, time)).toBe(true)
    expect(hasNewer(held, page(row('c', '2026-11-22T10:02:00Z')), id, time)).toBe(true)
  })

  it('is false when the newest entry is already held', () => {
    expect(hasNewer(held, page(row('b', '2026-11-22T10:02:00Z')), id, time)).toBe(false)
    expect(hasNewer(held, page(row('a', '2026-11-22T10:01:00Z')), id, time)).toBe(false)
  })

  it('is false for an older entry that only moved up because a held one left the feed', () => {
    expect(hasNewer(held, page(row('z', '2026-11-22T09:00:00Z')), id, time)).toBe(false)
  })

  it('is true for the first entry of a feed that was empty, and false for an empty answer', () => {
    expect(hasNewer(page(), page(row('a', '2026-11-22T10:01:00Z')), id, time)).toBe(true)
    expect(hasNewer(held, page(), id, time)).toBe(false)
  })

  it('is false before the first page is held or without an answer', () => {
    expect(hasNewer(null, page(row('a', '2026-11-22T10:01:00Z')), id, time)).toBe(false)
    expect(hasNewer(held, null, id, time)).toBe(false)
  })
})

describe('useNewActivity', () => {
  beforeEach(() => vi.useFakeTimers())
  afterEach(() => vi.useRealTimers())

  async function advance(ms: number) {
    await act(async () => {
      await vi.advanceTimersByTimeAsync(ms)
    })
  }

  const first = page(row('b', '2026-11-22T10:02:00Z'))

  it('reports a newer entry after one interval and then keeps checking', async () => {
    const probe = vi.fn(() => Promise.resolve(page(row('c', '2026-11-22T10:03:00Z'))))
    const view = renderHook(() =>
      useNewActivity({ base: 'o1|q', first, busy: false, probe, id, time }),
    )
    expect(view.result.current).toEqual({ available: false, failed: false })
    expect(probe).not.toHaveBeenCalled()
    await advance(REFRESH_INTERVAL_MS)
    expect(probe).toHaveBeenCalledTimes(1)
    expect(view.result.current).toEqual({ available: true, failed: false })
    await advance(REFRESH_INTERVAL_MS)
    expect(probe).toHaveBeenCalledTimes(2)
  })

  it('does not check while no page is held or while the feed itself is loading', async () => {
    const probe = vi.fn(() => Promise.resolve(page(row('c', '2026-11-22T10:03:00Z'))))
    const view = renderHook((props) => useNewActivity(props), {
      initialProps: {
        base: 'o1|q',
        first: null as FeedPage<Row> | null,
        busy: false,
        probe,
        id,
        time,
      },
    })
    await advance(REFRESH_INTERVAL_MS * 3)
    view.rerender({ base: 'o1|q', first, busy: true, probe, id, time })
    await advance(REFRESH_INTERVAL_MS * 3)
    expect(probe).not.toHaveBeenCalled()
  })

  it('never overlaps a check: the next one is armed only after the answer', async () => {
    let release: (value: FeedPage<Row>) => void = () => undefined
    const probe = vi.fn(
      () =>
        new Promise<FeedPage<Row>>((resolve) => {
          release = resolve
        }),
    )
    renderHook(() => useNewActivity({ base: 'o1|q', first, busy: false, probe, id, time }))
    await advance(REFRESH_INTERVAL_MS)
    expect(probe).toHaveBeenCalledTimes(1)
    await advance(REFRESH_INTERVAL_MS * 5)
    expect(probe).toHaveBeenCalledTimes(1)
    await act(async () => release(first))
    await advance(REFRESH_INTERVAL_MS)
    expect(probe).toHaveBeenCalledTimes(2)
  })

  it('reports a failed check without claiming anything is new, and recovers', async () => {
    let fail = true
    const probe = vi.fn(() => (fail ? Promise.reject(new Error('down')) : Promise.resolve(first)))
    const view = renderHook(() =>
      useNewActivity({ base: 'o1|q', first, busy: false, probe, id, time }),
    )
    await advance(REFRESH_INTERVAL_MS)
    expect(view.result.current).toEqual({ available: false, failed: true })
    fail = false
    await advance(REFRESH_INTERVAL_MS)
    expect(view.result.current).toEqual({ available: false, failed: false })
  })

  it('forgets an answer and aborts its request when the query changes', async () => {
    const signals: AbortSignal[] = []
    const probe = vi.fn((signal: AbortSignal) => {
      signals.push(signal)
      return Promise.resolve(page(row('c', '2026-11-22T10:03:00Z')))
    })
    const view = renderHook((props) => useNewActivity(props), {
      initialProps: { base: 'o1|a', first, busy: false, probe, id, time },
    })
    await advance(REFRESH_INTERVAL_MS)
    expect(view.result.current.available).toBe(true)
    view.rerender({ base: 'o1|b', first, busy: false, probe, id, time })
    expect(view.result.current.available).toBe(false)
    expect(signals[0]?.aborted).toBe(true)
    // The new query waits a whole interval before its own first check.
    expect(probe).toHaveBeenCalledTimes(1)
  })

  it('aborts a running check on unmount', async () => {
    const signals: AbortSignal[] = []
    const probe = vi.fn((signal: AbortSignal) => {
      signals.push(signal)
      return new Promise<FeedPage<Row>>(() => undefined)
    })
    const view = renderHook(() =>
      useNewActivity({ base: 'o1|q', first, busy: false, probe, id, time }),
    )
    await advance(REFRESH_INTERVAL_MS)
    expect(signals).toHaveLength(1)
    view.unmount()
    expect(signals[0]?.aborted).toBe(true)
  })
})
