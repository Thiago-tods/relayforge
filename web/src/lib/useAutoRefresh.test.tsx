import { act, renderHook } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { REFRESH_INTERVAL_MS, useAutoRefresh } from './useAutoRefresh'

function setVisibility(state: 'visible' | 'hidden') {
  Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => state })
  act(() => {
    document.dispatchEvent(new Event('visibilitychange'))
  })
}

function tick(ms: number) {
  act(() => {
    vi.advanceTimersByTime(ms)
  })
}

beforeEach(() => vi.useFakeTimers())
afterEach(() => {
  vi.useRealTimers()
  Reflect.deleteProperty(document, 'visibilityState')
})

describe('useAutoRefresh', () => {
  it('runs once per interval while nothing is running', () => {
    const run = vi.fn()
    const view = renderHook((props) => useAutoRefresh(props), {
      initialProps: { busy: false, run },
    })
    tick(REFRESH_INTERVAL_MS - 1)
    expect(run).not.toHaveBeenCalled()
    tick(1)
    expect(run).toHaveBeenCalledTimes(1)
    // The request the call started runs, then settles: only then is the next tick armed.
    view.rerender({ busy: true, run })
    tick(REFRESH_INTERVAL_MS * 3)
    expect(run).toHaveBeenCalledTimes(1)
    view.rerender({ busy: false, run })
    tick(REFRESH_INTERVAL_MS)
    expect(run).toHaveBeenCalledTimes(2)
  })

  it('never overlaps a request and does not queue the ticks it skipped', () => {
    const run = vi.fn()
    const view = renderHook((props) => useAutoRefresh(props), {
      initialProps: { busy: true, run },
    })
    tick(REFRESH_INTERVAL_MS * 10)
    expect(run).not.toHaveBeenCalled()
    view.rerender({ busy: false, run })
    // Ten missed intervals do not become ten refreshes, and not even one at once: the schedule starts again.
    expect(run).not.toHaveBeenCalled()
    tick(REFRESH_INTERVAL_MS)
    expect(run).toHaveBeenCalledTimes(1)
  })

  it('starts the interval again when a request settles, so a manual refresh resets the schedule', () => {
    const run = vi.fn()
    const view = renderHook((props) => useAutoRefresh(props), {
      initialProps: { busy: false, run },
    })
    tick(REFRESH_INTERVAL_MS - 1000)
    view.rerender({ busy: true, run }) // a manual refresh starts
    view.rerender({ busy: false, run }) // and settles
    tick(REFRESH_INTERVAL_MS - 1)
    expect(run).not.toHaveBeenCalled()
    tick(1)
    expect(run).toHaveBeenCalledTimes(1)
  })

  it('schedules nothing while the tab is hidden and runs at once when it is visible again', () => {
    const run = vi.fn()
    renderHook(() => useAutoRefresh({ busy: false, run }))
    tick(REFRESH_INTERVAL_MS - 5000)
    setVisibility('hidden')
    tick(REFRESH_INTERVAL_MS * 4)
    expect(run).not.toHaveBeenCalled()
    setVisibility('visible')
    expect(run).toHaveBeenCalledTimes(1)
  })

  it('does not start a second request when the tab returns while one is running', () => {
    const run = vi.fn()
    const view = renderHook((props) => useAutoRefresh(props), {
      initialProps: { busy: false, run },
    })
    setVisibility('hidden')
    view.rerender({ busy: true, run })
    setVisibility('visible')
    expect(run).not.toHaveBeenCalled()
    // It settles later: the schedule resumes with a full interval, not an immediate repeat.
    view.rerender({ busy: false, run })
    expect(run).not.toHaveBeenCalled()
    tick(REFRESH_INTERVAL_MS)
    expect(run).toHaveBeenCalledTimes(1)
  })

  it('does nothing when disabled', () => {
    const run = vi.fn()
    renderHook(() => useAutoRefresh({ busy: false, run, enabled: false }))
    tick(REFRESH_INTERVAL_MS * 3)
    setVisibility('hidden')
    setVisibility('visible')
    expect(run).not.toHaveBeenCalled()
  })

  it('clears its timer on unmount and removes its listener', () => {
    const run = vi.fn()
    const removed = vi.spyOn(document, 'removeEventListener')
    const view = renderHook(() => useAutoRefresh({ busy: false, run }))
    view.unmount()
    expect(vi.getTimerCount()).toBe(0)
    expect(removed).toHaveBeenCalledWith('visibilitychange', expect.any(Function))
    tick(REFRESH_INTERVAL_MS * 3)
    expect(run).not.toHaveBeenCalled()
    removed.mockRestore()
  })

  it('always calls the latest run function', () => {
    const first = vi.fn()
    const second = vi.fn()
    const view = renderHook((props) => useAutoRefresh(props), {
      initialProps: { busy: false, run: first },
    })
    view.rerender({ busy: false, run: second })
    tick(REFRESH_INTERVAL_MS)
    expect(first).not.toHaveBeenCalled()
    expect(second).toHaveBeenCalledTimes(1)
  })
})
