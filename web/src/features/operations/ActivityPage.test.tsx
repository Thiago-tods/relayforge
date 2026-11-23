import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { Link, MemoryRouter, Route, Routes } from 'react-router-dom'
import { describe, expect, it, vi } from 'vitest'
import { AuthContext } from '../../auth/authContext'
import type { AuthValue } from '../../auth/authContext'
import { OrgLayout } from '../../org/OrgLayout'
import {
  apiError,
  attemptItem,
  DELIVERY_A,
  DELIVERY_B,
  deliverySummary,
  DEST_A,
  DEST_B,
  destinationPage,
  failureItem,
  feedPage,
  json,
  meBody,
  org,
  stubFetch,
  USER,
} from '../../test/fetchStub'
import type { Recorded } from '../../test/fetchStub'
import { ToastProvider } from '../../ui/toast'
import { fakeChannel, renderApp } from '../../test/renderApp'
import { ActivityPage } from './ActivityPage'

vi.mock('../../auth/crossTab', () => ({ openAuthChannel: vi.fn() }))

// Every number and row here is mocked: the scratch database holds no deliveries, and these tests use a stubbed fetch,
// so they are not end-to-end evidence.

const API = '/api/v1/organizations/o1'
type Answer = (query: URLSearchParams, call: Recorded) => Response | Promise<Response>
interface Answers {
  summary?: Answer
  failures?: Answer
  attempts?: Answer
  destinations?: Answer
}

function respond(answers: Answers) {
  return (call: Recorded): Response | Promise<Response> => {
    if (call.path === '/api/v1/auth/me') return json(200, meBody([org('o1'), org('o2')]))
    const url = new URL(call.path, 'http://localhost')
    const query = url.searchParams
    const table: Record<string, Answer | undefined> = {
      [`${API}/operations/deliveries`]: answers.summary ?? (() => json(200, deliverySummary())),
      [`${API}/operations/failures`]:
        answers.failures ?? (() => json(200, feedPage([failureItem()]))),
      [`${API}/operations/attempts`]:
        answers.attempts ?? (() => json(200, feedPage([attemptItem()]))),
      [`${API}/destinations`]: answers.destinations,
    }
    const answer = table[url.pathname]
    return answer ? answer(query, call) : apiError(404, 'not_found', 'resource not found')
  }
}

function open(answers: Answers = {}, path = '/o/o1/operations/activity') {
  fakeChannel()
  const calls = stubFetch(respond(answers))
  return { calls, ...renderApp(path) }
}

/** The query of every request to a path, in order. */
function reads(calls: Recorded[], suffix: string): Record<string, string>[] {
  return calls
    .map((call) => new URL(call.path, 'http://localhost'))
    .filter((url) => url.pathname === `${API}${suffix}`)
    .map((url) => Object.fromEntries(url.searchParams))
}

const summaryRegion = () => screen.findByRole('region', { name: 'Delivery summary' })
const failuresRegion = () => screen.findByRole('region', { name: 'Recent failures' })
const attemptsRegion = () => screen.findByRole('region', { name: 'Attempt activity' })

/** Waits until the three sections of the page have shown their data. */
async function loaded() {
  await screen.findByRole('table', { name: 'Failed deliveries, newest completion first' })
  await screen.findByRole('table', { name: 'Recorded attempts, newest start first' })
  await screen.findByRole('table', {
    name: 'Deliveries that failed in the window, by failure reason',
  })
}

function gate() {
  let release: () => void = () => undefined
  const promise = new Promise<void>((resolve) => {
    release = resolve
  })
  return { promise, release }
}

function range(form: HTMLElement, from: string, to: string) {
  fireEvent.change(within(form).getByLabelText('From (UTC)'), { target: { value: from } })
  fireEvent.change(within(form).getByLabelText('To (UTC)'), { target: { value: to } })
}

const uuid = (i: number) => `00000000-0000-4000-8000-${String(i).padStart(12, '0')}`
const failure = (i: number) =>
  failureItem({
    deliveryId: uuid(i),
    destinationName: `Hook ${i}`,
    completedAt: `2026-11-22T10:${String(i).padStart(2, '0')}:00Z`,
  })

describe('opening the page', () => {
  it('makes exactly three reads, asks the server for its default windows, and never polls', async () => {
    const { calls } = open()
    await loaded()
    await new Promise((resolve) => setTimeout(resolve, 100))
    expect(reads(calls, '/operations/deliveries')).toEqual([{}])
    expect(reads(calls, '/operations/failures')).toEqual([{ limit: '25' }])
    expect(reads(calls, '/operations/attempts')).toEqual([{ limit: '25' }])
    // No destinations are requested until the user asks.
    expect(reads(calls, '/destinations')).toEqual([])
    expect(calls.filter((c) => c.path.startsWith(`${API}/`))).toHaveLength(3)
  })

  it('shows the window the server chose, in UTC, and does not compute one itself', async () => {
    open()
    const region = await summaryRegion()
    expect(await within(region).findByText('2026-11-21 17:30:00 UTC')).toBeInTheDocument()
    expect(within(region).getAllByText('2026-11-22 17:30:00 UTC').length).toBeGreaterThan(0)
  })

  it('uses none of the wording the backend does not support', async () => {
    const { container } = open()
    await loaded()
    expect(container.textContent ?? '').not.toMatch(/\b(stuck|healthy|unhealthy|dead)\b/i)
  })

  it('links delivery IDs in both feeds to the organization-scoped detail route', async () => {
    open()
    const failures = await screen.findByRole('table', {
      name: 'Failed deliveries, newest completion first',
    })
    const attempts = await screen.findByRole('table', {
      name: 'Recorded attempts, newest start first',
    })
    const failureRow = within(failures).getAllByRole('row')[1]!
    const attemptRow = within(attempts).getAllByRole('row')[1]!
    expect(within(failureRow).getByRole('link', { name: 'View' })).toHaveAttribute(
      'href',
      `/o/o1/operations/deliveries/${DELIVERY_A}`,
    )
    expect(within(attemptRow).getByRole('link', { name: 'View' })).toHaveAttribute(
      'href',
      `/o/o1/operations/deliveries/${DELIVERY_A}`,
    )
    expect(
      within(failureRow).getByRole('button', { name: /copy delivery id/i }),
    ).toBeInTheDocument()
  })

  it('lists Activity next to Queue and opens the page from the navigation', async () => {
    const { user } = open({}, '/o/o1')
    const nav = await screen.findByRole('navigation', { name: 'Main navigation' })
    const group = within(nav).getByRole('group', { name: 'Operations' })
    await user.click(within(group).getByRole('link', { name: 'Activity' }))
    expect(await screen.findByRole('heading', { name: 'Activity', level: 1 })).toBeInTheDocument()
    expect(screen.getByTestId('location')).toHaveTextContent('/o/o1/operations/activity')
  })
})

describe('delivery summary', () => {
  it('shows the two cohorts separately and says what each one counts', async () => {
    open()
    const region = await summaryRegion()
    const created = await within(region).findByRole('group', { name: 'Created in this window' })
    const completed = within(region).getByRole('group', { name: 'Completed in this window' })
    expect(
      within(created).getByText(/grouped by each delivery's current status/),
    ).toBeInTheDocument()
    const card = (group: HTMLElement, name: string) =>
      within(group).getByRole('heading', { name }).closest('article')!
    expect(within(card(created, 'Total created')).getByText('40')).toBeInTheDocument()
    expect(within(card(created, 'Pending now')).getByText('2')).toBeInTheDocument()
    expect(within(card(completed, 'Succeeded')).getByText('30')).toBeInTheDocument()
    expect(within(card(completed, 'Failure rate')).getByText('16.7%')).toBeInTheDocument()
    expect(within(region).getByText(/delivery counts, not attempt counts/)).toBeInTheDocument()
  })

  it('lists the six failure reasons, with an unrecorded reason as "Not recorded"', async () => {
    open()
    const table = await screen.findByRole('table', {
      name: 'Deliveries that failed in the window, by failure reason',
    })
    const rows = within(table).getAllByRole('row').slice(1)
    expect(rows.map((row) => within(row).getByRole('rowheader').textContent)).toEqual([
      'Destination rejected',
      'Redirect refused',
      'Egress blocked',
      'Retries exhausted',
      'Internal error',
      'Not recorded',
    ])
    expect(within(rows[0]!).getAllByRole('cell').at(-1)).toHaveTextContent('3')
  })

  it('explains a missing failure rate without guessing why', async () => {
    const summary = deliverySummary()
    open({
      summary: () =>
        json(200, deliverySummary({ completed: { ...summary.completed, failureRate: null } })),
    })
    const region = await summaryRegion()
    expect(await within(region).findByText(/Not available: nothing completed/)).toBeInTheDocument()
    expect(within(region).queryByText(/%/)).not.toBeInTheDocument()
  })

  it('never shows a failure rate that is not zero as 0.0%', async () => {
    const summary = deliverySummary()
    open({
      summary: () =>
        json(
          200,
          deliverySummary({
            completed: {
              ...summary.completed,
              succeeded: 99_999,
              failed: 1,
              failureRate: 1 / 100_000,
            },
          }),
        ),
    })
    const region = await summaryRegion()
    const card = (await within(region).findByRole('heading', { name: 'Failure rate' })).closest(
      'article',
    )!
    expect(within(card).getByText('< 0.1%')).toBeInTheDocument()
    expect(within(card).queryByText('0.0%')).not.toBeInTheDocument()
  })

  it('shows zero data as zeros', async () => {
    open({
      summary: () =>
        json(
          200,
          deliverySummary({
            created: {
              total: 0,
              byStatus: { PENDING: 0, IN_FLIGHT: 0, SUCCEEDED: 0, FAILED: 0, CANCELLED: 0 },
            },
            completed: {
              succeeded: 0,
              failed: 0,
              cancelled: 0,
              failureRate: null,
              byFailureReason: {
                DESTINATION_REJECTED: 0,
                REDIRECT_REFUSED: 0,
                EGRESS_BLOCKED: 0,
                RETRIES_EXHAUSTED: 0,
                INTERNAL_ERROR: 0,
                UNRECORDED: 0,
              },
            },
          }),
        ),
    })
    const region = await summaryRegion()
    const created = await within(region).findByRole('group', { name: 'Created in this window' })
    expect(within(created).getAllByText('0').length).toBeGreaterThanOrEqual(6)
  })

  it('shows a saturated count as a lower bound and the others exactly', async () => {
    const summary = deliverySummary()
    open({
      summary: () =>
        json(
          200,
          deliverySummary({
            created: { ...summary.created, total: 100_000 },
            capped: true,
          }),
        ),
    })
    const region = await summaryRegion()
    expect(await within(region).findByText('≥ 100,000')).toBeInTheDocument()
    expect(within(region).getAllByText('≥ 100,000')).toHaveLength(1)
    expect(within(region).getByText(/lower bounds/)).toBeInTheDocument()
    expect(within(region).getAllByText('30')).toHaveLength(2)
  })

  it('applies a custom range through the URL and reuses it for a refresh', async () => {
    const { calls, user } = open()
    const region = await summaryRegion()
    await within(region).findByText('2026-11-21 17:30:00 UTC')
    const form = within(region).getByRole('form', { name: 'Summary filters' })
    range(form, '2026-11-01T00:00', '2026-11-08T12:30')
    await user.click(within(form).getByRole('button', { name: 'Apply' }))
    await waitFor(() => expect(reads(calls, '/operations/deliveries')).toHaveLength(2))
    expect(reads(calls, '/operations/deliveries')[1]).toEqual({
      from: '2026-11-01T00:00:00Z',
      to: '2026-11-08T12:30:00Z',
    })
    expect(screen.getByTestId('search')).toHaveTextContent(
      'sFrom=2026-11-01T00%3A00%3A00Z&sTo=2026-11-08T12%3A30%3A00Z',
    )
    // The other sections did not reload and did not write to the URL.
    expect(reads(calls, '/operations/failures')).toHaveLength(1)
    expect(screen.getByTestId('search')).not.toHaveTextContent('fFrom')

    await user.click(within(region).getByRole('button', { name: 'Refresh' }))
    await waitFor(() => expect(reads(calls, '/operations/deliveries')).toHaveLength(3))
    expect(reads(calls, '/operations/deliveries')[2]).toEqual(
      reads(calls, '/operations/deliveries')[1],
    )
  })

  it('goes back to the server default window on Reset', async () => {
    const { calls, user } = open(
      {},
      '/o/o1/operations/activity?sFrom=2026-11-01T00:00:00Z&sTo=2026-11-02T00:00:00Z',
    )
    const region = await summaryRegion()
    await within(region).findByText('2026-11-21 17:30:00 UTC')
    expect(reads(calls, '/operations/deliveries')[0]).toEqual({
      from: '2026-11-01T00:00:00Z',
      to: '2026-11-02T00:00:00Z',
    })
    await user.click(within(region).getByRole('button', { name: 'Reset' }))
    await waitFor(() => expect(reads(calls, '/operations/deliveries')).toHaveLength(2))
    expect(reads(calls, '/operations/deliveries')[1]).toEqual({})
    expect(screen.getByTestId('search')).toBeEmptyDOMElement()
  })

  it('asks for a fresh default window on refresh when no range is applied', async () => {
    const { calls, user } = open()
    const region = await summaryRegion()
    await within(region).findByText('2026-11-21 17:30:00 UTC')
    await user.click(within(region).getByRole('button', { name: 'Refresh' }))
    await waitFor(() => expect(reads(calls, '/operations/deliveries')).toHaveLength(2))
    expect(reads(calls, '/operations/deliveries')).toEqual([{}, {}])
  })
})

describe('failure feed', () => {
  it('shows each failure with its reason, last result and a copyable delivery id', async () => {
    open({
      failures: () =>
        json(
          200,
          feedPage([
            failureItem(),
            failureItem({
              deliveryId: DELIVERY_B,
              failureReason: 'UNRECORDED',
              lastHttpStatus: undefined,
              lastErrorClass: undefined,
              destinationName: 'Legacy hook',
            }),
          ]),
        ),
    })
    const table = await screen.findByRole('table', {
      name: 'Failed deliveries, newest completion first',
    })
    const rows = within(table).getAllByRole('row').slice(1)
    expect(rows).toHaveLength(2)
    expect(within(rows[0]!).getByText('Destination rejected')).toBeInTheDocument()
    expect(within(rows[0]!).getByText('HTTP 410 · http_4xx')).toBeInTheDocument()
    expect(within(rows[0]!).getByText('2026-11-22 11:00:00 UTC')).toBeInTheDocument()
    expect(within(rows[0]!).getByText(DELIVERY_A)).toBeInTheDocument()
    expect(
      within(rows[0]!).getByRole('button', { name: `Copy delivery id ${DELIVERY_A}` }),
    ).toBeInTheDocument()
    expect(within(rows[1]!).getByText('Not recorded')).toBeInTheDocument()
    expect(within(rows[1]!).getAllByText('—').length).toBeGreaterThan(0)
    expect(within(rows[0]!).getByRole('link', { name: 'View' })).toHaveAttribute(
      'href',
      `/o/o1/operations/deliveries/${DELIVERY_A}`,
    )
    expect(within(rows[1]!).getByRole('link', { name: 'View' })).toHaveAttribute(
      'href',
      `/o/o1/operations/deliveries/${DELIVERY_B}`,
    )
  })

  it('says that a redriven delivery leaves the list', async () => {
    open()
    const region = await failuresRegion()
    expect(within(region).getByText(/A redriven delivery leaves this list/)).toBeInTheDocument()
  })

  it('shows an empty state', async () => {
    open({ failures: () => json(200, feedPage([])) })
    const region = await failuresRegion()
    expect(await within(region).findByText(/No failed deliveries match/)).toBeInTheDocument()
    expect(within(region).queryByRole('table')).not.toBeInTheDocument()
  })

  it('pages with the cursor the server issued and the window of the first page', async () => {
    const { calls, user } = open({
      failures: (query) =>
        query.get('cursor') === 'cursor-2'
          ? json(200, feedPage([failure(2)], null, { from: 'x', to: 'y' } as never))
          : json(
              200,
              feedPage([failure(1)], 'cursor-2', {
                from: '2026-11-15T17:30:00.5Z',
                to: '2026-11-22T17:30:00.5Z',
              }),
            ),
    })
    const region = await failuresRegion()
    await within(region).findByText('Hook 1')
    const pager = within(region).getByRole('navigation', { name: 'Recent failures pages' })
    expect(within(pager).getByRole('button', { name: 'Previous' })).toBeDisabled()
    await user.click(within(pager).getByRole('button', { name: 'Next' }))
    await within(region).findByText('Hook 2')
    expect(reads(calls, '/operations/failures')[1]).toEqual({
      limit: '25',
      cursor: 'cursor-2',
      from: '2026-11-15T17:30:00.5Z',
      to: '2026-11-22T17:30:00.5Z',
    })
    expect(within(pager).getByRole('button', { name: 'Next' })).toBeDisabled()
    expect(within(pager).getByText('Page 2 (last page)')).toBeInTheDocument()
    // The cursor never reaches the URL.
    expect(screen.getByTestId('search')).toBeEmptyDOMElement()

    await user.click(within(pager).getByRole('button', { name: 'Previous' }))
    expect(await within(region).findByText('Hook 1')).toBeInTheDocument()
    expect(reads(calls, '/operations/failures')).toHaveLength(2)
    // Focus returns to the section heading after a page change.
    expect(within(region).getByRole('heading', { name: 'Recent failures' })).toHaveFocus()
  })

  it('starts again from the first page, without a cursor, when a filter is applied', async () => {
    const { calls, user } = open({
      failures: (query) =>
        query.get('cursor')
          ? json(200, feedPage([failure(2)]))
          : json(200, feedPage([failure(1)], 'cursor-2')),
    })
    const region = await failuresRegion()
    await within(region).findByText('Hook 1')
    await user.click(within(region).getByRole('button', { name: 'Next' }))
    await within(region).findByText('Hook 2')

    const form = within(region).getByRole('form', { name: 'Failure filters' })
    await user.selectOptions(within(form).getByLabelText('Failure reason'), 'Not recorded')
    await user.click(within(form).getByRole('button', { name: 'Apply' }))
    await within(region).findByText('Hook 1')
    const sent = reads(calls, '/operations/failures')
    expect(sent.at(-1)).toEqual({ limit: '25', failureReason: 'UNRECORDED' })
    expect(screen.getByTestId('search')).toHaveTextContent('fReason=UNRECORDED')
    expect(within(region).getByText('Page 1')).toBeInTheDocument()
  })

  it('keeps a custom range for every page and for a refresh, whatever the server echoes', async () => {
    const { calls, user } = open({
      failures: (query) =>
        query.get('cursor')
          ? json(200, feedPage([failure(2)]))
          : json(200, feedPage([failure(1)], 'cursor-2')),
    })
    const region = await failuresRegion()
    await within(region).findByText('Hook 1')
    const form = within(region).getByRole('form', { name: 'Failure filters' })
    range(form, '2026-11-01T00:00', '2026-11-03T00:00')
    await user.click(within(form).getByRole('button', { name: 'Apply' }))
    await waitFor(() => expect(reads(calls, '/operations/failures')).toHaveLength(2))
    await within(region).findByText('Hook 1')
    await user.click(within(region).getByRole('button', { name: 'Next' }))
    await within(region).findByText('Hook 2')
    await user.click(within(region).getByRole('button', { name: 'Refresh' }))
    await waitFor(() => expect(reads(calls, '/operations/failures')).toHaveLength(4))
    const window = { from: '2026-11-01T00:00:00Z', to: '2026-11-03T00:00:00Z' }
    expect(reads(calls, '/operations/failures').slice(1)).toEqual([
      { limit: '25', ...window },
      { limit: '25', cursor: 'cursor-2', ...window },
      { limit: '25', ...window },
    ])
  })

  it('asks for a fresh default window, from the first page, when refreshing a rolling window', async () => {
    const { calls, user } = open({
      failures: (query) =>
        query.get('cursor')
          ? json(200, feedPage([failure(2)]))
          : json(200, feedPage([failure(1)], 'cursor-2')),
    })
    const region = await failuresRegion()
    await within(region).findByText('Hook 1')
    await user.click(within(region).getByRole('button', { name: 'Next' }))
    await within(region).findByText('Hook 2')
    await user.click(within(region).getByRole('button', { name: 'Refresh' }))
    await within(region).findByText('Hook 1')
    expect(reads(calls, '/operations/failures').at(-1)).toEqual({ limit: '25' })
  })

  it('refuses an invalid range before any request and focuses the field', async () => {
    const { calls, user } = open()
    const region = await failuresRegion()
    await within(region).findByRole('table')
    const form = within(region).getByRole('form', { name: 'Failure filters' })
    range(form, '2026-11-05T00:00', '2026-11-01T00:00')
    await user.click(within(form).getByRole('button', { name: 'Apply' }))
    expect(await within(form).findByText('To must be after From.')).toBeInTheDocument()
    expect(within(form).getByLabelText('To (UTC)')).toHaveFocus()
    expect(reads(calls, '/operations/failures')).toHaveLength(1)

    range(form, '2026-09-01T00:00', '2026-11-01T00:00')
    await user.click(within(form).getByRole('button', { name: 'Apply' }))
    expect(await within(form).findByText('The window must not exceed 31 days.')).toBeInTheDocument()
    expect(reads(calls, '/operations/failures')).toHaveLength(1)
  })

  it('shows a read timeout with a hint and retries only that section', async () => {
    let reads_ = 0
    const { calls, user } = open({
      failures: () => {
        reads_ += 1
        return reads_ === 1
          ? apiError(
              503,
              'read_model_timeout',
              'the query took too long; try a shorter time window',
            )
          : json(200, feedPage([failure(1)]))
      },
    })
    const region = await failuresRegion()
    const alert = await within(region).findByRole('alert')
    expect(alert).toHaveTextContent('the query took too long')
    expect(alert).toHaveTextContent('Try a shorter time window or a narrower filter.')
    // The other sections are unaffected.
    expect(
      await screen.findByRole('table', { name: 'Recorded attempts, newest start first' }),
    ).toBeInTheDocument()
    await user.click(within(alert).getByRole('button', { name: 'Try again' }))
    expect(await within(region).findByText('Hook 1')).toBeInTheDocument()
    expect(reads(calls, '/operations/attempts')).toHaveLength(1)
    expect(reads(calls, '/operations/deliveries')).toHaveLength(1)
  })

  it('shows a safe error for a malformed response, and does not crash', async () => {
    open({ failures: () => json(200, { items: 'nope' }) })
    const region = await failuresRegion()
    const alert = await within(region).findByRole('alert')
    expect(alert).toHaveTextContent('could not read')
    expect(screen.getByRole('heading', { name: 'Attempt activity' })).toBeInTheDocument()
  })

  it('sends the destination filter to the server and never filters the rows it gets back', async () => {
    const { calls, user } = open({
      destinations: () =>
        json(
          200,
          destinationPage(
            [
              { id: DEST_A, name: 'Billing hook' },
              { id: DEST_B, name: 'Audit hook', status: 'REVOKED' },
            ],
            2,
          ),
        ),
      // The server answers with rows of both destinations whatever is asked: the page must show them all.
      failures: () =>
        json(
          200,
          feedPage([
            failureItem({
              deliveryId: DELIVERY_A,
              destinationId: DEST_A,
              destinationName: 'Billing hook',
            }),
            failureItem({
              deliveryId: DELIVERY_B,
              destinationId: DEST_B,
              destinationName: 'Audit hook',
            }),
          ]),
        ),
    })
    const region = await failuresRegion()
    await within(region).findByRole('table')
    const form = within(region).getByRole('form', { name: 'Failure filters' })
    await user.click(within(form).getByRole('button', { name: 'Load destinations' }))
    await user.selectOptions(
      await within(form).findByLabelText('Destination'),
      `Audit hook — REVOKED (${DEST_B.slice(0, 8)})`,
    )
    await user.click(within(form).getByRole('button', { name: 'Apply' }))
    await waitFor(() => expect(reads(calls, '/operations/failures')).toHaveLength(2))
    expect(reads(calls, '/operations/failures')[1]).toEqual({ limit: '25', destinationId: DEST_B })
    expect(screen.getByTestId('search')).toHaveTextContent(`fDest=${DEST_B}`)
    // Both returned rows are still shown, including the one of the destination that was not selected.
    const table = within(region).getByRole('table', {
      name: 'Failed deliveries, newest completion first',
    })
    expect(within(table).getAllByRole('row')).toHaveLength(3)
    expect(within(table).getByText('Billing hook')).toBeInTheDocument()
    expect(within(table).getByText('Audit hook')).toBeInTheDocument()
  })

  it('does not move focus later when a page request failed, for example after Refresh', async () => {
    let cursorReads = 0
    const { calls, user } = open({
      failures: (query) => {
        if (!query.get('cursor')) return json(200, feedPage([failure(1)], 'cursor-2'))
        cursorReads += 1
        return apiError(500, 'internal_error', 'secret detail')
      },
    })
    const region = await failuresRegion()
    await within(region).findByText('Hook 1')
    await user.click(within(region).getByRole('button', { name: 'Next' }))
    expect(await within(region).findByRole('alert')).toBeInTheDocument()
    expect(cursorReads).toBe(1)
    const heading = within(region).getByRole('heading', { name: 'Recent failures' })
    expect(heading).not.toHaveFocus()

    await user.click(within(region).getByRole('button', { name: 'Refresh' }))
    await waitFor(() => expect(reads(calls, '/operations/failures')).toHaveLength(3))
    await within(region).findByRole('button', { name: 'Refresh' })
    await within(region).findByText('Hook 1')
    expect(heading).not.toHaveFocus()
    expect(within(region).queryByRole('alert')).not.toBeInTheDocument()
  })

  it('moves focus to the heading after Next succeeds', async () => {
    const { user } = open({
      failures: (query) =>
        query.get('cursor')
          ? json(200, feedPage([failure(2)]))
          : json(200, feedPage([failure(1)], 'c2')),
    })
    const region = await failuresRegion()
    await within(region).findByText('Hook 1')
    await user.click(within(region).getByRole('button', { name: 'Next' }))
    await within(region).findByText('Hook 2')
    expect(within(region).getByRole('heading', { name: 'Recent failures' })).toHaveFocus()
  })

  it('has its live region ready before the rows arrive, then announces the page', async () => {
    const answer = gate()
    open({
      failures: async () => {
        await answer.promise
        return json(200, feedPage([failure(1)]))
      },
    })
    const region = await failuresRegion()
    const live = region.querySelector('p[role="status"].visually-hidden')
    expect(live).not.toBeNull()
    expect(live).toBeEmptyDOMElement()
    await act(async () => answer.release())
    await within(region).findByText('Hook 1')
    expect(live).toHaveTextContent('Page 1 loaded, 1 rows.')
    // The same element was updated, not replaced.
    expect(region.querySelector('p[role="status"].visually-hidden')).toBe(live)
  })

  it('explains each failure reason as text, not only in a title attribute', async () => {
    open()
    const region = await failuresRegion()
    await within(region).findByRole('table')
    expect(within(region).getByText('What the failure reasons mean')).toBeInTheDocument()
    expect(
      within(region).getByText('The destination answered with a final 4xx response.'),
    ).toBeInTheDocument()
    expect(
      within(region).getByText('The delivery failed before failure reasons were recorded.'),
    ).toBeInTheDocument()
    expect(region.querySelectorAll('[title]')).toHaveLength(0)
    const attempts = await attemptsRegion()
    expect(attempts.querySelectorAll('[title]')).toHaveLength(0)
  })
})

describe('destination selector', () => {
  it('loads on request, shows only name, status and a short id, and never the url', async () => {
    const { calls, user, container } = open({
      destinations: () =>
        json(200, destinationPage([{ id: DEST_A, name: 'Billing hook', status: 'PAUSED' }], 1)),
    })
    const region = await failuresRegion()
    await within(region).findByRole('table')
    expect(reads(calls, '/destinations')).toEqual([])
    await user.click(within(region).getByRole('button', { name: 'Load destinations' }))
    expect(
      await within(region).findByRole('option', {
        name: `Billing hook — PAUSED (${DEST_A.slice(0, 8)})`,
      }),
    ).toBeInTheDocument()
    expect(reads(calls, '/destinations')).toEqual([{ limit: '100', offset: '0' }])
    expect(container.textContent).not.toContain('secret-host')
    expect(within(region).getByText('All 1 destinations are loaded.')).toBeInTheDocument()
  })

  it('pages beyond 100, merges by id, and says the list can change while loading', async () => {
    const all = Array.from({ length: 150 }, (_, i) => ({ id: uuid(i + 1), name: `Dest ${i + 1}` }))
    const { calls, user } = open({
      destinations: (query) => {
        const offset = Number(query.get('offset'))
        // The second page repeats the last row of the first one, as can happen if a destination was created meanwhile.
        const items = offset === 0 ? all.slice(0, 100) : all.slice(99, 150)
        return json(200, destinationPage(items, 150, offset))
      },
    })
    const region = await failuresRegion()
    await within(region).findByRole('table')
    expect(within(region).getByText(/can change while you load it/)).toBeInTheDocument()
    await user.click(within(region).getByRole('button', { name: 'Load destinations' }))
    const more = await within(region).findByRole('button', { name: 'Load more (100 of 150)' })
    await user.click(more)
    await within(region).findByText('All 150 destinations are loaded.')
    expect(reads(calls, '/destinations').map((q) => q.offset)).toEqual(['0', '100'])
    const select = within(region).getByLabelText('Destination')
    // 150 destinations plus "All destinations": the repeated row was merged, not shown twice.
    expect(within(select).getAllByRole('option')).toHaveLength(151)
  })

  it('shows a destination error without blocking the feed, and can retry', async () => {
    let reads_ = 0
    const { user } = open({
      destinations: () => {
        reads_ += 1
        return reads_ === 1
          ? apiError(403, 'forbidden', 'you do not have permission to perform this action')
          : json(200, destinationPage([{ id: DEST_A, name: 'Billing hook' }], 1))
      },
    })
    const region = await failuresRegion()
    await within(region).findByRole('table')
    await user.click(within(region).getByRole('button', { name: 'Load destinations' }))
    expect(
      await within(region).findByText('you do not have permission to perform this action'),
    ).toBeInTheDocument()
    expect(within(region).getByRole('table')).toBeInTheDocument()
    await user.click(within(region).getByRole('button', { name: 'Load destinations' }))
    expect(await within(region).findByText('All 1 destinations are loaded.')).toBeInTheDocument()
  })

  it('names a destination from the link that is not loaded yet by its id', async () => {
    const { calls } = open({}, `/o/o1/operations/activity?fDest=${DEST_A}`)
    const region = await failuresRegion()
    await within(region).findByRole('table')
    expect(reads(calls, '/operations/failures')[0]).toEqual({ limit: '25', destinationId: DEST_A })
    expect(
      within(region).getByRole('option', { name: `Destination ${DEST_A} (not loaded)` }),
    ).toBeInTheDocument()
  })
})

describe('attempt feed', () => {
  const outcomes = [
    ['SUCCESS', 'Success'],
    ['HTTP_ERROR', 'HTTP error'],
    ['REDIRECT', 'Redirect'],
    ['NETWORK_ERROR', 'Network error'],
    ['TIMEOUT', 'Timeout'],
    ['BLOCKED_BY_POLICY', 'Blocked by policy'],
    ['ABANDONED', 'Abandoned'],
  ] as const

  it('shows every outcome as text, ABANDONED without an HTTP status, and a value it does not know', async () => {
    const items = [
      ...outcomes.map(([outcome], i) =>
        attemptItem({
          attemptId: uuid(100 + i),
          outcome,
          httpStatus: outcome === 'ABANDONED' ? undefined : 200,
          errorClass: undefined,
        }),
      ),
      attemptItem({ attemptId: uuid(200), outcome: 'SOMETHING_NEW' }),
    ]
    open({ attempts: () => json(200, feedPage(items)) })
    const table = await screen.findByRole('table', {
      name: 'Recorded attempts, newest start first',
    })
    const rows = within(table).getAllByRole('row').slice(1)
    expect(rows).toHaveLength(8)
    outcomes.forEach(([, label], i) => {
      expect(within(rows[i]!).getByText(label)).toBeInTheDocument()
    })
    expect(within(rows[6]!).getAllByRole('cell')[5]).toHaveTextContent('—')
    expect(within(rows[7]!).getByText('SOMETHING_NEW')).toBeInTheDocument()
    const region = await attemptsRegion()
    expect(
      within(region).getByText(
        'A worker did not finish this attempt. This does not mean the destination failed.',
      ),
    ).toBeInTheDocument()
  })

  it('marks an attempt that ended a failure episode and nothing else', async () => {
    open({
      attempts: () =>
        json(
          200,
          feedPage([
            attemptItem({
              attemptId: uuid(1),
              attemptNumber: 3,
              terminalReason: 'RETRIES_EXHAUSTED',
            }),
            attemptItem({ attemptId: uuid(2), attemptNumber: 2 }),
            attemptItem({
              attemptId: uuid(3),
              attemptNumber: 6,
              terminalReason: 'DESTINATION_REJECTED',
            }),
          ]),
        ),
    })
    const table = await screen.findByRole('table', {
      name: 'Recorded attempts, newest start first',
    })
    const rows = within(table).getAllByRole('row').slice(1)
    expect(
      within(rows[0]!).getByText('Ended a failure episode: Retries exhausted'),
    ).toBeInTheDocument()
    expect(within(rows[1]!).queryByText(/Ended a failure episode/)).not.toBeInTheDocument()
    // A redriven delivery that failed again has one marked attempt per episode.
    expect(
      within(rows[2]!).getByText('Ended a failure episode: Destination rejected'),
    ).toBeInTheDocument()
    expect(within(rows[0]!).getByText('#3')).toBeInTheDocument()
  })

  it('has no destination control and says why', async () => {
    const { calls, user } = open({
      destinations: () => json(200, destinationPage([{ id: DEST_A, name: 'Billing hook' }], 1)),
    })
    const region = await attemptsRegion()
    await within(region).findByRole('table')
    expect(within(region).queryByLabelText('Destination')).not.toBeInTheDocument()
    expect(within(region).queryByRole('button', { name: /destinations/i })).not.toBeInTheDocument()
    expect(within(region).getByText(/not by destination/)).toBeInTheDocument()

    // Choosing a destination for the failures never changes the attempt requests.
    const failures = await failuresRegion()
    await user.click(within(failures).getByRole('button', { name: 'Load destinations' }))
    await user.selectOptions(
      await within(failures).findByLabelText('Destination'),
      `Billing hook — ACTIVE (${DEST_A.slice(0, 8)})`,
    )
    await user.click(within(failures).getByRole('button', { name: 'Apply' }))
    await waitFor(() => expect(reads(calls, '/operations/failures')).toHaveLength(2))
    expect(reads(calls, '/operations/attempts')).toEqual([{ limit: '25' }])
    expect(
      calls.some((c) => c.path.includes('/attempts') && c.path.includes('destinationId')),
    ).toBe(false)
  })

  it('filters by outcome and delivery, and refuses a delivery id that is not a UUID', async () => {
    const { calls, user } = open()
    const region = await attemptsRegion()
    await within(region).findByRole('table')
    const form = within(region).getByRole('form', { name: 'Attempt filters' })
    await user.selectOptions(within(form).getByLabelText('Outcome'), 'Abandoned')
    await user.type(within(form).getByLabelText('Delivery ID'), 'not-a-uuid')
    await user.click(within(form).getByRole('button', { name: 'Apply' }))
    expect(await within(form).findByText('Enter a delivery ID in UUID format.')).toBeInTheDocument()
    expect(within(form).getByLabelText('Delivery ID')).toHaveFocus()
    expect(reads(calls, '/operations/attempts')).toHaveLength(1)

    await user.clear(within(form).getByLabelText('Delivery ID'))
    await user.type(within(form).getByLabelText('Delivery ID'), DELIVERY_A.toUpperCase())
    await user.click(within(form).getByRole('button', { name: 'Apply' }))
    await waitFor(() => expect(reads(calls, '/operations/attempts')).toHaveLength(2))
    expect(reads(calls, '/operations/attempts')[1]).toEqual({
      limit: '25',
      outcome: 'ABANDONED',
      deliveryId: DELIVERY_A,
    })
    expect(screen.getByTestId('search')).toHaveTextContent(`aDelivery=${DELIVERY_A}`)
  })

  it('pages with its own cursor, independently of the failures', async () => {
    const { calls, user } = open({
      failures: () => json(200, feedPage([failure(1)], 'failure-cursor')),
      attempts: (query) =>
        query.get('cursor')
          ? json(200, feedPage([attemptItem({ attemptId: uuid(2), attemptNumber: 2 })]))
          : json(200, feedPage([attemptItem({ attemptId: uuid(1) })], 'attempt-cursor')),
    })
    const region = await attemptsRegion()
    await within(region).findByRole('table')
    await user.click(within(region).getByRole('button', { name: 'Next' }))
    await within(region).findByText('#2')
    expect(reads(calls, '/operations/attempts')[1]).toMatchObject({ cursor: 'attempt-cursor' })
    expect(reads(calls, '/operations/failures')).toHaveLength(1)
  })

  it('shows an empty state', async () => {
    open({ attempts: () => json(200, feedPage([])) })
    const region = await attemptsRegion()
    expect(await within(region).findByText(/No attempts match/)).toBeInTheDocument()
  })
})

describe('from a failure to its attempts', () => {
  const DEFAULT_WINDOW = { from: '2026-11-15T17:30:00.5Z', to: '2026-11-22T17:30:00.5Z' }

  /** A failure feed that echoes the window it was asked for, like the real one. */
  const failuresOf = (item: ReturnType<typeof failureItem>) => (query: URLSearchParams) =>
    json(
      200,
      feedPage([item], null, {
        from: query.get('from') ?? DEFAULT_WINDOW.from,
        to: query.get('to') ?? DEFAULT_WINDOW.to,
      }),
    )

  const show = async (delivery = DELIVERY_A) =>
    within(await failuresRegion()).findByRole('button', {
      name: `Show attempts of delivery ${delivery}`,
    })

  it('covers the whole lifetime of a delivery that fits in 31 days, from the delivery and not from the feed', async () => {
    const { calls, user } = open({
      failures: failuresOf(
        failureItem({ createdAt: '2026-11-20T10:00:30Z', completedAt: '2026-11-22T11:00:00.5Z' }),
      ),
    })
    await user.click(await show())
    await waitFor(() => expect(reads(calls, '/operations/attempts')).toHaveLength(2))
    expect(reads(calls, '/operations/attempts')[1]).toEqual({
      limit: '25',
      deliveryId: DELIVERY_A,
      from: '2026-11-20T10:00:00Z',
      to: '2026-11-22T11:01:00Z',
    })
    expect(screen.getByRole('heading', { name: 'Attempt activity' })).toHaveFocus()
    expect(reads(calls, '/operations/failures')).toHaveLength(1)
    const search = screen.getByTestId('search')
    expect(search).toHaveTextContent(`aDelivery=${DELIVERY_A}`)
    expect(search).not.toHaveTextContent('cursor')
    // Complete, so no truncation warning. The section still says it only shows its own window.
    expect(screen.queryByText(/Older attempts are excluded/)).not.toBeInTheDocument()
    expect(
      screen.getByText(/Only attempts that started inside this section’s window/),
    ).toBeInTheDocument()
  })

  it('ignores a custom failure range that reaches into the future', async () => {
    const { calls, user } = open({
      failures: failuresOf(
        failureItem({ createdAt: '2026-11-05T09:00:00Z', completedAt: '2026-11-21T09:00:00Z' }),
      ),
    })
    const failures = await failuresRegion()
    await within(failures).findByRole('table')
    const form = within(failures).getByRole('form', { name: 'Failure filters' })
    // The window ends 29 days after the failure. Anchored to it, the look-back would stop at 2026-11-19.
    range(form, '2026-11-20T00:00', '2026-12-20T00:00')
    await user.click(within(form).getByRole('button', { name: 'Apply' }))
    await user.click(await show())
    await waitFor(() => expect(reads(calls, '/operations/attempts')).toHaveLength(2))
    expect(reads(calls, '/operations/attempts')[1]).toEqual({
      limit: '25',
      deliveryId: DELIVERY_A,
      from: '2026-11-05T09:00:00Z',
      to: '2026-11-21T09:00:00Z',
    })
    expect(screen.queryByText(/Older attempts are excluded/)).not.toBeInTheDocument()
  })

  it('cuts a redriven delivery older than 31 days, says so, and shows both failure episodes in the window', async () => {
    const { calls, user } = open({
      failures: failuresOf(
        failureItem({
          attemptCount: 14,
          createdAt: '2026-09-01T08:20:10Z',
          completedAt: '2026-11-22T11:00:00Z',
        }),
      ),
      attempts: (query) =>
        query.get('deliveryId')
          ? json(
              200,
              feedPage([
                attemptItem({
                  attemptId: uuid(14),
                  attemptNumber: 14,
                  startedAt: '2026-11-22T10:59:00Z',
                  terminalReason: 'DESTINATION_REJECTED',
                }),
                attemptItem({
                  attemptId: uuid(8),
                  attemptNumber: 8,
                  startedAt: '2026-10-25T10:00:00Z',
                  outcome: 'TIMEOUT',
                  terminalReason: 'RETRIES_EXHAUSTED',
                }),
              ]),
            )
          : json(200, feedPage([attemptItem()])),
    })
    await user.click(await show())
    await waitFor(() => expect(reads(calls, '/operations/attempts')).toHaveLength(2))
    // The 31 days that end at the latest completion: never more than the backend allows.
    expect(reads(calls, '/operations/attempts')[1]).toEqual({
      limit: '25',
      deliveryId: DELIVERY_A,
      from: '2026-10-22T11:00:00Z',
      to: '2026-11-22T11:00:00Z',
    })
    const warning = await screen.findByText(/Older attempts are excluded/)
    expect(warning).toHaveTextContent('2026-09-01 08:20:10 UTC')
    expect(warning).toHaveTextContent("not the delivery's complete history")

    const region = await attemptsRegion()
    const table = await within(region).findByRole('table', {
      name: 'Recorded attempts, newest start first',
    })
    expect(
      within(table).getByText('Ended a failure episode: Destination rejected'),
    ).toBeInTheDocument()
    expect(
      within(table).getByText('Ended a failure episode: Retries exhausted'),
    ).toBeInTheDocument()
    expect(within(table).getByText('#14')).toBeInTheDocument()
    expect(within(table).getByText('#8')).toBeInTheDocument()

    // Changing the outcome keeps the same cut window, so the warning stays.
    const form = within(region).getByRole('form', { name: 'Attempt filters' })
    await user.selectOptions(within(form).getByLabelText('Outcome'), 'Timeout')
    await user.click(within(form).getByRole('button', { name: 'Apply' }))
    await waitFor(() => expect(reads(calls, '/operations/attempts')).toHaveLength(3))
    expect(screen.getByText(/Older attempts are excluded/)).toBeInTheDocument()

    // Choosing another range is the user's own view: the warning about the cut window goes away, and the section
    // still says it shows only its own window.
    range(form, '2026-09-01T00:00', '2026-09-20T00:00')
    await user.click(within(form).getByRole('button', { name: 'Apply' }))
    await waitFor(() => expect(reads(calls, '/operations/attempts')).toHaveLength(4))
    expect(screen.queryByText(/Older attempts are excluded/)).not.toBeInTheDocument()
    expect(
      screen.getByText(/Only attempts that started inside this section’s window/),
    ).toBeInTheDocument()
  })

  it('says nothing about truncation for a delivery of exactly 31 days', async () => {
    const { user } = open({
      failures: failuresOf(
        failureItem({ createdAt: '2026-10-22T11:00:00Z', completedAt: '2026-11-22T11:00:00Z' }),
      ),
    })
    await user.click(await show())
    await within(await attemptsRegion()).findByRole('table')
    expect(screen.queryByText(/Older attempts are excluded/)).not.toBeInTheDocument()
  })

  it('does not leave focus pending when the URL does not change', async () => {
    const { calls, user } = open({
      failures: failuresOf(failureItem({ createdAt: '2026-11-20T10:00:00Z' })),
    })
    const button = await show()
    await user.click(button)
    await waitFor(() => expect(reads(calls, '/operations/attempts')).toHaveLength(2))
    const heading = screen.getByRole('heading', { name: 'Attempt activity' })
    expect(heading).toHaveFocus()

    // The same click again leaves the URL as it is. Focus goes to the heading once more, and nothing stays pending.
    await user.click(button)
    expect(heading).toHaveFocus()
    expect(reads(calls, '/operations/attempts')).toHaveLength(2)

    // An unrelated Apply later must not pull focus to the attempt section.
    const summary = await summaryRegion()
    const form = within(summary).getByRole('form', { name: 'Summary filters' })
    range(form, '2026-11-01T00:00', '2026-11-08T00:00')
    await user.click(within(form).getByRole('button', { name: 'Apply' }))
    await waitFor(() => expect(reads(calls, '/operations/deliveries')).toHaveLength(2))
    expect(heading).not.toHaveFocus()
    expect(within(form).getByRole('button', { name: 'Apply' })).toHaveFocus()
  })
})

describe('links', () => {
  it('applies the filters of a link to the right sections', async () => {
    const { calls } = open(
      {},
      `/o/o1/operations/activity?fReason=EGRESS_BLOCKED&aOutcome=TIMEOUT&sFrom=2026-11-01T00:00:00Z&sTo=2026-11-02T00:00:00Z`,
    )
    await loaded()
    expect(reads(calls, '/operations/deliveries')[0]).toEqual({
      from: '2026-11-01T00:00:00Z',
      to: '2026-11-02T00:00:00Z',
    })
    expect(reads(calls, '/operations/failures')[0]).toEqual({
      limit: '25',
      failureReason: 'EGRESS_BLOCKED',
    })
    expect(reads(calls, '/operations/attempts')[0]).toEqual({ limit: '25', outcome: 'TIMEOUT' })
    const failures = await failuresRegion()
    expect(within(failures).getByLabelText('Failure reason')).toHaveValue('EGRESS_BLOCKED')
    expect(screen.queryByText(/were not valid/)).not.toBeInTheDocument()
  })

  it('ignores a value that is not valid, says so, and does not send it', async () => {
    const { calls } = open(
      {},
      '/o/o1/operations/activity?fReason=nope&fFrom=2026-11-01T00:00:00Z&cursor=abc',
    )
    await loaded()
    expect(
      await screen.findByText('Some filters in the link were not valid and were ignored.'),
    ).toBeInTheDocument()
    expect(reads(calls, '/operations/failures')[0]).toEqual({ limit: '25' })
  })

  it('removes the ignored values and the notice when the user clears them, without a new read', async () => {
    const { calls, user } = open(
      {},
      '/o/o1/operations/activity?fReason=nope&fFrom=2026-11-01T00:00:00Z&other=keep',
    )
    await loaded()
    await user.click(await screen.findByRole('button', { name: 'Clear ignored filters' }))
    expect(screen.queryByText(/were not valid/)).not.toBeInTheDocument()
    expect(screen.getByTestId('search')).toHaveTextContent(/^\?other=keep$/)
    expect(reads(calls, '/operations/failures')).toHaveLength(1)
  })

  it('does not keep the notice after the user applies any section', async () => {
    const { user } = open({}, '/o/o1/operations/activity?aOutcome=nope&fReason=EGRESS_BLOCKED')
    await loaded()
    expect(await screen.findByText(/were not valid/)).toBeInTheDocument()
    const summary = await summaryRegion()
    const form = within(summary).getByRole('form', { name: 'Summary filters' })
    range(form, '2026-11-01T00:00', '2026-11-08T00:00')
    await user.click(within(form).getByRole('button', { name: 'Apply' }))
    await waitFor(() => expect(screen.queryByText(/were not valid/)).not.toBeInTheDocument())
    const search = screen.getByTestId('search')
    expect(search).not.toHaveTextContent('aOutcome')
    // What was valid stays.
    expect(search).toHaveTextContent('fReason=EGRESS_BLOCKED')
    expect(search).toHaveTextContent('sFrom=')
  })
})

describe('independent states', () => {
  it('keeps the feeds when the summary fails, and retries only the summary', async () => {
    let n = 0
    const { calls, user } = open({
      summary: () => {
        n += 1
        return n === 1
          ? apiError(
              503,
              'read_model_timeout',
              'the query took too long; try a shorter time window',
            )
          : json(200, deliverySummary())
      },
    })
    const region = await summaryRegion()
    const alert = await within(region).findByRole('alert')
    expect(alert).toHaveTextContent('the query took too long')
    expect(alert).toHaveTextContent('Try a shorter time window.')
    expect(
      await screen.findByRole('table', { name: 'Failed deliveries, newest completion first' }),
    ).toBeInTheDocument()
    await user.click(within(alert).getByRole('button', { name: 'Try again' }))
    expect(await within(region).findByText('16.7%')).toBeInTheDocument()
    expect(reads(calls, '/operations/failures')).toHaveLength(1)
    expect(reads(calls, '/operations/attempts')).toHaveLength(1)
  })

  it('shows the permission message of a 403 in every section', async () => {
    const denied = () =>
      apiError(403, 'forbidden', 'you do not have permission to perform this action')
    open({ summary: denied, failures: denied, attempts: denied })
    expect(
      await screen.findAllByText('you do not have permission to perform this action'),
    ).toHaveLength(3)
  })

  it('disables Refresh while a section loads and keeps its rows', async () => {
    const second = gate()
    let n = 0
    const { user } = open({
      failures: async () => {
        n += 1
        if (n === 2) await second.promise
        return json(200, feedPage([failure(n === 1 ? 1 : 2)]))
      },
    })
    const region = await failuresRegion()
    await within(region).findByText('Hook 1')
    await user.click(within(region).getByRole('button', { name: 'Refresh' }))
    expect(await within(region).findByRole('button', { name: 'Loading…' })).toBeDisabled()
    expect(within(region).getByText('Hook 1')).toBeInTheDocument()
    await act(async () => second.release())
    expect(await within(region).findByText('Hook 2')).toBeInTheDocument()
  })
})

describe('organization switching', () => {
  it('cancels the old reads and never shows another organization’s rows', async () => {
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
    const o1Gate = gate()
    const calls = stubFetch(async (call) => {
      const o1 = call.path.includes('/organizations/o1/')
      if (o1) await o1Gate.promise
      if (call.path.includes('/operations/deliveries')) return json(200, deliverySummary())
      if (call.path.includes('/operations/failures')) {
        return json(
          200,
          feedPage([failureItem({ destinationName: o1 ? 'Org one hook' : 'Org two hook' })]),
        )
      }
      return json(
        200,
        feedPage([attemptItem({ destinationName: o1 ? 'Org one hook' : 'Org two hook' })]),
      )
    })
    const user = userEvent.setup()
    render(
      <AuthContext.Provider value={auth}>
        <ToastProvider>
          <MemoryRouter initialEntries={['/o/o1/operations/activity']}>
            <Link to="/o/o2/operations/activity">to o2</Link>
            <Routes>
              <Route path="/o/:orgId" element={<OrgLayout />}>
                <Route path="operations/activity" element={<ActivityPage />} />
              </Route>
            </Routes>
          </MemoryRouter>
        </ToastProvider>
      </AuthContext.Provider>,
    )
    await user.click(screen.getByRole('link', { name: 'to o2' }))
    expect((await screen.findAllByText('Org two hook')).length).toBeGreaterThan(0)
    const o1Reads = calls.filter((c) => c.path.includes('/organizations/o1/'))
    expect(o1Reads).toHaveLength(3)
    expect(o1Reads.every((c) => c.signal?.aborted === true)).toBe(true)
    await act(async () => o1Gate.release())
    expect(screen.queryByText('Org one hook')).not.toBeInTheDocument()
  })
})
