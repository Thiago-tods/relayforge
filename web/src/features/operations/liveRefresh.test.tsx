import { act, screen, waitFor, within } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import * as autoRefresh from '../../lib/useAutoRefresh'
import {
  apiError,
  attemptItem,
  deliverySummary,
  failureItem,
  feedPage,
  json,
  meBody,
  org,
  queueAge,
  queueSnapshot,
  stubFetch,
} from '../../test/fetchStub'
import type { Recorded } from '../../test/fetchStub'
import { fakeChannel, renderApp } from '../../test/renderApp'

vi.mock('../../auth/crossTab', () => ({ openAuthChannel: vi.fn() }))

// The pages use the real scheduling hook with a short interval, so these tests run in real time. The exact 15 second
// schedule and the immediate refresh on becoming visible are proven with fake timers in useAutoRefresh.test.tsx.
const INTERVAL = 250
vi.mock('../../lib/useAutoRefresh', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../lib/useAutoRefresh')>()
  return {
    ...actual,
    useAutoRefresh: (options: Parameters<typeof actual.useAutoRefresh>[0]) =>
      actual.useAutoRefresh({ intervalMs: INTERVAL, ...options }),
  }
})

// Scheduled refresh of the Queue and Activity pages. Every response is mocked: these tests use a stubbed fetch and
// prove the scheduling and the state handling of the pages, not a real backend.

const API = '/api/v1/organizations/o1'

afterEach(() => {
  Reflect.deleteProperty(document, 'visibilityState')
})

/**
 * Lets real time pass while the page keeps running. Inside act() React would hold every state update back until the
 * callback returns, so the page would stall; like the testing library's own async helpers, this leaves the act
 * environment for the wait.
 */
async function pass(ms: number) {
  const env = globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean | undefined }
  const previous = env.IS_REACT_ACT_ENVIRONMENT
  env.IS_REACT_ACT_ENVIRONMENT = false
  try {
    await new Promise((resolve) => setTimeout(resolve, ms))
  } finally {
    env.IS_REACT_ACT_ENVIRONMENT = previous
  }
}

async function setVisibility(state: 'visible' | 'hidden') {
  Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => state })
  await act(async () => {
    document.dispatchEvent(new Event('visibilitychange'))
  })
}

function gate<T>() {
  let release: (value: T) => void = () => undefined
  const promise = new Promise<T>((resolve) => {
    release = resolve
  })
  return { promise, release }
}

type Answer = (query: URLSearchParams, call: Recorded) => Response | Promise<Response>

function open(table: Record<string, Answer>, path: string) {
  fakeChannel()
  const calls = stubFetch((call) => {
    if (call.path === '/api/v1/auth/me') return json(200, meBody([org('o1'), org('o2')]))
    const url = new URL(call.path, 'http://localhost')
    const answer = table[url.pathname]
    return answer
      ? answer(url.searchParams, call)
      : apiError(404, 'not_found', 'resource not found')
  })
  return { calls, ...renderApp(path) }
}

/** The calls to one path, in order. */
const to = (calls: Recorded[], path: string) =>
  calls.filter((call) => new URL(call.path, 'http://localhost').pathname === path)
const queryOf = (call: Recorded) =>
  Object.fromEntries(new URL(call.path, 'http://localhost').searchParams)

it('uses the 15 second interval in production', () => {
  expect(autoRefresh.REFRESH_INTERVAL_MS).toBe(15_000)
})

describe('queue page: scheduled refresh', () => {
  const QUEUE = `${API}/operations/queue`
  const AGE = `${API}/operations/queue/age`
  const queuePath = '/o/o1/operations/queue'

  it('reads on entry, then refreshes each resource on its own without looking busy', async () => {
    let queueReads = 0
    const second = gate<void>()
    const { calls } = open(
      {
        [QUEUE]: async () => {
          queueReads += 1
          if (queueReads === 2) await second.promise
          return json(200, queueSnapshot({ pending: queueReads === 1 ? 12 : 99 }))
        },
        [AGE]: () => json(200, queueAge()),
      },
      queuePath,
    )
    await screen.findAllByRole('article')
    // The first reads are made on entry, not after an interval.
    expect(to(calls, QUEUE).length).toBeGreaterThanOrEqual(1)
    expect(to(calls, AGE).length).toBeGreaterThanOrEqual(1)

    await waitFor(() => expect(to(calls, QUEUE)).toHaveLength(2))
    await waitFor(() => expect(to(calls, AGE).length).toBeGreaterThanOrEqual(2))

    // The queue read is still running: the old numbers stay, and nothing says "Refreshing".
    expect(screen.getByText('12')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Refresh' })).toBeEnabled()
    expect(screen.queryByRole('button', { name: 'Refreshing…' })).not.toBeInTheDocument()
    expect(screen.getByRole('region', { name: 'Queue snapshot' })).toHaveAttribute(
      'aria-busy',
      'false',
    )

    await act(async () => second.release())
    expect(await screen.findByText('99')).toBeInTheDocument()
  })

  it('never overlaps a resource with itself, never replays missed ticks, and keeps the other resource going', async () => {
    const stuck = gate<void>()
    let queueReads = 0
    const { calls } = open(
      {
        [QUEUE]: async () => {
          queueReads += 1
          if (queueReads === 2) await stuck.promise
          return json(200, queueSnapshot())
        },
        [AGE]: () => json(200, queueAge()),
      },
      queuePath,
    )
    await screen.findAllByRole('article')
    await waitFor(() => expect(to(calls, QUEUE)).toHaveLength(2))
    const ageBefore = to(calls, AGE).length

    // Many intervals pass while the queue read hangs: it is not started again, and the age read is not held up.
    await pass(INTERVAL * 6)
    expect(to(calls, QUEUE)).toHaveLength(2)
    expect(to(calls, AGE).length).toBeGreaterThan(ageBefore + 2)

    // The missed ticks are not replayed: after the answer, a full interval passes before the next read.
    await act(async () => stuck.release())
    await pass(INTERVAL / 2)
    expect(to(calls, QUEUE)).toHaveLength(2)
    await waitFor(() => expect(to(calls, QUEUE)).toHaveLength(3))
  })

  it('pauses while the tab is hidden and refreshes without waiting when it is visible again', async () => {
    const { calls } = open(
      {
        [QUEUE]: () => json(200, queueSnapshot()),
        [AGE]: () => json(200, queueAge()),
      },
      queuePath,
    )
    await screen.findAllByRole('article')
    await setVisibility('hidden')
    const queueReads = to(calls, QUEUE).length
    const ageReads = to(calls, AGE).length
    await pass(INTERVAL * 5)
    expect(to(calls, QUEUE)).toHaveLength(queueReads)
    expect(to(calls, AGE)).toHaveLength(ageReads)

    await setVisibility('visible')
    // Well inside one interval, so this is the immediate refresh and not the next scheduled one.
    await waitFor(() => expect(to(calls, QUEUE)).toHaveLength(queueReads + 1), {
      timeout: INTERVAL / 2,
    })
    expect(to(calls, AGE)).toHaveLength(ageReads + 1)
  })

  it('keeps the shown numbers and shows a non-blocking notice when a refresh fails, then recovers', async () => {
    let queueReads = 0
    open(
      {
        [QUEUE]: () => {
          queueReads += 1
          if (queueReads === 2) return apiError(500, 'internal_error', 'internal error')
          return json(200, queueSnapshot({ pending: queueReads === 1 ? 12 : 77 }))
        },
        [AGE]: () => json(200, queueAge()),
      },
      queuePath,
    )
    await screen.findAllByRole('article')
    const section = screen.getByRole('region', { name: 'Queue snapshot' })
    expect(await within(section).findByText(/Could not refresh this section/)).toBeInTheDocument()
    expect(within(section).getByText('12')).toBeInTheDocument()
    expect(within(section).queryByRole('alert')).not.toBeInTheDocument()
    // The other resource is unaffected by it.
    const ageSection = screen.getByRole('region', { name: 'Overdue age of due deliveries' })
    expect(within(ageSection).queryByText(/Could not refresh/)).not.toBeInTheDocument()

    expect(await within(section).findByText('77')).toBeInTheDocument()
    expect(within(section).queryByText(/Could not refresh/)).not.toBeInTheDocument()
  })

  it('still blocks with an error when there is nothing to show yet', async () => {
    open(
      {
        [QUEUE]: () => apiError(500, 'internal_error', 'internal error'),
        [AGE]: () => json(200, queueAge()),
      },
      queuePath,
    )
    const section = await screen.findByRole('region', { name: 'Queue snapshot' })
    expect(await within(section).findByRole('alert')).toBeInTheDocument()
  })

  it('keeps the manual refresh, which replaces a running scheduled read and shows as busy', async () => {
    let queueReads = 0
    const scheduled = gate<void>()
    const manual = gate<void>()
    const { calls, user } = open(
      {
        [QUEUE]: async () => {
          queueReads += 1
          if (queueReads === 2) await scheduled.promise
          if (queueReads === 3) await manual.promise
          return json(200, queueSnapshot({ pending: queueReads === 1 ? 11 : 13 }))
        },
        [AGE]: () => json(200, queueAge()),
      },
      queuePath,
    )
    await screen.findAllByRole('article')
    await waitFor(() => expect(to(calls, QUEUE)).toHaveLength(2))

    await user.click(screen.getByRole('button', { name: 'Refresh' }))
    expect(await screen.findByRole('button', { name: 'Refreshing…' })).toBeDisabled()
    expect(to(calls, QUEUE)).toHaveLength(3)
    // The scheduled read it replaced was cancelled, so only one read of the resource is live.
    expect(to(calls, QUEUE)[1]?.signal?.aborted).toBe(true)
    await act(async () => manual.release())
    expect(await screen.findByText('13')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Refresh' })).toBeEnabled()
  })

  it('cancels its reads and stops scheduling when the organization changes', async () => {
    const stuck = gate<void>()
    let queueReads = 0
    const { calls, user } = open(
      {
        [QUEUE]: async () => {
          queueReads += 1
          if (queueReads === 2) await stuck.promise
          return json(200, queueSnapshot())
        },
        [AGE]: () => json(200, queueAge()),
      },
      queuePath,
    )
    await screen.findAllByRole('article')
    await waitFor(() => expect(to(calls, QUEUE)).toHaveLength(2))

    await user.selectOptions(screen.getByLabelText('Organization'), 'o2')
    await waitFor(() => expect(screen.getByTestId('location')).toHaveTextContent('/o/o2'))
    expect(to(calls, QUEUE)[1]?.signal?.aborted).toBe(true)
    const before = calls.filter((c) => c.path.includes('/organizations/o1/')).length
    await pass(INTERVAL * 4)
    await act(async () => stuck.release())
    await pass(INTERVAL * 2)
    expect(calls.filter((c) => c.path.includes('/organizations/o1/'))).toHaveLength(before)
  })

  it('cancels its reads and stops scheduling on unmount', async () => {
    const stuck = gate<void>()
    let queueReads = 0
    const { calls, unmount } = open(
      {
        [QUEUE]: async () => {
          queueReads += 1
          if (queueReads === 2) await stuck.promise
          return json(200, queueSnapshot())
        },
        [AGE]: () => json(200, queueAge()),
      },
      queuePath,
    )
    await screen.findAllByRole('article')
    await waitFor(() => expect(to(calls, QUEUE)).toHaveLength(2))
    unmount()
    expect(to(calls, QUEUE)[1]?.signal?.aborted).toBe(true)
    const count = calls.length
    await pass(INTERVAL * 5)
    expect(calls).toHaveLength(count)
  })
})

describe('activity page: scheduled refresh', () => {
  const SUMMARY = `${API}/operations/deliveries`
  const FAILURES = `${API}/operations/failures`
  const ATTEMPTS = `${API}/operations/attempts`
  const activityPath = '/o/o1/operations/activity'

  const uuid = (i: number) => `00000000-0000-4000-8000-${String(i).padStart(12, '0')}`
  const failure = (i: number) =>
    failureItem({
      deliveryId: uuid(i),
      destinationName: `Hook ${i}`,
      completedAt: `2026-11-22T10:${String(i).padStart(2, '0')}:00Z`,
    })
  const attempt = (i: number) =>
    attemptItem({
      attemptId: uuid(100 + i),
      deliveryId: uuid(i),
      destinationName: `Hook ${i}`,
      startedAt: `2026-11-22T10:${String(i).padStart(2, '0')}:00Z`,
    })

  interface World {
    /** The newest failure the server holds. Page 1 is the newest two, and "c1" is the page after. */
    newest: number
    probeFails: boolean
    summaryFails: boolean
  }
  const world = (over: Partial<World> = {}): World => ({
    newest: 9,
    probeFails: false,
    summaryFails: false,
    ...over,
  })

  function server(state: World): Record<string, Answer> {
    return {
      [SUMMARY]: () =>
        state.summaryFails
          ? apiError(500, 'internal_error', 'internal error')
          : json(200, deliverySummary()),
      [FAILURES]: (q) => {
        if (q.get('limit') === '1') {
          return state.probeFails
            ? apiError(500, 'internal_error', 'internal error')
            : json(200, feedPage([failure(state.newest)]))
        }
        if (q.get('cursor') === 'c1') return json(200, feedPage([failure(state.newest - 3)]))
        return json(200, feedPage([failure(state.newest), failure(state.newest - 1)], 'c1'))
      },
      [ATTEMPTS]: (q) =>
        q.get('limit') === '1'
          ? json(200, feedPage([attempt(state.newest)]))
          : json(200, feedPage([attempt(state.newest), attempt(state.newest - 1)])),
    }
  }

  /** The feed answers of a server whose probes of the failure feed hang until the gate is released. */
  function stuckProbes(extra: string[] = []) {
    const stuck = gate<Response>()
    const table = server(world())
    for (const path of [FAILURES, ...extra]) {
      const answer = table[path]!
      table[path] = (q, call) => (q.get('limit') === '1' ? stuck.promise : answer(q, call))
    }
    return { stuck, table }
  }

  async function loaded() {
    await screen.findByRole('table', { name: 'Failed deliveries, newest completion first' })
    await screen.findByRole('table', { name: 'Recorded attempts, newest start first' })
    await screen.findByRole('table', {
      name: 'Deliveries that failed in the window, by failure reason',
    })
  }
  const failuresRegion = () => screen.getByRole('region', { name: 'Recent failures' })

  it('reads on entry, then refreshes the summary in place and only probes the feeds', async () => {
    const { calls } = open(server(world()), activityPath)
    await loaded()
    expect(to(calls, SUMMARY).map(queryOf)[0]).toEqual({})
    expect(to(calls, FAILURES).map(queryOf)[0]).toEqual({ limit: '25' })
    expect(to(calls, ATTEMPTS).map(queryOf)[0]).toEqual({ limit: '25' })

    // A rolling window is asked for without a date, so the server picks it again.
    await waitFor(() => expect(to(calls, SUMMARY).length).toBeGreaterThanOrEqual(2))
    await waitFor(() => expect(to(calls, FAILURES).length).toBeGreaterThanOrEqual(2))
    await waitFor(() => expect(to(calls, ATTEMPTS).length).toBeGreaterThanOrEqual(2))
    expect(to(calls, SUMMARY).map(queryOf)[1]).toEqual({})
    expect(to(calls, FAILURES).map(queryOf)[1]).toEqual({ limit: '1' })
    expect(to(calls, ATTEMPTS).map(queryOf)[1]).toEqual({ limit: '1' })
    // The rows the user sees were not replaced, and no first page was read again.
    expect(within(failuresRegion()).getAllByRole('row').slice(1)).toHaveLength(2)
    expect(
      to(calls, FAILURES)
        .map(queryOf)
        .filter((q) => q.limit === '25'),
    ).toHaveLength(1)
    expect(screen.queryByText('New activity available.')).not.toBeInTheDocument()
  })

  it('offers newer entries without moving the page, the filters or the position, and loads them on request', async () => {
    const state = world()
    const { calls, user } = open(server(state), `${activityPath}?fReason=RETRIES_EXHAUSTED`)
    await loaded()
    const region = failuresRegion()
    await user.click(within(region).getByRole('button', { name: 'Next' }))
    await waitFor(() => expect(within(region).getByText('Hook 6')).toBeInTheDocument())
    expect(within(region).getByText('Page 2 (last page)')).toBeInTheDocument()
    const held = to(calls, FAILURES).map(queryOf)

    state.newest = 12
    expect(await within(region).findByText('New activity available.')).toBeInTheDocument()

    // Nothing moved: same page, same rows, same filter in the URL, and no first page was read again.
    expect(within(region).getByText('Page 2 (last page)')).toBeInTheDocument()
    expect(within(region).getByText('Hook 6')).toBeInTheDocument()
    expect(within(region).queryByText('Hook 12')).not.toBeInTheDocument()
    expect(screen.getByTestId('search')).toHaveTextContent('fReason=RETRIES_EXHAUSTED')
    const since = to(calls, FAILURES).map(queryOf).slice(held.length)
    expect(since.length).toBeGreaterThan(0)
    expect(since.every((q) => q.limit === '1' && !('cursor' in q))).toBe(true)
    // The notice is also announced through a region that was mounted before the text appeared.
    expect(
      screen
        .getAllByRole('status')
        .some((node) => /New activity is available/.test(node.textContent)),
    ).toBe(true)

    await user.click(within(region).getByRole('button', { name: 'Show newest' }))
    await waitFor(() => expect(within(region).getByText('Hook 12')).toBeInTheDocument())
    expect(within(region).getByText('Page 1')).toBeInTheDocument()
    expect(within(region).queryByText('New activity available.')).not.toBeInTheDocument()
    const firstPages = to(calls, FAILURES)
      .map(queryOf)
      .filter((q) => q.limit === '25')
    expect(firstPages.at(-1)).toEqual({ limit: '25', failureReason: 'RETRIES_EXHAUSTED' })
  })

  it('does not report an entry that is already held', async () => {
    const { calls } = open(server(world()), activityPath)
    await loaded()
    await waitFor(() => expect(to(calls, FAILURES).length).toBeGreaterThanOrEqual(3))
    expect(screen.queryByText('New activity available.')).not.toBeInTheDocument()
  })

  it('probes with the applied filters and explicit range, and with no date for a rolling window', async () => {
    const url =
      `${activityPath}?fFrom=2026-11-20T00:00:00Z&fTo=2026-11-22T00:00:00Z&fReason=EGRESS_BLOCKED` +
      '&aOutcome=TIMEOUT'
    const { calls } = open(server(world()), url)
    await loaded()
    await waitFor(() => expect(to(calls, FAILURES).length).toBeGreaterThanOrEqual(2))
    await waitFor(() => expect(to(calls, ATTEMPTS).length).toBeGreaterThanOrEqual(2))
    expect(queryOf(to(calls, FAILURES)[1]!)).toEqual({
      limit: '1',
      failureReason: 'EGRESS_BLOCKED',
      from: '2026-11-20T00:00:00Z',
      to: '2026-11-22T00:00:00Z',
    })
    // The attempt section has no range applied, so its probe sends none either: the server picks the window.
    expect(queryOf(to(calls, ATTEMPTS)[1]!)).toEqual({ limit: '1', outcome: 'TIMEOUT' })
    expect(queryOf(to(calls, SUMMARY)[0]!)).toEqual({})
  })

  it('keeps the rows and says so when the check for new activity fails, then recovers', async () => {
    const state = world({ probeFails: true })
    open(server(state), activityPath)
    await loaded()
    const region = failuresRegion()
    expect(await within(region).findByText(/Could not check for new activity/)).toBeInTheDocument()
    expect(within(region).getByText('Hook 9')).toBeInTheDocument()
    expect(within(region).queryByRole('alert')).not.toBeInTheDocument()

    state.probeFails = false
    await waitFor(() =>
      expect(
        within(region).queryByText(/Could not check for new activity/),
      ).not.toBeInTheDocument(),
    )
  })

  it('refreshes each section independently: a failed summary keeps its numbers and leaves the feeds alone', async () => {
    const state = world()
    open(server(state), activityPath)
    await loaded()
    state.summaryFails = true
    const summary = screen.getByRole('region', { name: 'Delivery summary' })
    expect(await within(summary).findByText(/Could not refresh this section/)).toBeInTheDocument()
    expect(within(summary).getByText('Total created')).toBeInTheDocument()
    expect(within(summary).queryByRole('alert')).not.toBeInTheDocument()
    const region = failuresRegion()
    expect(within(region).queryByText(/Could not/)).not.toBeInTheDocument()
    expect(within(region).getByText('Hook 9')).toBeInTheDocument()
  })

  it('keeps the manual refresh of every section, also while the tab is hidden', async () => {
    const { calls, user } = open(server(world()), activityPath)
    await loaded()
    await setVisibility('hidden')
    const summaryReads = to(calls, SUMMARY).length
    await user.click(
      within(screen.getByRole('region', { name: 'Delivery summary' })).getByRole('button', {
        name: 'Refresh',
      }),
    )
    await waitFor(() => expect(to(calls, SUMMARY)).toHaveLength(summaryReads + 1))
    const firstPages = () =>
      to(calls, FAILURES)
        .map(queryOf)
        .filter((q) => q.limit === '25').length
    const before = firstPages()
    await user.click(within(failuresRegion()).getByRole('button', { name: 'Refresh' }))
    await waitFor(() => expect(firstPages()).toBe(before + 1))
    const attemptPages = () =>
      to(calls, ATTEMPTS)
        .map(queryOf)
        .filter((q) => q.limit === '25').length
    const attemptsBefore = attemptPages()
    await user.click(
      within(screen.getByRole('region', { name: 'Attempt activity' })).getByRole('button', {
        name: 'Refresh',
      }),
    )
    await waitFor(() => expect(attemptPages()).toBe(attemptsBefore + 1))
  })

  it('pauses all three sections while the tab is hidden and resumes when it is visible', async () => {
    const { calls } = open(server(world()), activityPath)
    await loaded()
    await setVisibility('hidden')
    const before = [SUMMARY, FAILURES, ATTEMPTS].map((path) => to(calls, path).length)
    await pass(INTERVAL * 5)
    expect([SUMMARY, FAILURES, ATTEMPTS].map((path) => to(calls, path).length)).toEqual(before)

    await setVisibility('visible')
    await waitFor(() => expect(to(calls, SUMMARY)).toHaveLength(before[0]! + 1), {
      timeout: INTERVAL / 2,
    })
    await waitFor(() => expect(to(calls, FAILURES)).toHaveLength(before[1]! + 1), {
      timeout: INTERVAL / 2,
    })
    await waitFor(() => expect(to(calls, ATTEMPTS)).toHaveLength(before[2]! + 1), {
      timeout: INTERVAL / 2,
    })
  })

  it('does not overlap a check with itself while it is running, and the other sections keep their schedule', async () => {
    const { stuck, table } = stuckProbes()
    const { calls } = open(table, activityPath)
    await loaded()
    await waitFor(() => expect(to(calls, FAILURES).length).toBeGreaterThanOrEqual(2))
    const summaryBefore = to(calls, SUMMARY).length
    await pass(INTERVAL * 6)
    expect(to(calls, FAILURES).map(queryOf)).toEqual([{ limit: '25' }, { limit: '1' }])
    expect(to(calls, SUMMARY).length).toBeGreaterThan(summaryBefore + 2)
    await act(async () => stuck.release(json(200, feedPage([failure(9)]))))
  })

  it('cancels a running check and ignores its late answer when the organization changes', async () => {
    const { stuck, table } = stuckProbes()
    const { calls, user } = open(table, activityPath)
    await loaded()
    await waitFor(() => expect(to(calls, FAILURES).length).toBeGreaterThanOrEqual(2))
    const probe = to(calls, FAILURES)[1]!

    await user.selectOptions(screen.getByLabelText('Organization'), 'o2')
    await waitFor(() => expect(screen.getByTestId('location')).toHaveTextContent('/o/o2'))
    expect(probe.signal?.aborted).toBe(true)
    await act(async () => stuck.release(json(200, feedPage([failure(40)]))))
    await pass(INTERVAL * 2)
    expect(screen.queryByText('New activity available.')).not.toBeInTheDocument()
    expect(screen.queryByText('Hook 9')).not.toBeInTheDocument()
  })

  it('cancels every running read and stops scheduling on unmount', async () => {
    const { table } = stuckProbes([ATTEMPTS])
    const { calls, unmount } = open(table, activityPath)
    await loaded()
    await waitFor(() => expect(to(calls, FAILURES).length).toBeGreaterThanOrEqual(2))
    await waitFor(() => expect(to(calls, ATTEMPTS).length).toBeGreaterThanOrEqual(2))
    unmount()
    expect(to(calls, FAILURES)[1]?.signal?.aborted).toBe(true)
    expect(to(calls, ATTEMPTS)[1]?.signal?.aborted).toBe(true)
    const count = calls.length
    await pass(INTERVAL * 5)
    expect(calls).toHaveLength(count)
  })
})
