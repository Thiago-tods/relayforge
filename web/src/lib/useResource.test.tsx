import { act, renderHook, waitFor } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { useResource } from './useResource'

/** A promise a test settles by hand. */
function gate<T>() {
  let resolve: (value: T) => void = () => undefined
  let reject: (error: unknown) => void = () => undefined
  const promise = new Promise<T>((res, rej) => {
    resolve = res
    reject = rej
  })
  return { promise, resolve, reject }
}

describe('useResource', () => {
  it('loads, then exposes the data', async () => {
    const { result } = renderHook(() => useResource('a', () => Promise.resolve('data-a')))
    expect(result.current).toMatchObject({ data: null, loading: true })
    await waitFor(() => expect(result.current.loading).toBe(false))
    expect(result.current).toMatchObject({ data: 'data-a', error: null })
  })

  it('aborts the request on unmount', () => {
    let signal: AbortSignal | undefined
    const { unmount } = renderHook(() =>
      useResource('a', (s) => {
        signal = s
        return new Promise<string>(() => undefined)
      }),
    )
    expect(signal?.aborted).toBe(false)
    unmount()
    expect(signal?.aborted).toBe(true)
  })

  it('hides the previous scope at once and ignores its late answer', async () => {
    const answers = new Map<string, ReturnType<typeof gate<string>>>([
      ['a', gate<string>()],
      ['b', gate<string>()],
    ])
    const signals = new Map<string, AbortSignal>()
    const { result, rerender } = renderHook(
      ({ scope }) =>
        useResource(scope, (s) => {
          signals.set(scope, s)
          return answers.get(scope)!.promise
        }),
      { initialProps: { scope: 'a' } },
    )
    await act(async () => answers.get('a')!.resolve('data-a'))
    expect(result.current.data).toBe('data-a')

    rerender({ scope: 'b' })
    // Nothing of scope "a" is shown while "b" loads, and its request is cancelled.
    expect(result.current).toMatchObject({ data: null, loading: true })
    expect(signals.get('b')?.aborted).toBe(false)

    await act(async () => answers.get('b')!.resolve('data-b'))
    expect(result.current.data).toBe('data-b')

    // A late answer for "a" changes nothing.
    await act(async () => answers.get('a')!.resolve('late-a'))
    expect(result.current.data).toBe('data-b')
  })

  it('keeps the data while a reload runs, then replaces it', async () => {
    let n = 0
    const next = [gate<string>(), gate<string>()]
    const { result } = renderHook(() => useResource('a', () => next[n++]!.promise))
    await act(async () => next[0]!.resolve('first'))
    expect(result.current.data).toBe('first')

    act(() => result.current.reload())
    expect(result.current).toMatchObject({ data: 'first', loading: true })

    await act(async () => next[1]!.resolve('second'))
    expect(result.current).toMatchObject({ data: 'second', loading: false })
  })

  it('reports an error, and keeps earlier data when a reload fails', async () => {
    let n = 0
    const next = [gate<string>(), gate<string>()]
    const { result } = renderHook(() => useResource('a', () => next[n++]!.promise))
    await act(async () => next[0]!.resolve('first'))

    act(() => result.current.reload())
    const failure = new Error('boom')
    await act(async () => next[1]!.reject(failure))
    expect(result.current).toMatchObject({ data: 'first', error: failure, loading: false })
  })

  it('does not report an abort as an error', async () => {
    const { result, unmount } = renderHook(() =>
      useResource('a', () => Promise.reject(new DOMException('aborted', 'AbortError'))),
    )
    await Promise.resolve()
    expect(result.current.error).toBeNull()
    unmount()
  })
})
