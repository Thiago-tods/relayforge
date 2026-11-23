import { act, screen, waitFor, within } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { apiError, json, meBody, org, stubFetch } from '../../test/fetchStub'
import { fakeChannel, renderApp } from '../../test/renderApp'

vi.mock('../../auth/crossTab', () => ({ openAuthChannel: vi.fn() }))

const ID = '11111111-1111-4111-8111-111111111111'
const path = (orgId: string, id = ID) => `/api/v1/organizations/${orgId}/deliveries/${id}`
const delivery = {
  id: ID,
  organizationId: 'o1',
  eventId: '22222222-2222-4222-8222-222222222222',
  destinationId: '33333333-3333-4333-8333-333333333333',
  destinationName: 'Orders webhook',
  status: 'FAILED',
  attemptCount: 3,
  maxAttempts: 8,
  lastHttpStatus: 503,
  lastErrorClass: 'server_error',
  failureReason: 'RETRIES_EXHAUSTED',
  createdAt: '2026-11-20T10:00:00Z',
  updatedAt: '2026-11-20T10:03:00Z',
  completedAt: '2026-11-20T10:03:00Z',
}
const attempts = [
  {
    attemptNumber: 1,
    startedAt: '2026-11-20T10:00:00Z',
    finishedAt: '2026-11-20T10:00:01Z',
    durationMs: 1000,
    outcome: 'HTTP_ERROR',
    httpStatus: 500,
    errorClass: 'server_error',
  },
  {
    attemptNumber: 2,
    startedAt: '2026-11-20T10:01:00Z',
    finishedAt: '2026-11-20T10:01:01Z',
    durationMs: 1000,
    outcome: 'ABANDONED',
  },
  {
    attemptNumber: 3,
    startedAt: '2026-11-20T10:02:00Z',
    finishedAt: '2026-11-20T10:02:01Z',
    durationMs: 1000,
    outcome: 'HTTP_ERROR',
    terminalReason: 'RETRIES_EXHAUSTED',
  },
].map((item) => ({ httpStatus: null, errorClass: null, terminalReason: null, ...item }))

function open(answer: Response, url = `/o/o1/operations/deliveries/${ID}`) {
  fakeChannel()
  const calls = stubFetch((call) => {
    if (call.path === '/api/v1/auth/me') return json(200, meBody([org('o1'), org('o2')]))
    if (call.path === path('o1')) return answer
    return apiError(404, 'not_found', 'resource not found')
  })
  return { calls, ...renderApp(url) }
}

describe('delivery detail page', () => {
  it('loads the organization-scoped endpoint once and shows metadata plus every attempt newest first', async () => {
    const { calls } = open(json(200, { delivery, attempts }))
    expect(await screen.findByRole('heading', { name: 'FAILED', level: 1 })).toBeInTheDocument()
    expect(screen.getByText('Orders webhook')).toBeInTheDocument()
    const overview = screen.getByRole('region', { name: 'Delivery overview' })
    expect(within(overview).getByText('RETRIES_EXHAUSTED')).toBeInTheDocument()
    // Updated and Completed share this instant, so read the value that belongs to the Completed label.
    expect(within(overview).getByText('Completed').nextElementSibling).toHaveTextContent(
      '2026-11-20 10:03:00 UTC',
    )
    const history = screen.getByRole('region', { name: 'Attempt history' })
    const rows = within(history).getAllByText(/Attempt #/)
    expect(rows.map((row) => row.textContent)).toEqual(['Attempt #3', 'Attempt #2', 'Attempt #1'])
    expect(within(history).getByText(/worker did not finish this attempt/i)).toBeInTheDocument()
    expect(calls.filter((call) => call.path.includes('/deliveries/'))).toHaveLength(1)
    expect(calls.find((call) => call.path.includes('/deliveries/'))?.path).toBe(path('o1'))
  })

  it('separates failure episodes only where terminal markers exist', async () => {
    open(
      json(200, {
        delivery,
        attempts: [
          { ...attempts[0], attemptNumber: 1, terminalReason: 'INTERNAL_ERROR' },
          { ...attempts[1], attemptNumber: 2, terminalReason: null },
          { ...attempts[2], attemptNumber: 3, terminalReason: 'RETRIES_EXHAUSTED' },
        ],
      }),
    )
    const history = await screen.findByRole('region', { name: 'Attempt history' })
    const episodes = within(history).getAllByRole('region')
    expect(episodes).toHaveLength(2)
    expect(
      within(episodes[0]!).getByRole('heading', { name: /RETRIES_EXHAUSTED/ }),
    ).toBeInTheDocument()
    expect(
      within(episodes[1]!).getByRole('heading', { name: /INTERNAL_ERROR/ }),
    ).toBeInTheDocument()
  })

  it('shows a safe not-found message and a retry action', async () => {
    open(apiError(404, 'not_found', 'resource not found'))
    expect(await screen.findByRole('heading', { name: 'Delivery not found' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Try again' })).toBeInTheDocument()
  })

  it('aborts the previous organization request and never shows its delivery after an organization switch', async () => {
    let release: ((response: Response) => void) | undefined
    const calls = stubFetch((call) => {
      if (call.path === '/api/v1/auth/me') return json(200, meBody([org('o1'), org('o2')]))
      if (call.path === path('o1'))
        return new Promise<Response>((resolve) => {
          release = resolve
        })
      return apiError(404, 'not_found', 'resource not found')
    })
    fakeChannel()
    const { user } = renderApp(`/o/o1/operations/deliveries/${ID}`)
    await screen.findByText('Loading delivery detail…')
    // The organization selector navigates to the home of the chosen organization, not to the same delivery in it.
    await user.selectOptions(screen.getByLabelText('Organization'), 'o2')
    await waitFor(() => expect(screen.getByTestId('location')).toHaveTextContent('/o/o2'))
    expect(screen.queryByText('Loading delivery detail…')).not.toBeInTheDocument()
    expect(calls.find((call) => call.path === path('o1'))?.signal?.aborted).toBe(true)
    expect(calls.some((call) => call.path === path('o2'))).toBe(false)
    // The old request resolves late: its delivery must still never appear.
    await act(async () => release?.(json(200, { delivery, attempts })))
    expect(screen.queryByText('Orders webhook')).not.toBeInTheDocument()
    expect(screen.queryByRole('heading', { name: 'FAILED', level: 1 })).not.toBeInTheDocument()
  })
})
