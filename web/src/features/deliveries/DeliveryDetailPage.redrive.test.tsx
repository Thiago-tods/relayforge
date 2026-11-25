import { act, fireEvent, renderHook, screen, waitFor, within } from '@testing-library/react'
import type { UserEvent } from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import { setCsrfToken } from '../../api/client'
import type { OrganizationWithRole } from '../../api/types'
import { apiError, json, meBody, org, stubFetch } from '../../test/fetchStub'
import type { Handler, Recorded } from '../../test/fetchStub'
import { fakeChannel, renderApp } from '../../test/renderApp'
import { useRedrive } from './useRedrive'

// Everything here runs against a stubbed fetch: it proves the UI's behavior for the answers it is given, not the
// behavior of the real backend.

vi.mock('../../auth/crossTab', () => ({ openAuthChannel: vi.fn() }))

const ID = '11111111-1111-4111-8111-111111111111'
const path = (orgId: string) => `/api/v1/organizations/${orgId}/deliveries/${ID}`
const failed = {
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
const detailBody = (over: Record<string, unknown> = {}) => ({
  delivery: { ...failed, ...over },
  attempts: [],
})
const okBody = () => ({
  delivery: {
    ...failed,
    status: 'PENDING',
    maxAttempts: 13,
    failureReason: undefined,
    completedAt: undefined,
    nextAttemptAt: '2026-11-25T10:00:00Z',
  },
  redrive: { maxAttempts: 13, previousFailureReason: 'RETRIES_EXHAUSTED', previousMaxAttempts: 8 },
})

type Answer = () => Response | Promise<Response>

interface Setup {
  role?: OrganizationWithRole['role']
  /** The first detail GET. */
  detail?: Record<string, unknown>
  /** The answers to the detail GETs after the first one, in order. When they run out the delivery stays FAILED. */
  after?: Answer[]
  post?: Handler
}

function mount(setup: Setup = {}) {
  fakeChannel()
  const later = [...(setup.after ?? [])]
  let first = true
  const calls = stubFetch((call) => {
    if (call.path === '/api/v1/auth/me') {
      return json(200, meBody([org('o1', setup.role), org('o2', setup.role)]))
    }
    if (call.path === '/api/v1/auth/csrf') return json(200, { session: { csrfToken: 'tok' } })
    if (call.path === path('o1') && call.method === 'GET') {
      if (first) {
        first = false
        return json(200, detailBody(setup.detail))
      }
      return (later.shift() ?? (() => json(200, detailBody())))()
    }
    if (call.path === `${path('o1')}/redrive` && call.method === 'POST') {
      return (setup.post ?? (() => json(200, okBody())))(call)
    }
    return apiError(404, 'not_found', 'resource not found')
  })
  return { calls, ...renderApp(`/o/o1/operations/deliveries/${ID}`) }
}

const posts = (calls: Recorded[]) =>
  calls.filter((call) => call.method === 'POST' && call.path.endsWith('/redrive'))
const detailGets = (calls: Recorded[]) =>
  calls.filter((call) => call.method === 'GET' && call.path === path('o1'))

async function openDialog(user: UserEvent) {
  const trigger = await screen.findByRole('button', { name: 'Redrive…' })
  // A token is held, as it is after an earlier mutation, so the identity round trip is not part of this test.
  setCsrfToken('tok')
  await user.click(trigger)
  return screen.getByRole('dialog')
}

async function confirm(user: UserEvent, reason = 'Receiver fixed') {
  const dialog = screen.getByRole('dialog')
  if (reason !== '') await user.type(within(dialog).getByLabelText('Reason'), reason)
  await user.click(within(dialog).getByRole('button', { name: 'Redrive delivery' }))
}

const live = () => screen.getByTestId('redrive-live')
const networkLost: Handler = () => {
  throw new TypeError('Failed to fetch')
}

describe('redrive: who sees the action and when', () => {
  it.each(['OWNER', 'ADMIN', 'DEVELOPER'] as const)(
    '%s sees Redrive on a FAILED delivery',
    async (role) => {
      mount({ role })
      expect(await screen.findByRole('button', { name: 'Redrive…' })).toBeInTheDocument()
    },
  )

  it('shows a VIEWER nothing: no action, no dialog, no live region, and no request is ever sent', async () => {
    const { calls, user } = mount({ role: 'VIEWER' })
    await screen.findByRole('heading', { name: 'FAILED', level: 1 })
    expect(screen.queryByRole('button', { name: /redrive/i })).not.toBeInTheDocument()
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    expect(screen.queryByTestId('redrive-live')).not.toBeInTheDocument()
    await user.tab()
    await user.tab()
    await user.keyboard('{Enter}{Escape}')
    expect(posts(calls)).toHaveLength(0)
    expect(calls.some((call) => call.method === 'POST')).toBe(false)
  })

  it('cannot be invoked without permission: the hook ignores open and submit', () => {
    const calls = stubFetch(() => json(200, okBody()))
    const { result } = renderHook(() =>
      useRedrive({ orgId: 'o1', deliveryId: ID, allowed: false, onFresh: () => undefined }),
    )
    act(() => result.current.open({ ...failed, nextAttemptAt: null } as never))
    expect(result.current.flow.phase).toBe('idle')
    act(() => result.current.submit('reason'))
    expect(result.current.flow.phase).toBe('idle')
    expect(calls).toHaveLength(0)
  })

  it.each(['PENDING', 'IN_FLIGHT', 'SUCCEEDED', 'CANCELLED'])(
    'offers no redrive for a %s delivery',
    async (status) => {
      mount({ detail: { status } })
      await screen.findByRole('heading', { name: status, level: 1 })
      expect(screen.queryByRole('button', { name: /redrive/i })).not.toBeInTheDocument()
    },
  )

  it('explains instead of offering the action when the budget is already at the maximum', async () => {
    mount({ detail: { attemptCount: 50, maxAttempts: 50 } })
    expect(
      await screen.findByText(/already has the maximum retry budget \(50 attempts\)/),
    ).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /redrive/i })).not.toBeInTheDocument()
  })

  it('hides the action once the server answers 403 forbidden', async () => {
    const { user, calls } = mount({ post: () => apiError(403, 'forbidden', 'forbidden') })
    await openDialog(user)
    await confirm(user)
    expect(await screen.findByRole('heading', { name: 'Not permitted' })).toBeInTheDocument()
    expect(screen.getByRole('alert')).toHaveTextContent('no longer have permission')
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /redrive/i })).not.toBeInTheDocument()
    expect(posts(calls)).toHaveLength(1)
  })
})

describe('redrive dialog', () => {
  it('is a modal dialog with an accessible name and description, and focus starts in the reason field', async () => {
    const showModal = vi.spyOn(HTMLDialogElement.prototype, 'showModal')
    const { user } = mount()
    const dialog = await openDialog(user)
    expect(showModal).toHaveBeenCalledTimes(1)
    // The live region is mounted before any text is announced.
    expect(live()).toBeEmptyDOMElement()
    expect(dialog).toHaveAccessibleName('Redrive this delivery?')
    expect(dialog).toHaveAccessibleDescription(/normal delivery pipeline/)
    expect(within(dialog).getByLabelText('Reason')).toHaveFocus()
  })

  it('shows the failure context, an advisory budget and the warnings', async () => {
    const { user } = mount()
    const dialog = await openDialog(user)
    expect(within(dialog).getByText('Orders webhook')).toBeInTheDocument()
    expect(within(dialog).getByText('RETRIES_EXHAUSTED')).toBeInTheDocument()
    expect(within(dialog).getByText('503')).toBeInTheDocument()
    expect(within(dialog).getByText('server_error')).toBeInTheDocument()
    expect(within(dialog).getByText('8 → 13 (expected)')).toBeInTheDocument()
    expect(dialog).toHaveTextContent(/advisory/i)
    expect(dialog).toHaveTextContent(/at-least-once/)
    expect(dialog).toHaveTextContent(/same delivery ID again/)
    expect(dialog).toHaveTextContent(/paused, the delivery may stay PENDING/)
    expect(dialog).toHaveTextContent(/revoked, the server rejects/)
    expect(dialog).toHaveTextContent(/audit log/)
    expect(dialog).toHaveTextContent(/secrets or personal data/)
    expect(dialog).toHaveTextContent(/does not guarantee/)
  })

  it('caps the advisory budget at 50 and explains a legacy delivery with no recorded reason', async () => {
    const { user } = mount({ detail: { maxAttempts: 48, failureReason: null } })
    const dialog = await openDialog(user)
    expect(within(dialog).getByText('48 → 50 (expected)')).toBeInTheDocument()
    expect(dialog).toHaveTextContent(
      /Not recorded \(this delivery failed before reasons were recorded\)/,
    )
  })

  it('has a keyboard order of reason, Cancel, Redrive delivery', async () => {
    const { user } = mount()
    const dialog = await openDialog(user)
    expect(within(dialog).getByLabelText('Reason')).toHaveFocus()
    await user.tab()
    expect(within(dialog).getByRole('button', { name: 'Cancel' })).toHaveFocus()
    await user.tab()
    expect(within(dialog).getByRole('button', { name: 'Redrive delivery' })).toHaveFocus()
  })

  it('returns focus to the Redrive button after Cancel and after Escape, without sending anything', async () => {
    const { user, calls } = mount()
    await openDialog(user)
    await user.click(screen.getByRole('button', { name: 'Cancel' }))
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Redrive…' })).toHaveFocus()

    await user.click(screen.getByRole('button', { name: 'Redrive…' }))
    expect(screen.getByRole('dialog')).toBeInTheDocument()
    await user.keyboard('{Escape}')
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Redrive…' })).toHaveFocus()
    expect(posts(calls)).toHaveLength(0)
  })

  it('also closes on the native cancel event', async () => {
    const { user } = mount()
    const dialog = await openDialog(user)
    fireEvent(dialog, new Event('cancel', { cancelable: true }))
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
  })

  it('counts code points as you type and never changes pasted text', async () => {
    const { user } = mount()
    const dialog = await openDialog(user)
    const reason = within(dialog).getByLabelText('Reason')
    await user.type(reason, 'ab😀')
    expect(dialog).toHaveTextContent('3 / 500 characters')
    fireEvent.change(reason, { target: { value: 'line one\tline two' } })
    expect(reason).toHaveValue('line one\tline two')
    expect(reason).toHaveAccessibleDescription(/control character/)
    expect(reason).toBeInvalid()
  })

  it('warns about a reason over 500 code points but does not disable Confirm', async () => {
    const { user } = mount()
    const dialog = await openDialog(user)
    fireEvent.change(within(dialog).getByLabelText('Reason'), {
      target: { value: '😀'.repeat(501) },
    })
    expect(within(dialog).getByLabelText('Reason')).toHaveAccessibleDescription(/longer than 500/)
    expect(within(dialog).getByRole('button', { name: 'Redrive delivery' })).toBeEnabled()
  })

  it('does not block an empty reason on the client: the server decides and its message is shown inline', async () => {
    const { user, calls } = mount({
      post: () =>
        apiError(400, 'validation_failed', 'reason must be 1 to 500 printable characters'),
    })
    const dialog = await openDialog(user)
    expect(within(dialog).getByRole('button', { name: 'Redrive delivery' })).toBeEnabled()
    await user.click(within(dialog).getByRole('button', { name: 'Redrive delivery' }))
    expect(
      await within(dialog).findByText('reason must be 1 to 500 printable characters'),
    ).toBeInTheDocument()
    expect(screen.getByRole('dialog')).toBeInTheDocument()
    expect(within(dialog).getByLabelText('Reason')).toHaveFocus()
    expect(within(dialog).getByLabelText('Reason')).toBeInvalid()
    expect(posts(calls)).toHaveLength(1)
    expect(posts(calls)[0]?.body).toEqual({ reason: '', expectedAttemptCount: 3 })
  })

  it('shows the server message when it rejects a reason the client accepted', async () => {
    const { user } = mount({
      post: () =>
        apiError(400, 'validation_failed', 'reason must be 1 to 500 printable characters'),
    })
    await openDialog(user)
    await confirm(user, 'looks fine here')
    expect(
      await screen.findByText('reason must be 1 to 500 printable characters'),
    ).toBeInTheDocument()
    expect(screen.getByRole('dialog')).toBeInTheDocument()
    expect(screen.getByLabelText('Reason')).toHaveValue('looks fine here')
  })
})

describe('redrive: success', () => {
  it('sends one POST, then reads the delivery once, and announces the server budget', async () => {
    const { user, calls } = mount({
      after: [() => json(200, detailBody({ status: 'PENDING', maxAttempts: 13 }))],
    })
    await openDialog(user)
    await confirm(user, '  Receiver fixed  ')
    const heading = await screen.findByRole('heading', { name: 'Redrive accepted' })
    expect(posts(calls)).toHaveLength(1)
    expect(posts(calls)[0]?.path).toBe(`${path('o1')}/redrive`)
    expect(posts(calls)[0]?.body).toEqual({ reason: '  Receiver fixed  ', expectedAttemptCount: 3 })
    expect(posts(calls)[0]?.headers.get('X-CSRF-Token')).toBe('tok')
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    expect(heading).toHaveFocus()
    expect(heading.parentElement).toHaveTextContent('changed from 8 to 13')
    expect(heading.parentElement).toHaveTextContent('does not guarantee')
    expect(heading.parentElement).toHaveTextContent('asynchronously')
    expect(live()).toHaveTextContent(/Redrive accepted\. .* from 8 to 13/)
    await waitFor(() =>
      expect(screen.getByRole('heading', { name: 'PENDING', level: 1 })).toBeInTheDocument(),
    )
    expect(detailGets(calls)).toHaveLength(2)
    expect(screen.queryByRole('button', { name: /redrive/i })).not.toBeInTheDocument()
  })

  it('keeps the success banner and says so when the follow-up read fails', async () => {
    const { user, calls } = mount({ after: [() => apiError(500, 'internal_error', 'boom')] })
    await openDialog(user)
    await confirm(user)
    expect(await screen.findByRole('heading', { name: 'Redrive accepted' })).toBeInTheDocument()
    expect(await screen.findByText(/could not be refreshed/)).toBeInTheDocument()
    expect(screen.getByRole('heading', { name: 'Redrive accepted' })).toBeInTheDocument()
    // The detail on screen is out of date, so a second redrive is not offered.
    expect(screen.queryByRole('button', { name: 'Redrive…' })).not.toBeInTheDocument()
    expect(posts(calls)).toHaveLength(1)
    expect(detailGets(calls)).toHaveLength(2)
  })

  it('disables Confirm and shows a status while the request runs', async () => {
    let release: ((response: Response) => void) | undefined
    const { user } = mount({
      post: () =>
        new Promise<Response>((resolve) => {
          release = resolve
        }),
    })
    const dialog = await openDialog(user)
    await confirm(user)
    expect(within(dialog).getByRole('button', { name: 'Redrive delivery' })).toBeDisabled()
    expect(within(dialog).getByRole('button', { name: 'Cancel' })).toBeDisabled()
    expect(within(dialog).getByText('Redriving delivery…')).toHaveFocus()
    await user.keyboard('{Escape}')
    expect(screen.getByRole('dialog')).toBeInTheDocument()
    await act(async () => release?.(json(200, okBody())))
    expect(await screen.findByRole('heading', { name: 'Redrive accepted' })).toBeInTheDocument()
  })

  it('sends one POST for a double submit in the same tick', async () => {
    const { user, calls } = mount()
    const dialog = await openDialog(user)
    await user.type(within(dialog).getByLabelText('Reason'), 'Receiver fixed')
    const form = dialog.querySelector('form') as HTMLFormElement
    act(() => {
      fireEvent.submit(form)
      fireEvent.submit(form)
    })
    await screen.findByRole('heading', { name: 'Redrive accepted' })
    expect(posts(calls)).toHaveLength(1)
  })

  it('sends one POST for a double click and for Enter plus click', async () => {
    let release: ((response: Response) => void) | undefined
    const { user, calls } = mount({
      post: () =>
        new Promise<Response>((resolve) => {
          release = resolve
        }),
    })
    const dialog = await openDialog(user)
    await user.type(within(dialog).getByLabelText('Reason'), 'Receiver fixed{Enter}')
    await user.dblClick(within(dialog).getByRole('button', { name: 'Redrive delivery' }))
    expect(posts(calls)).toHaveLength(1)
    await act(async () => release?.(json(200, okBody())))
    await screen.findByRole('heading', { name: 'Redrive accepted' })
    expect(posts(calls)).toHaveLength(1)
  })

  it('sends the attempt count of the snapshot taken when the dialog opened, even if the page refreshes meanwhile', async () => {
    const { user, calls } = mount({ after: [() => json(200, detailBody({ attemptCount: 4 }))] })
    await openDialog(user)
    await user.click(screen.getByRole('button', { name: 'Refresh' }))
    await screen.findByText('4 / 8 attempts')
    await confirm(user)
    await screen.findByRole('heading', { name: 'Redrive accepted' })
    expect(posts(calls)[0]?.body).toMatchObject({ expectedAttemptCount: 3 })
  })
})

describe('redrive: known rejections', () => {
  const closing: [string, number, string, string, boolean][] = [
    ['delivery_not_failed', 409, 'The delivery is no longer failed', 'alert-free', true],
    ['delivery_changed', 409, 'The delivery changed', 'alert-free', true],
    ['destination_revoked', 409, 'The destination is revoked', 'alert', false],
    ['retry_budget_exhausted', 409, 'Retry budget exhausted', 'alert', true],
    ['event_payload_unavailable', 409, 'Event payload unavailable', 'alert', false],
    ['not_found', 404, 'Delivery not found', 'alert', false],
  ]
  it.each(closing)(
    '%s closes the dialog, shows a banner, repeats nothing and reads the delivery only when it helps',
    async (code, status, title, kind, refreshes) => {
      const { user, calls } = mount({ post: () => apiError(status, code, 'server words') })
      await openDialog(user)
      await confirm(user)
      const heading = await screen.findByRole('heading', { name: title })
      expect(heading).toHaveFocus()
      expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
      if (kind === 'alert') expect(screen.getByRole('alert')).toHaveTextContent(title)
      await waitFor(() => expect(detailGets(calls)).toHaveLength(refreshes ? 2 : 1))
      expect(posts(calls)).toHaveLength(1)
      expect(heading.parentElement).toHaveTextContent(/nothing was changed|changed nothing/i)
    },
  )

  it('after a stale count the next deliberate redrive uses the new attempt count and an empty reason', async () => {
    let answers = 0
    const { user, calls } = mount({
      post: () => {
        answers += 1
        return answers === 1 ? apiError(409, 'delivery_changed', 'changed') : json(200, okBody())
      },
      after: [() => json(200, detailBody({ attemptCount: 5 }))],
    })
    await openDialog(user)
    await confirm(user, 'first reason')
    await screen.findByRole('heading', { name: 'The delivery changed' })
    await screen.findByText('5 / 8 attempts')
    await user.click(screen.getByRole('button', { name: 'Redrive…' }))
    expect(screen.getByLabelText('Reason')).toHaveValue('')
    await confirm(user, 'second reason')
    await screen.findByRole('heading', { name: 'Redrive accepted' })
    expect(posts(calls).map((call) => call.body)).toEqual([
      { reason: 'first reason', expectedAttemptCount: 3 },
      { reason: 'second reason', expectedAttemptCount: 5 },
    ])
  })

  it('keeps the dialog open for lock_timeout, says nothing changed, and sends again only on a new confirmation', async () => {
    let answers = 0
    const { user, calls } = mount({
      post: () => {
        answers += 1
        return answers === 1
          ? apiError(503, 'lock_timeout', 'try later', { 'Retry-After': '1' })
          : json(200, okBody())
      },
    })
    await openDialog(user)
    await confirm(user)
    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent(/nothing was changed|changed nothing/i)
    expect(alert).toHaveFocus()
    expect(screen.getByRole('dialog')).toBeInTheDocument()
    expect(screen.getByLabelText('Reason')).toHaveValue('Receiver fixed')
    expect(posts(calls)).toHaveLength(1)
    expect(detailGets(calls)).toHaveLength(1)
    await user.click(screen.getByRole('button', { name: 'Redrive delivery' }))
    await screen.findByRole('heading', { name: 'Redrive accepted' })
    expect(posts(calls)).toHaveLength(2)
  })

  it('shows a reload message after csrf_invalid and repeats only the client token retry', async () => {
    const { user, calls } = mount({ post: () => apiError(403, 'csrf_invalid', 'bad token') })
    await openDialog(user)
    await confirm(user)
    expect(await screen.findByRole('alert')).toHaveTextContent('Reload the page')
    expect(screen.getByRole('dialog')).toBeInTheDocument()
    // The client sends the request a second time with a rotated token, because the backend rejects a bad token
    // before the handler runs. The UI adds no repeat of its own.
    expect(posts(calls)).toHaveLength(2)
    expect(detailGets(calls)).toHaveLength(1)
  })
})

describe('redrive: ambiguous outcomes', () => {
  const causes: [string, Handler][] = [
    ['a lost connection', networkLost],
    [
      'a body that fails while it is read',
      () =>
        new Response(
          new ReadableStream({
            start: (controller) => controller.error(new TypeError('network error')),
          }),
          { status: 200 },
        ),
    ],
    ['a 502 from a proxy', () => apiError(502, 'bad_gateway', 'bad gateway')],
    ['a generic 500', () => apiError(500, 'internal_error', 'boom')],
    ['a 200 that is not JSON', () => new Response('<html>', { status: 200 })],
    ['a 200 with a missing key', () => json(200, { delivery: okBody().delivery })],
    ['a 200 with an extra key', () => json(200, { ...okBody(), extra: 1 })],
    [
      'a 200 for another delivery',
      () => json(200, { ...okBody(), delivery: { ...okBody().delivery, id: 'x' } }),
    ],
  ]
  it.each(causes)(
    'treats %s as ambiguous: one POST, one verification GET, no claim of success',
    async (_name, post) => {
      const { user, calls } = mount({ post })
      await openDialog(user)
      await confirm(user)
      const heading = await screen.findByRole('heading', {
        name: 'The redrive does not appear to have been applied',
      })
      expect(heading).toHaveFocus()
      expect(posts(calls)).toHaveLength(1)
      expect(detailGets(calls)).toHaveLength(2)
      expect(screen.queryByRole('heading', { name: 'Redrive accepted' })).not.toBeInTheDocument()
      // Still FAILED with the same count and budget: a new, deliberate redrive is possible.
      expect(screen.getByRole('button', { name: 'Redrive…' })).toBeInTheDocument()
    },
  )

  const inconsistentBudgets: [string, number, number][] = [
    ['a previous budget that differs from the dialog snapshot', 13, 18],
    ['an increase larger than five', 8, 14],
    ['an increase smaller than five', 8, 12],
    ['a budget above 50', 48, 53],
  ]
  it.each(inconsistentBudgets)(
    'treats a 200 with %s as ambiguous: one POST, one verification GET, no success',
    async (_name, previous, max) => {
      const { user, calls } = mount({
        post: () =>
          json(200, {
            delivery: { ...okBody().delivery, maxAttempts: max },
            redrive: {
              maxAttempts: max,
              previousFailureReason: null,
              previousMaxAttempts: previous,
            },
          }),
        after: [() => json(200, detailBody({ status: 'PENDING', maxAttempts: 13 }))],
      })
      await openDialog(user)
      await confirm(user)
      const heading = await screen.findByRole('heading', {
        name: 'The delivery appears to have been redriven',
      })
      expect(heading.parentElement).toHaveTextContent(
        /cannot show whether this request or another one caused it/,
      )
      expect(screen.queryByRole('heading', { name: 'Redrive accepted' })).not.toBeInTheDocument()
      expect(posts(calls)).toHaveLength(1)
      expect(detailGets(calls)).toHaveLength(2)
    },
  )

  it('keeps State unverified and Check again when the check after an inconsistent 200 fails', async () => {
    const { user, calls } = mount({
      post: () =>
        json(200, {
          delivery: { ...okBody().delivery, maxAttempts: 14 },
          redrive: { maxAttempts: 14, previousFailureReason: null, previousMaxAttempts: 8 },
        }),
      after: [() => apiError(500, 'internal_error', 'boom')],
    })
    await openDialog(user)
    await confirm(user)
    await screen.findByRole('heading', { name: 'State unverified' })
    expect(screen.getByRole('button', { name: 'Check again' })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Redrive…' })).not.toBeInTheDocument()
    expect(posts(calls)).toHaveLength(1)
    expect(detailGets(calls)).toHaveLength(2)
  })

  it('says the outcome is unknown and hides Redrive while the one check runs', async () => {
    let release: ((response: Response) => void) | undefined
    const { user, calls } = mount({
      post: networkLost,
      after: [
        () =>
          new Promise<Response>((resolve) => {
            release = resolve
          }),
      ],
    })
    await openDialog(user)
    await confirm(user)
    const heading = await screen.findByRole('heading', { name: 'Checking the delivery state' })
    expect(heading.parentElement).toHaveTextContent('may or may not have been applied')
    expect(heading.parentElement).toHaveTextContent('connection was lost')
    expect(live()).toHaveTextContent('Checking the delivery state')
    expect(screen.queryByRole('button', { name: 'Redrive…' })).not.toBeInTheDocument()
    expect(heading).toHaveFocus()
    await act(async () => release?.(json(200, detailBody())))
    await screen.findByRole('heading', { name: 'The redrive does not appear to have been applied' })
    expect(posts(calls)).toHaveLength(1)
    expect(detailGets(calls)).toHaveLength(2)
  })

  const comparisons: [string, Record<string, unknown>, string, RegExp][] = [
    [
      'still FAILED with the same count and budget',
      {},
      'The redrive does not appear to have been applied',
      /still FAILED with the same attempt count/,
    ],
    [
      'PENDING with the budget raised',
      { status: 'PENDING', maxAttempts: 13 },
      'The delivery appears to have been redriven',
      /cannot show whether this request or another one caused it/,
    ],
    [
      'IN_FLIGHT with the budget raised',
      { status: 'IN_FLIGHT', maxAttempts: 13 },
      'The delivery appears to have been redriven',
      /8 to 13/,
    ],
    [
      'FAILED with a higher attempt count',
      { attemptCount: 6, maxAttempts: 13 },
      'The delivery was processed again and failed again',
      /6 attempts \(it showed 3\)/,
    ],
    [
      'SUCCEEDED',
      { status: 'SUCCEEDED' },
      'The delivery is now SUCCEEDED',
      /does not show whether a redrive caused/,
    ],
    [
      'CANCELLED',
      { status: 'CANCELLED' },
      'The delivery is now CANCELLED',
      /does not show whether a redrive caused/,
    ],
  ]
  it.each(comparisons)(
    'compares the check with the snapshot: %s',
    async (_name, latest, title, text) => {
      const { user } = mount({ post: networkLost, after: [() => json(200, detailBody(latest))] })
      await openDialog(user)
      await confirm(user)
      const heading = await screen.findByRole('heading', { name: title })
      expect(heading.parentElement).toHaveTextContent(text)
      expect(heading.parentElement).not.toHaveTextContent(/your redrive (was|succeeded)/i)
    },
  )

  it('shows State unverified when the check fails, keeps Redrive hidden, and rechecks with a GET only', async () => {
    const { user, calls } = mount({
      post: networkLost,
      after: [
        () => apiError(500, 'internal_error', 'boom'),
        () => apiError(500, 'internal_error', 'boom'),
        () => json(200, detailBody({ status: 'PENDING', maxAttempts: 13 })),
      ],
    })
    await openDialog(user)
    await confirm(user)
    const heading = await screen.findByRole('heading', { name: 'State unverified' })
    expect(heading).toHaveFocus()
    expect(screen.getByRole('alert')).toHaveTextContent('not known')
    expect(screen.queryByRole('button', { name: 'Redrive…' })).not.toBeInTheDocument()
    expect(detailGets(calls)).toHaveLength(2)

    await user.click(screen.getByRole('button', { name: 'Check again' }))
    await screen.findByRole('heading', { name: 'State unverified' })
    expect(detailGets(calls)).toHaveLength(3)
    expect(screen.queryByRole('button', { name: 'Redrive…' })).not.toBeInTheDocument()

    await user.click(screen.getByRole('button', { name: 'Check again' }))
    await screen.findByRole('heading', { name: 'The delivery appears to have been redriven' })
    expect(detailGets(calls)).toHaveLength(4)
    expect(posts(calls)).toHaveLength(1)
  })

  it('treats a 30 second wait as ambiguous: one POST, one verification GET', async () => {
    const { user, calls } = mount({
      post: (call) =>
        new Promise<Response>((_resolve, reject) => {
          call.signal?.addEventListener('abort', () =>
            reject(new DOMException('The operation was aborted.', 'AbortError')),
          )
        }),
    })
    const dialog = await openDialog(user)
    await user.type(within(dialog).getByLabelText('Reason'), 'Receiver fixed')
    vi.useFakeTimers()
    fireEvent.click(within(dialog).getByRole('button', { name: 'Redrive delivery' }))
    await act(async () => {
      await vi.advanceTimersByTimeAsync(29_999)
    })
    expect(posts(calls)).toHaveLength(1)
    expect(screen.getByText('Redriving delivery…')).toBeInTheDocument()
    expect(detailGets(calls)).toHaveLength(1)
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1)
    })
    vi.useRealTimers()
    const heading = await screen.findByRole('heading', {
      name: 'The redrive does not appear to have been applied',
    })
    expect(heading).toBeInTheDocument()
    expect(posts(calls)).toHaveLength(1)
    expect(detailGets(calls)).toHaveLength(2)
    expect(posts(calls)[0]?.signal?.aborted).toBe(true)
  })
})

describe('redrive: leaving the page', () => {
  it('stops without a verification GET when the organization changes during the request', async () => {
    const { user, calls } = mount({
      post: (call) =>
        new Promise<Response>((_resolve, reject) => {
          call.signal?.addEventListener('abort', () =>
            reject(new DOMException('The operation was aborted.', 'AbortError')),
          )
        }),
    })
    await openDialog(user)
    await confirm(user)
    await screen.findByText('Redriving delivery…')
    await user.selectOptions(screen.getByLabelText('Organization'), 'o2')
    await waitFor(() => expect(screen.getByTestId('location')).toHaveTextContent('/o/o2'))
    expect(posts(calls)[0]?.signal?.aborted).toBe(true)
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 20))
    })
    expect(detailGets(calls)).toHaveLength(1)
    expect(posts(calls)).toHaveLength(1)
    expect(screen.queryByText(/Checking the delivery state/)).not.toBeInTheDocument()
    expect(screen.queryByRole('heading', { name: 'Redrive accepted' })).not.toBeInTheDocument()
  })

  it('ignores a late answer after the organization changed: no banner, no follow-up GET', async () => {
    let release: ((response: Response) => void) | undefined
    const { user, calls } = mount({
      post: () =>
        new Promise<Response>((resolve) => {
          release = resolve
        }),
    })
    await openDialog(user)
    await confirm(user)
    await screen.findByText('Redriving delivery…')
    await user.selectOptions(screen.getByLabelText('Organization'), 'o2')
    await waitFor(() => expect(screen.getByTestId('location')).toHaveTextContent('/o/o2'))
    await act(async () => release?.(json(200, okBody())))
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 20))
    })
    expect(screen.queryByRole('heading', { name: 'Redrive accepted' })).not.toBeInTheDocument()
    expect(screen.queryByText('Orders webhook')).not.toBeInTheDocument()
    expect(detailGets(calls)).toHaveLength(1)
    expect(posts(calls)).toHaveLength(1)
  })

  it('starts no verification GET when the page is left during the request', async () => {
    const { user, calls } = mount({ post: () => new Promise<Response>(() => undefined) })
    await openDialog(user)
    await confirm(user)
    await screen.findByText('Redriving delivery…')
    await user.click(screen.getByRole('link', { name: '← Back to Activity' }))
    await waitFor(() =>
      expect(screen.getByTestId('location')).toHaveTextContent('/o/o1/operations/activity'),
    )
    expect(posts(calls)[0]?.signal?.aborted).toBe(true)
    expect(detailGets(calls)).toHaveLength(1)
  })
})
