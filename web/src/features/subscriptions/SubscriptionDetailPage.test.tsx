import { act, fireEvent, screen, waitFor, within } from '@testing-library/react'
import type { UserEvent } from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import { setCsrfToken } from '../../api/client'
import { destinationBody } from '../../test/destinationFixtures'
import { apiError, json } from '../../test/fetchStub'
import type { Handler } from '../../test/fetchStub'
import {
  DEST_ID,
  endpointBody,
  EP_ID,
  revokedSubscriptionBody,
  SUB_ID,
  SUB_ID_2,
  subscriptionBody,
  subscriptionList,
} from '../../test/subscriptionFixtures'
import { mountSubscriptions, requests } from '../../test/subscriptionHarness'
import type { HarnessSetup } from '../../test/subscriptionHarness'

vi.mock('../../auth/crossTab', () => ({ openAuthChannel: vi.fn() }))

// Stubbed fetch only: these tests prove the UI's behavior for the answers it is given, not the real backend.

const ORG = '/api/v1/organizations/o1'
const SUBS = `${ORG}/subscriptions`
const ONE = `${SUBS}/${SUB_ID}`
const EP = `${ORG}/ingestion-endpoints/${EP_ID}`
const DEST = `${ORG}/destinations/${DEST_ID}`
const PAGE = `/o/o1/subscriptions/${SUB_ID}`

const get =
  (over: Record<string, unknown> = {}): Handler =>
  () =>
    json(200, { subscription: subscriptionBody(over) })

function mount(setup: HarnessSetup & { sub?: Record<string, unknown> } = {}) {
  const { sub, ...rest } = setup
  return mountSubscriptions(PAGE, {
    ...rest,
    routes: {
      [`GET ${ONE}`]: get(sub),
      [`GET ${EP}`]: () => json(200, { endpoint: endpointBody() }),
      [`GET ${DEST}`]: () => json(200, { destination: destinationBody() }),
      [`GET ${SUBS}`]: () =>
        json(
          200,
          subscriptionList([
            subscriptionBody(),
            revokedSubscriptionBody({ id: SUB_ID_2, createdAt: '2026-11-10T10:00:00Z' }),
          ]),
        ),
      ...rest.routes,
    },
  })
}

async function ready(user: UserEvent, name: string) {
  const button = await screen.findByRole('button', { name })
  setCsrfToken('tok')
  await user.click(button)
  return screen.getByRole('dialog')
}

const revoke = async (user: UserEvent) => {
  const dialog = await ready(user, 'Revoke subscription')
  await user.click(within(dialog).getByRole('checkbox'))
  return dialog
}
const confirm = (dialog: HTMLElement) =>
  within(dialog).getByRole('button', { name: 'Revoke subscription' })

describe('subscription detail', () => {
  it('shows the relationship, the separate statuses and the interval', async () => {
    const { calls } = mount()
    expect(
      await screen.findByRole('heading', { name: 'Orders intake → Billing hook', level: 1 }),
    ).toBeInTheDocument()
    const overview = screen.getByRole('region', { name: 'Overview' })
    expect(overview.querySelector('[data-kind="subscription"]')).toHaveTextContent('Active')
    expect(within(overview).getByText('Not ended')).toBeInTheDocument()
    const endpoint = screen.getByRole('region', { name: 'Ingestion endpoint' })
    expect(within(endpoint).getByText('Orders intake')).toBeInTheDocument()
    expect(endpoint.querySelector('[data-kind="endpoint"]')).toHaveTextContent('Active')
    const destination = screen.getByRole('region', { name: 'Destination' })
    expect(within(destination).getByRole('link', { name: 'Billing hook' })).toHaveAttribute(
      'href',
      `/o/o1/destinations/${DEST_ID}`,
    )
    expect(destination.querySelector('[data-kind="destination"]')).toHaveTextContent('Active')
    // The creator is never shown.
    expect(document.body.textContent).not.toContain('u-1')
    expect(requests(calls, 'GET', EP)).toHaveLength(1)
    expect(requests(calls, 'GET', DEST)).toHaveLength(1)
  })

  it('lists the intervals of the pair, marks this one and links to the others', async () => {
    const { calls } = mount()
    const table = await screen.findByRole('table')
    const rows = within(table).getAllByRole('row').slice(1)
    expect(rows).toHaveLength(2)
    expect(within(rows[0]!).getByText('This subscription')).toBeInTheDocument()
    expect(rows[0]).toHaveAttribute('aria-current', 'true')
    expect(
      within(rows[1]!).getByRole('link', {
        name: 'Open the subscription that started 2026-11-10T10:00:00Z',
      }),
    ).toHaveAttribute('href', `/o/o1/subscriptions/${SUB_ID_2}`)
    expect(within(rows[1]!).getByText('Revoked')).toBeInTheDocument()
    const query = new URL(requests(calls, 'GET', SUBS)[0]!.path, 'http://x').searchParams
    expect(Object.fromEntries(query)).toEqual({
      limit: '100',
      offset: '0',
      endpointId: EP_ID,
      destinationId: DEST_ID,
    })
  })

  it.each([
    ['REVOKED', /never sent to.*does not revoke its subscriptions/],
    ['PAUSED', /deliveries stay pending/],
    ['ACTIVE', /separate statuses/],
  ])(
    'explains a %s destination without merging it into the subscription status',
    async (status, note) => {
      mount({
        sub: { destinationStatus: status },
        routes: {
          [`GET ${DEST}`]: () =>
            json(200, {
              destination: destinationBody({
                status,
                ...(status === 'REVOKED' ? { revokedAt: '2026-11-22T09:00:00Z' } : {}),
              }),
            }),
        },
      })
      expect(await screen.findByTestId('destination-status-note')).toHaveTextContent(note)
      const overview = screen.getByRole('region', { name: 'Overview' })
      expect(overview.querySelector('[data-kind="subscription"]')).toHaveTextContent('Active')
    },
  )

  it('prefers the destination read over the snapshot that came with the subscription', async () => {
    mount({
      routes: {
        [`GET ${DEST}`]: () =>
          json(200, { destination: destinationBody({ status: 'PAUSED', name: 'Renamed hook' }) }),
      },
    })
    const destination = await screen.findByRole('region', { name: 'Destination' })
    await waitFor(() =>
      expect(destination.querySelector('[data-kind="destination"]')).toHaveTextContent('Paused'),
    )
    expect(within(destination).getByRole('link', { name: 'Renamed hook' })).toBeInTheDocument()
  })

  it('falls back to a short id and a retry when the endpoint cannot be read', async () => {
    let fail = true
    const { user } = mount({
      routes: {
        [`GET ${EP}`]: () =>
          fail ? apiError(500, 'internal_error', 'boom') : json(200, { endpoint: endpointBody() }),
      },
    })
    expect(
      await screen.findByRole('heading', {
        name: `Endpoint ${EP_ID.slice(0, 8)} → Billing hook`,
        level: 1,
      }),
    ).toBeInTheDocument()
    const section = screen.getByRole('region', { name: 'Ingestion endpoint' })
    expect(await within(section).findByText(/Endpoint details are unavailable/)).toBeInTheDocument()
    // Everything else still works.
    expect(screen.getByRole('button', { name: 'Revoke subscription' })).toBeInTheDocument()
    fail = false
    await user.click(within(section).getByRole('button', { name: 'Try again' }))
    expect(await within(section).findByText('Orders intake')).toBeInTheDocument()
  })

  it('uses the snapshot of the subscription when the destination cannot be read', async () => {
    mount({ routes: { [`GET ${DEST}`]: () => apiError(404, 'not_found', 'resource not found') } })
    const destination = await screen.findByRole('region', { name: 'Destination' })
    expect(
      await within(destination).findByText(/Destination details are unavailable/),
    ).toBeInTheDocument()
    expect(within(destination).getByRole('link', { name: 'Billing hook' })).toBeInTheDocument()
    expect(destination.querySelector('[data-kind="destination"]')).toHaveTextContent('Active')
  })

  it('keeps the page when the intervals cannot be loaded', async () => {
    mount({ routes: { [`GET ${SUBS}`]: () => apiError(500, 'internal_error', 'boom') } })
    expect(await screen.findByText(/The other intervals could not be loaded/)).toBeInTheDocument()
    expect(screen.getByRole('heading', { name: /Orders intake/, level: 1 })).toBeInTheDocument()
  })

  it('shows a safe not-found state with a retry', async () => {
    mount({ routes: { [`GET ${ONE}`]: () => apiError(404, 'not_found', 'resource not found') } })
    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent(/Subscription not found/)
    expect(within(alert).getByRole('button', { name: 'Try again' })).toBeInTheDocument()
  })

  it('rejects an answer for another subscription instead of showing it', async () => {
    mount({ routes: { [`GET ${ONE}`]: get({ id: SUB_ID_2 }) } })
    expect(await screen.findByRole('alert')).toHaveTextContent(/Could not load subscription/)
    expect(screen.queryByRole('heading', { name: /Orders intake/ })).not.toBeInTheDocument()
  })

  it('rejects an answer of another organization', async () => {
    mount({ routes: { [`GET ${ONE}`]: get({ organizationId: 'o2' }) } })
    expect(await screen.findByRole('alert')).toHaveTextContent(/Could not load subscription/)
  })

  it('shows a revoked subscription as ended, with no action', async () => {
    mount({
      routes: {
        [`GET ${ONE}`]: () => json(200, { subscription: revokedSubscriptionBody() }),
      },
    })
    expect(await screen.findByText(/This subscription has ended/)).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Revoke subscription' })).not.toBeInTheDocument()
    const overview = screen.getByRole('region', { name: 'Overview' })
    expect(overview.querySelector('[data-kind="subscription"]')).toHaveTextContent('Revoked')
    expect(within(overview).queryByText('Not ended')).not.toBeInTheDocument()
  })
})

describe('subscription actions by role', () => {
  it.each(['OWNER', 'ADMIN', 'DEVELOPER'] as const)('%s can revoke', async (role) => {
    mount({ role })
    expect(await screen.findByRole('button', { name: 'Revoke subscription' })).toBeInTheDocument()
  })

  it('shows a VIEWER no control and sends nothing however the page is used', async () => {
    const { user, calls } = mount({ role: 'VIEWER' })
    expect(
      await screen.findByText('You have read-only access to subscriptions.'),
    ).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /Revoke/ })).not.toBeInTheDocument()
    await user.keyboard('{Enter}{Escape}')
    expect(
      calls.filter((call) => call.method !== 'GET' && !call.path.includes('/auth/')),
    ).toHaveLength(0)
  })
})

describe('revoke subscription', () => {
  const revoked: Handler = () => json(200, { subscription: revokedSubscriptionBody() })

  it('explains the effect and needs the acknowledgment, and sends nothing before confirming', async () => {
    const { user, calls } = mount({ routes: { [`DELETE ${ONE}`]: revoked } })
    const dialog = await ready(user, 'Revoke subscription')
    expect(dialog).toHaveAccessibleDescription(
      /Revoking the subscription to Billing hook is permanent/,
    )
    expect(dialog).toHaveTextContent(/never reactivated/)
    expect(dialog).toHaveTextContent(/stay eligible for delivery/)
    expect(dialog).toHaveTextContent(/does not change the destination or the endpoint/)
    expect(confirm(dialog)).toBeDisabled()
    await user.click(confirm(dialog))
    expect(requests(calls, 'DELETE', ONE)).toHaveLength(0)
    await user.click(within(dialog).getByRole('checkbox'))
    expect(confirm(dialog)).toBeEnabled()
    await user.click(within(dialog).getByRole('button', { name: 'Cancel' }))
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    expect(requests(calls, 'DELETE', ONE)).toHaveLength(0)
  })

  it('revokes with one DELETE, shows the answer at once, reloads the intervals and moves focus', async () => {
    const { user, calls } = mount({ routes: { [`DELETE ${ONE}`]: revoked } })
    const dialog = await revoke(user)
    await user.click(confirm(dialog))
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    expect(requests(calls, 'DELETE', ONE)).toHaveLength(1)
    expect(requests(calls, 'DELETE', ONE)[0]!.body).toBeUndefined()
    const overview = screen.getByRole('region', { name: 'Overview' })
    expect(overview.querySelector('[data-kind="subscription"]')).toHaveTextContent('Revoked')
    expect(screen.getByText(/This subscription has ended/)).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Revoke subscription' })).not.toBeInTheDocument()
    expect(screen.getByRole('heading', { level: 1 })).toHaveFocus()
    // The subscription itself was not read again; only its intervals were.
    expect(requests(calls, 'GET', ONE)).toHaveLength(1)
    await waitFor(() => expect(requests(calls, 'GET', SUBS)).toHaveLength(2))
  })

  it('sends one DELETE for a double submit in the same tick', async () => {
    const { user, calls } = mount({ routes: { [`DELETE ${ONE}`]: revoked } })
    const dialog = await revoke(user)
    const button = confirm(dialog)
    await act(async () => {
      fireEvent.click(button)
      fireEvent.click(button)
      fireEvent.submit(button.closest('form')!)
    })
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 30))
    })
    expect(requests(calls, 'DELETE', ONE)).toHaveLength(1)
  })

  it('disables the controls and ignores Escape while the request runs', async () => {
    let finish: (response: Response) => void = () => undefined
    const pending = new Promise<Response>((resolve) => {
      finish = resolve
    })
    const { user } = mount({ routes: { [`DELETE ${ONE}`]: () => pending } })
    const dialog = await revoke(user)
    await user.click(confirm(dialog))
    await waitFor(() =>
      expect(within(dialog).getByRole('button', { name: 'Cancel' })).toBeDisabled(),
    )
    await user.keyboard('{Escape}')
    expect(screen.getByRole('dialog')).toBeInTheDocument()
    await act(async () => {
      finish(json(200, { subscription: revokedSubscriptionBody() }))
    })
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
  })

  it('hides the control after a 403 and says nothing was changed', async () => {
    const { user } = mount({
      routes: { [`DELETE ${ONE}`]: () => apiError(403, 'forbidden', 'you do not have permission') },
    })
    const dialog = await revoke(user)
    await user.click(confirm(dialog))
    expect(await screen.findByRole('alert', { name: 'Not permitted' })).toHaveTextContent(
      /Nothing was changed/,
    )
    expect(screen.queryByRole('button', { name: 'Revoke subscription' })).not.toBeInTheDocument()
    expect(screen.getByText('You can no longer change this subscription.')).toBeInTheDocument()
  })

  it('shows a 404 as not found and reads again', async () => {
    const { user, calls } = mount({
      routes: { [`DELETE ${ONE}`]: () => apiError(404, 'not_found', 'resource not found') },
    })
    const dialog = await revoke(user)
    await user.click(confirm(dialog))
    expect(await screen.findByRole('alert', { name: 'Subscription not found' })).toBeInTheDocument()
    expect(requests(calls, 'DELETE', ONE)).toHaveLength(1)
    await waitFor(() => expect(requests(calls, 'GET', ONE)).toHaveLength(2))
  })

  it('reads the subscription again after 409 subscription_revoked and shows it as ended', async () => {
    let reads = 0
    const { user, calls } = mount({
      routes: {
        [`GET ${ONE}`]: () => {
          reads += 1
          return json(200, {
            subscription: reads === 1 ? subscriptionBody() : revokedSubscriptionBody(),
          })
        },
        [`DELETE ${ONE}`]: () =>
          apiError(409, 'subscription_revoked', 'the subscription is already revoked'),
      },
    })
    const dialog = await revoke(user)
    await user.click(confirm(dialog))
    expect(await screen.findByRole('status', { name: 'Already revoked' })).toHaveTextContent(
      /nothing was changed/,
    )
    await waitFor(() => expect(requests(calls, 'GET', ONE)).toHaveLength(2))
    expect(await screen.findByText(/This subscription has ended/)).toBeInTheDocument()
    expect(requests(calls, 'DELETE', ONE)).toHaveLength(1)
  })

  const unknown: [string, Handler][] = [
    [
      'a lost connection',
      () => {
        throw new TypeError('Failed to fetch')
      },
    ],
    ['a proxy 502', () => apiError(502, 'bad_gateway', 'x')],
    ['an unreadable 200 (still ACTIVE)', () => json(200, { subscription: subscriptionBody() })],
    ['a 204', () => new Response(null, { status: 204 })],
    ['a 200 that is not JSON', () => new Response('<html>', { status: 200 })],
  ]
  it.each(unknown)(
    'treats %s as unknown: one DELETE, one read, no repeat, state read from the server',
    async (_name, del) => {
      let reads = 0
      const { user, calls } = mount({
        routes: {
          [`GET ${ONE}`]: () => {
            reads += 1
            return json(200, {
              subscription: reads === 1 ? subscriptionBody() : revokedSubscriptionBody(),
            })
          },
          [`DELETE ${ONE}`]: del,
        },
      })
      const dialog = await revoke(user)
      await user.click(confirm(dialog))
      const notice = await screen.findByRole('status', { name: 'The result is unknown' })
      expect(notice).toHaveTextContent(/It was not repeated/)
      await waitFor(() => expect(requests(calls, 'GET', ONE)).toHaveLength(2))
      expect(requests(calls, 'DELETE', ONE)).toHaveLength(1)
      // The read shows what the server really did.
      expect(await screen.findByText(/This subscription has ended/)).toBeInTheDocument()
      expect(screen.getByRole('heading', { level: 1 })).toHaveFocus()
    },
  )

  it('treats a request that takes 30 seconds as unknown and aborts it', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    const { user, calls } = mount({
      routes: {
        [`DELETE ${ONE}`]: (call) =>
          new Promise<Response>((_resolve, reject) => {
            call.signal?.addEventListener('abort', () =>
              reject(new DOMException('aborted', 'AbortError')),
            )
          }),
      },
    })
    const dialog = await revoke(user)
    await user.click(confirm(dialog))
    await waitFor(() => expect(requests(calls, 'DELETE', ONE)).toHaveLength(1))
    await act(async () => {
      vi.advanceTimersByTime(25_000)
    })
    expect(requests(calls, 'DELETE', ONE)[0]!.signal?.aborted).toBe(false)
    await act(async () => {
      vi.advanceTimersByTime(6_000)
    })
    expect(await screen.findByRole('status', { name: 'The result is unknown' })).toBeInTheDocument()
    expect(requests(calls, 'DELETE', ONE)[0]!.signal?.aborted).toBe(true)
    await waitFor(() => expect(requests(calls, 'GET', ONE)).toHaveLength(2))
    expect(requests(calls, 'DELETE', ONE)).toHaveLength(1)
  })

  it('aborts a pending revoke when the organization changes and ignores a late answer', async () => {
    let answer: (response: Response) => void = () => undefined
    const late = new Promise<Response>((resolve) => {
      answer = resolve
    })
    const { user, calls } = mount({ routes: { [`DELETE ${ONE}`]: () => late } })
    const dialog = await revoke(user)
    await user.click(confirm(dialog))
    await waitFor(() => expect(requests(calls, 'DELETE', ONE)).toHaveLength(1))
    await user.selectOptions(screen.getByLabelText('Organization'), 'o2')
    await waitFor(() => expect(screen.getByTestId('location')).toHaveTextContent('/o/o2'))
    expect(requests(calls, 'DELETE', ONE)[0]!.signal?.aborted).toBe(true)
    await act(async () => {
      answer(json(200, { subscription: revokedSubscriptionBody() }))
      await new Promise((resolve) => setTimeout(resolve, 20))
    })
    expect(requests(calls, 'GET', ONE)).toHaveLength(1)
    expect(screen.queryByText(/Orders intake/)).not.toBeInTheDocument()
  })

  it('does not change anything about the destination or the endpoint', async () => {
    const { user, calls } = mount({ routes: { [`DELETE ${ONE}`]: revoked } })
    const dialog = await revoke(user)
    await user.click(confirm(dialog))
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    const writes = calls.filter((call) => call.method !== 'GET' && !call.path.includes('/auth/'))
    expect(writes.map((call) => `${call.method} ${call.path}`)).toEqual([`DELETE ${ONE}`])
  })
})

describe('state isolation when following a link to another interval of the pair', () => {
  const ONE_B = `${SUBS}/${SUB_ID_2}`
  const B_STARTED = '2026-11-10T10:00:00Z'
  const bBody = () => subscriptionBody({ id: SUB_ID_2, createdAt: B_STARTED })
  // The stub serves two ACTIVE intervals for one pair, which the backend never would. These tests are about what the
  // page keeps when the route parameter changes, not about the pair rule.
  const twoIntervals: Record<string, Handler> = {
    [`GET ${SUBS}`]: () => json(200, subscriptionList([subscriptionBody(), bBody()])),
    [`GET ${ONE_B}`]: () => json(200, { subscription: bBody() }),
  }

  async function goToB(user: UserEvent, calls: Parameters<typeof requests>[0]) {
    await user.click(
      await screen.findByRole('link', {
        name: `Open the subscription that started ${B_STARTED}`,
      }),
    )
    await waitFor(() => expect(requests(calls, 'GET', ONE_B)).toHaveLength(1))
    // B is showing: its id is on the page and A's is gone.
    expect(await screen.findByText(SUB_ID_2)).toBeInTheDocument()
    expect(screen.queryByText(SUB_ID)).not.toBeInTheDocument()
  }

  it('does not hide the Revoke action of B after a 403 on A', async () => {
    const { user, calls } = mount({
      routes: {
        ...twoIntervals,
        [`DELETE ${ONE}`]: () => apiError(403, 'forbidden', 'you do not have permission'),
      },
    })
    const dialog = await revoke(user)
    await user.click(confirm(dialog))
    expect(await screen.findByRole('alert', { name: 'Not permitted' })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Revoke subscription' })).not.toBeInTheDocument()

    await goToB(user, calls)
    expect(await screen.findByRole('button', { name: 'Revoke subscription' })).toBeInTheDocument()
    expect(screen.queryByRole('alert', { name: 'Not permitted' })).not.toBeInTheDocument()
    expect(
      screen.queryByText('You can no longer change this subscription.'),
    ).not.toBeInTheDocument()
    // B is still governed by the user's real permission: nothing was sent for it.
    expect(requests(calls, 'DELETE', ONE_B)).toHaveLength(0)
  })

  it('does not show the "Already revoked" notice of A on B', async () => {
    let reads = 0
    const { user, calls } = mount({
      routes: {
        ...twoIntervals,
        [`GET ${ONE}`]: () => {
          reads += 1
          return json(200, {
            subscription: reads === 1 ? subscriptionBody() : revokedSubscriptionBody(),
          })
        },
        [`DELETE ${ONE}`]: () =>
          apiError(409, 'subscription_revoked', 'the subscription is already revoked'),
      },
    })
    const dialog = await revoke(user)
    await user.click(confirm(dialog))
    expect(await screen.findByRole('status', { name: 'Already revoked' })).toBeInTheDocument()

    await goToB(user, calls)
    expect(screen.queryByRole('status', { name: 'Already revoked' })).not.toBeInTheDocument()
    expect(screen.queryByText(/already revoked/i)).not.toBeInTheDocument()
    expect(await screen.findByRole('button', { name: 'Revoke subscription' })).toBeInTheDocument()
  })

  it('does not show the "result is unknown" notice of A on B', async () => {
    const { user, calls } = mount({
      routes: {
        ...twoIntervals,
        [`DELETE ${ONE}`]: () => {
          throw new TypeError('Failed to fetch')
        },
      },
    })
    const dialog = await revoke(user)
    await user.click(confirm(dialog))
    expect(await screen.findByRole('status', { name: 'The result is unknown' })).toBeInTheDocument()
    await waitFor(() => expect(requests(calls, 'GET', ONE)).toHaveLength(2))

    await goToB(user, calls)
    expect(screen.queryByRole('status', { name: 'The result is unknown' })).not.toBeInTheDocument()
    expect(screen.queryByText(/may or may not have been applied/)).not.toBeInTheDocument()
    expect(await screen.findByRole('button', { name: 'Revoke subscription' })).toBeInTheDocument()
    // Nothing was sent for B, and A's revocation was never repeated.
    expect(requests(calls, 'DELETE', ONE_B)).toHaveLength(0)
    expect(requests(calls, 'DELETE', ONE)).toHaveLength(1)
  })

  it('does not show the revoked answer of A as the state of B', async () => {
    const { user, calls } = mount({
      routes: {
        ...twoIntervals,
        [`DELETE ${ONE}`]: () => json(200, { subscription: revokedSubscriptionBody() }),
      },
    })
    const dialog = await revoke(user)
    await user.click(confirm(dialog))
    expect(await screen.findByText(/This subscription has ended/)).toBeInTheDocument()

    await goToB(user, calls)
    const overview = screen.getByRole('region', { name: 'Overview' })
    expect(overview.querySelector('[data-kind="subscription"]')).toHaveTextContent('Active')
    expect(screen.queryByText(/This subscription has ended/)).not.toBeInTheDocument()
    expect(await screen.findByRole('button', { name: 'Revoke subscription' })).toBeInTheDocument()
  })

  it('shows the loading state, not A, while B loads', async () => {
    let finishB: (response: Response) => void = () => undefined
    const slowB = new Promise<Response>((resolve) => {
      finishB = resolve
    })
    const { user, calls } = mount({
      routes: { ...twoIntervals, [`GET ${ONE_B}`]: () => slowB },
    })
    await screen.findByRole('button', { name: 'Revoke subscription' })
    await user.click(
      await screen.findByRole('link', {
        name: `Open the subscription that started ${B_STARTED}`,
      }),
    )
    // While B loads, A's page is not shown (no A id, no A actions).
    expect(await screen.findByText('Loading subscription…')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Revoke subscription' })).not.toBeInTheDocument()
    await act(async () => {
      finishB(json(200, { subscription: bBody() }))
    })
    expect(await screen.findByText(SUB_ID_2)).toBeInTheDocument()
    expect(requests(calls, 'GET', ONE_B)).toHaveLength(1)
  })
})
