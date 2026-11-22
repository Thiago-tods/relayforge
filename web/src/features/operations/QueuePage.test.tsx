import { act, render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { Link, MemoryRouter, Route, Routes } from 'react-router-dom'
import { describe, expect, it, vi } from 'vitest'
import { AuthContext } from '../../auth/authContext'
import type { AuthValue } from '../../auth/authContext'
import { OrgLayout } from '../../org/OrgLayout'
import {
  apiError,
  json,
  meBody,
  org,
  queueAge,
  queueSnapshot,
  stubFetch,
  USER,
} from '../../test/fetchStub'
import type { Recorded } from '../../test/fetchStub'
import { fakeChannel, renderApp, routes } from '../../test/renderApp'
import { QueuePage } from './QueuePage'

vi.mock('../../auth/crossTab', () => ({ openAuthChannel: vi.fn() }))

const ME = 'GET /api/v1/auth/me'
const queuePath = (id: string) => `GET /api/v1/organizations/${id}/operations/queue`
const agePath = (id: string) => `GET /api/v1/organizations/${id}/operations/queue/age`
const signedIn = (...orgs: ReturnType<typeof org>[]) => json(200, meBody(orgs))

/** Opens the queue page of org "o1" through the whole application (routing, shell, providers). */
function open(table: Parameters<typeof routes>[0]) {
  fakeChannel()
  const { handler } = routes({ [ME]: () => signedIn(org('o1'), org('o2')), ...table })
  const calls = stubFetch(handler)
  return { calls, ...renderApp('/o/o1/operations/queue') }
}

/** A promise a test settles by hand. */
function gate() {
  let release: () => void = () => undefined
  const promise = new Promise<void>((resolve) => {
    release = resolve
  })
  return { promise, release }
}

const operationsCalls = (calls: Recorded[]) => calls.filter((c) => c.path.includes('/operations/'))

describe('queue page', () => {
  it('shows the six metrics with their definitions', async () => {
    open({
      [queuePath('o1')]: () => json(200, queueSnapshot()),
      [agePath('o1')]: () => json(200, queueAge()),
    })
    expect(await screen.findByRole('heading', { name: 'Queue', level: 1 })).toBeInTheDocument()

    const cards = await screen.findAllByRole('article')
    expect(cards).toHaveLength(6)
    const card = (name: string) => cards.find((c) => within(c).queryByRole('heading', { name }))!
    expect(within(card('Pending')).getByText('12')).toBeInTheDocument()
    expect(within(card('Due')).getByText('9')).toBeInTheDocument()
    expect(within(card('Delayed')).getByText('3')).toBeInTheDocument()
    expect(within(card('In flight')).getByText('4')).toBeInTheDocument()
    expect(within(card('Expired leases')).getByText('1')).toBeInTheDocument()
    expect(within(card('Oldest due age')).getByText('1h 1m 1s')).toBeInTheDocument()
    expect(within(card('Oldest due age')).getByText('3,661 seconds')).toBeInTheDocument()

    await screen.findByRole('table')
    expect(screen.getByText('These are delivery counts, not attempt counts.')).toBeInTheDocument()
    expect(
      screen.getByText(
        "Scheduled time reached. A paused destination's deliveries still count as due.",
      ),
    ).toBeInTheDocument()
    // The exact wording required for expired leases.
    expect(
      screen.getByText(
        'Lease expired and awaiting recovery. This does not establish that the worker has stopped running.',
      ),
    ).toBeInTheDocument()
    expect(screen.getAllByText(/Snapshot taken/)).toHaveLength(2)
    expect(screen.getAllByText('2026-11-22 17:27:03 UTC')[0]).toBeInTheDocument()
  })

  it('uses none of the wording the backend does not support', async () => {
    const { container } = open({
      [queuePath('o1')]: () => json(200, queueSnapshot()),
      [agePath('o1')]: () => json(200, queueAge()),
    })
    await screen.findAllByRole('article')
    expect(container.textContent ?? '').not.toMatch(/\b(stuck|healthy|unhealthy|dead)\b/i)
  })

  it('shows "None due" when nothing is due, and no bars for an empty distribution', async () => {
    open({
      [queuePath('o1')]: () => json(200, queueSnapshot({ due: 0, oldestDueAgeSeconds: 0 })),
      [agePath('o1')]: () => json(200, queueAge([0, 0, 0, 0, 0, 0, 0])),
    })
    expect(await screen.findByText('None due')).toBeInTheDocument()
    expect(await screen.findByText('No due deliveries.')).toBeInTheDocument()
    expect(screen.queryByRole('table')).not.toBeInTheDocument()
  })

  it('requests each endpoint once on open, never polls, and refreshes only on demand', async () => {
    const { calls, user } = open({
      [queuePath('o1')]: () => json(200, queueSnapshot()),
      [agePath('o1')]: () => json(200, queueAge()),
    })
    await screen.findAllByRole('article')
    await new Promise((resolve) => setTimeout(resolve, 100))
    expect(operationsCalls(calls).map((c) => c.path)).toEqual(
      expect.arrayContaining([
        '/api/v1/organizations/o1/operations/queue',
        '/api/v1/organizations/o1/operations/queue/age',
      ]),
    )
    expect(operationsCalls(calls)).toHaveLength(2)

    await user.click(screen.getByRole('button', { name: 'Refresh' }))
    await screen.findByRole('button', { name: 'Refresh' })
    expect(operationsCalls(calls)).toHaveLength(4)
  })

  it('disables Refresh while a read runs and keeps the old numbers visible', async () => {
    const second = gate()
    let reads = 0
    const { user } = open({
      [queuePath('o1')]: async () => {
        reads += 1
        if (reads === 2) await second.promise
        return json(200, queueSnapshot({ pending: reads === 1 ? 12 : 99 }))
      },
      [agePath('o1')]: () => json(200, queueAge()),
    })
    await screen.findAllByRole('article')
    await user.click(screen.getByRole('button', { name: 'Refresh' }))
    const busy = await screen.findByRole('button', { name: 'Refreshing…' })
    expect(busy).toBeDisabled()
    expect(screen.getByText('12')).toBeInTheDocument()
    await act(async () => second.release())
    expect(await screen.findByText('99')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Refresh' })).toBeEnabled()
  })
})

describe('lower-bound counts', () => {
  const fields = ['pending', 'due', 'delayed', 'inFlight', 'expiredLeases'] as const
  const labels = {
    pending: 'Pending',
    due: 'Due',
    delayed: 'Delayed',
    inFlight: 'In flight',
    expiredLeases: 'Expired leases',
  }

  it.each(fields)(
    'shows a saturated %s as a lower bound, and the others exactly',
    async (field) => {
      open({
        [queuePath('o1')]: () =>
          json(
            200,
            queueSnapshot({
              pending: 7,
              due: 6,
              delayed: 5,
              inFlight: 4,
              expiredLeases: 3,
              [field]: 100_000,
              capped: true,
            }),
          ),
        [agePath('o1')]: () => json(200, queueAge()),
      })
      const cards = await screen.findAllByRole('article')
      const card = cards.find((c) => within(c).queryByRole('heading', { name: labels[field] }))!
      expect(within(card).getByText('≥ 100,000')).toBeInTheDocument()
      expect(within(card).getByText('at least 100,000')).toBeInTheDocument()
      expect(within(card).queryByText(/^100,000$/)).not.toBeInTheDocument()
      expect(screen.getAllByText('≥ 100,000')).toHaveLength(1)
      expect(screen.getAllByText(/lower bounds/)[0]).toBeInTheDocument()
    },
  )

  it('decides from the count, not from the response flag', async () => {
    open({
      [queuePath('o1')]: () => json(200, queueSnapshot({ pending: 100_000, capped: false })),
      [agePath('o1')]: () => json(200, queueAge()),
    })
    expect(await screen.findByText('≥ 100,000')).toBeInTheDocument()
  })

  it('keeps small counts exact even when the flag says capped', async () => {
    open({
      [queuePath('o1')]: () => json(200, queueSnapshot({ capped: true })),
      [agePath('o1')]: () => json(200, queueAge()),
    })
    await screen.findAllByRole('article')
    expect(screen.queryByText('≥ 100,000')).not.toBeInTheDocument()
    expect(screen.getByText('12')).toBeInTheDocument()
  })

  it.each([0, 1, 2, 3, 4, 5, 6])(
    'shows a saturated age bucket %d as a lower bound',
    async (index) => {
      const counts = [9, 8, 7, 6, 5, 4, 3]
      counts[index] = 100_000
      open({
        [queuePath('o1')]: () => json(200, queueSnapshot()),
        [agePath('o1')]: () => json(200, queueAge(counts, true)),
      })
      const table = await screen.findByRole('table')
      const rows = within(table).getAllByRole('row').slice(1)
      expect(rows).toHaveLength(7)
      rows.forEach((row, i) => {
        if (i === index) {
          expect(within(row).getByText('≥ 100,000')).toBeInTheDocument()
          expect(within(row).getByText('at least 100,000')).toBeInTheDocument()
        } else {
          expect(within(row).getByText(String(counts[i]))).toBeInTheDocument()
        }
      })
      expect(within(table).getAllByText('≥ 100,000')).toHaveLength(1)
    },
  )
})

describe('queue-age chart', () => {
  it('is a table in the returned bucket order with the API labels and counts', async () => {
    open({
      [queuePath('o1')]: () => json(200, queueSnapshot()),
      [agePath('o1')]: () => json(200, queueAge([5, 2, 1, 0, 1, 0, 3])),
    })
    const table = await screen.findByRole('table', {
      name: 'Due deliveries by how long they have been overdue',
    })
    expect(
      within(table)
        .getAllByRole('columnheader')
        .map((h) => h.textContent),
    ).toEqual(['Overdue for', 'Deliveries', 'Relative size'])
    const headers = within(table)
      .getAllByRole('rowheader')
      .map((h) => h.textContent ?? '')
    expect(headers.map((text) => text.split(',')[0])).toEqual([
      '<1m',
      '1-5m',
      '5-15m',
      '15-60m',
      '1-6h',
      '6-24h',
      '>24h',
    ])
    expect(headers[6]).toContain('no upper bound')
    expect(headers[0]).toContain('overdue from 0s up to 1m')
  })

  it('explains what the age means and shows no invented statistics', async () => {
    open({
      [queuePath('o1')]: () => json(200, queueSnapshot()),
      [agePath('o1')]: () => json(200, queueAge()),
    })
    expect(await screen.findByText(/not the time since it was created/)).toBeInTheDocument()
    expect(screen.queryByText(/percentile|p50|p95|p99|average|median/i)).not.toBeInTheDocument()
    expect(
      screen.getByText(/two separate reads, so their numbers can differ slightly/),
    ).toBeInTheDocument()
  })
})

describe('errors', () => {
  it('keeps one panel working when the other fails, and retries only the failed one', async () => {
    let queueReads = 0
    const { calls, user } = open({
      [queuePath('o1')]: () => {
        queueReads += 1
        return queueReads === 1
          ? apiError(500, 'internal_error', 'secret database detail')
          : json(200, queueSnapshot())
      },
      [agePath('o1')]: () => json(200, queueAge()),
    })
    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent('could not complete the request')
    expect(alert).not.toHaveTextContent('secret database detail')
    // The age panel is unaffected.
    expect(await screen.findByRole('table')).toBeInTheDocument()

    await user.click(within(alert).getByRole('button', { name: 'Try again' }))
    expect(await screen.findAllByRole('article')).toHaveLength(6)
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
    expect(calls.filter((c) => c.path.endsWith('/queue/age'))).toHaveLength(1)
  })

  it('shows the backend message of a read timeout and offers a retry', async () => {
    let reads = 0
    const { user } = open({
      [queuePath('o1')]: () => json(200, queueSnapshot()),
      [agePath('o1')]: () => {
        reads += 1
        return reads === 1
          ? apiError(
              503,
              'read_model_timeout',
              'the query took too long; try a shorter time window',
            )
          : json(200, queueAge())
      },
    })
    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent('the query took too long')
    expect(alert).toHaveTextContent('no time window to narrow')
    await user.click(within(alert).getByRole('button', { name: 'Try again' }))
    expect(await screen.findByRole('table')).toBeInTheDocument()
  })

  it('shows the permission message of a 403', async () => {
    open({
      [queuePath('o1')]: () =>
        apiError(403, 'forbidden', 'you do not have permission to perform this action'),
      [agePath('o1')]: () =>
        apiError(403, 'forbidden', 'you do not have permission to perform this action'),
    })
    expect(await screen.findAllByRole('alert')).toHaveLength(2)
    expect(screen.getAllByText('you do not have permission to perform this action')).toHaveLength(2)
  })
})

describe('navigation', () => {
  it('lists Operations > Queue inside an organization and opens the page', async () => {
    const { user } = open({
      [queuePath('o1')]: () => json(200, queueSnapshot()),
      [agePath('o1')]: () => json(200, queueAge()),
    })
    const nav = await screen.findByRole('navigation', { name: 'Main navigation' })
    const group = within(nav).getByRole('group', { name: 'Operations' })
    expect(
      within(group)
        .getAllByRole('link')
        .map((l) => l.textContent),
    ).toEqual(['Queue', 'Activity'])
    // Only real links: no dead entries for pages that do not exist yet.
    expect(
      within(nav)
        .getAllByRole('link')
        .map((l) => l.textContent),
    ).toEqual(['Organizations', 'Overview', 'Queue', 'Activity'])
    await user.click(within(group).getByRole('link', { name: 'Queue' }))
    expect(await screen.findByRole('heading', { name: 'Queue', level: 1 })).toBeInTheDocument()
  })

  it('has no Operations entry outside an organization', async () => {
    fakeChannel()
    stubFetch(routes({ [ME]: () => signedIn(org('o1'), org('o2')) }).handler)
    renderApp('/orgs')
    const nav = await screen.findByRole('navigation', { name: 'Main navigation' })
    expect(within(nav).queryByRole('group', { name: 'Operations' })).not.toBeInTheDocument()
    expect(within(nav).queryByRole('link', { name: 'Queue' })).not.toBeInTheDocument()
  })

  it('shows "not found", no Operations entry and no request for an organization the user is not in', async () => {
    fakeChannel()
    const calls = stubFetch(routes({ [ME]: () => signedIn(org('o1')) }).handler)
    renderApp('/o/foreign/operations/queue')
    expect(await screen.findByText(/does not exist or you are not a member/)).toBeInTheDocument()
    const nav = screen.getByRole('navigation', { name: 'Main navigation' })
    expect(within(nav).queryByRole('link', { name: 'Queue' })).not.toBeInTheDocument()
    expect(operationsCalls(calls)).toHaveLength(0)
  })

  it('shows "not found" for the bare /operations path until an overview exists', async () => {
    fakeChannel()
    stubFetch(routes({ [ME]: () => signedIn(org('o1')) }).handler)
    renderApp('/o/o1/operations')
    expect(await screen.findByRole('heading', { name: 'Page not found' })).toBeInTheDocument()
  })
})

describe('organization switching', () => {
  function renderTwoOrganizations(handler: (call: Recorded) => Response | Promise<Response>) {
    const auth: AuthValue = {
      status: 'authenticated',
      user: USER,
      organizations: [org('o1'), org('o2')],
      expiresAt: null,
      notice: null,
      errorMessage: null,
      login: vi.fn(),
      register: vi.fn(),
      logout: vi.fn(),
      addOrganization: vi.fn(),
      retry: vi.fn(),
    }
    const calls = stubFetch(handler)
    const user = userEvent.setup()
    render(
      <AuthContext.Provider value={auth}>
        <MemoryRouter initialEntries={['/o/o1/operations/queue']}>
          <Link to="/o/o2/operations/queue">to o2</Link>
          <Routes>
            <Route path="/o/:orgId" element={<OrgLayout />}>
              <Route path="operations/queue" element={<QueuePage />} />
            </Route>
          </Routes>
        </MemoryRouter>
      </AuthContext.Provider>,
    )
    return { calls, user }
  }

  it('cancels the old reads and never shows another organization’s numbers', async () => {
    const o1Gate = gate()
    const { calls, user } = renderTwoOrganizations(async (call) => {
      const isO1 = call.path.includes('/organizations/o1/')
      if (isO1) await o1Gate.promise
      if (call.path.endsWith('/queue/age')) return json(200, queueAge())
      return json(200, queueSnapshot({ pending: isO1 ? 111 : 222 }))
    })

    // The reads of o1 are still running when the user moves to o2.
    await user.click(screen.getByRole('link', { name: 'to o2' }))
    expect(await screen.findByText('222')).toBeInTheDocument()

    const o1Reads = calls.filter((c) => c.path.includes('/organizations/o1/'))
    expect(o1Reads).toHaveLength(2)
    expect(o1Reads.every((c) => c.signal?.aborted === true)).toBe(true)

    // Even if o1 answers late, its numbers never appear.
    await act(async () => o1Gate.release())
    expect(screen.queryByText('111')).not.toBeInTheDocument()
    expect(screen.getByText('222')).toBeInTheDocument()
  })
})
