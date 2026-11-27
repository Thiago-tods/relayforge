import { act, fireEvent, screen, waitFor, within } from '@testing-library/react'
import type { UserEvent } from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import { setCsrfToken } from '../../api/client'
import { destinationBody, destinationList } from '../../test/destinationFixtures'
import { apiError, json } from '../../test/fetchStub'
import type { Handler } from '../../test/fetchStub'
import {
  DEST_ID,
  DEST_ID_2,
  endpointBody,
  endpointList,
  EP_ID,
  EP_ID_2,
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
const EPS = `${ORG}/ingestion-endpoints`
const DESTS = `${ORG}/destinations`
const PAGE = '/o/o1/subscriptions'
const SUB_ID_3 = '99999999-9999-4999-8999-999999999999'

const rows = [
  subscriptionBody(),
  revokedSubscriptionBody({
    id: SUB_ID_2,
    endpointId: EP_ID_2,
    destinationId: DEST_ID_2,
    destinationName: 'Old hook',
    destinationStatus: 'REVOKED',
  }),
  subscriptionBody({ id: SUB_ID_3, destinationId: DEST_ID_2, destinationName: 'Old hook' }),
]

/** The endpoint detail routes for the endpoints of the sample rows. */
function endpointRoutes(): Record<string, Handler> {
  return {
    [`GET ${EPS}/${EP_ID}`]: () => json(200, { endpoint: endpointBody() }),
    [`GET ${EPS}/${EP_ID_2}`]: () =>
      json(200, {
        endpoint: endpointBody({ id: EP_ID_2, name: 'Refunds intake', status: 'PAUSED' }),
      }),
  }
}

function mount(setup: HarnessSetup = {}, path = PAGE) {
  return mountSubscriptions(path, {
    ...setup,
    routes: {
      [`GET ${SUBS}`]: () => json(200, subscriptionList(rows)),
      ...endpointRoutes(),
      ...setup.routes,
    },
  })
}

describe('subscriptions list', () => {
  it('shows the subscriptions with endpoint names from the detail API and two separate statuses', async () => {
    const { calls } = mount()
    const table = await screen.findByRole('table')
    expect(await within(table).findAllByText('Orders intake')).toHaveLength(2)
    expect(await within(table).findByText('Refunds intake')).toBeInTheDocument()
    for (const name of [
      'Ingestion endpoint',
      'Destination',
      'Destination status',
      'Subscription status',
      'Created',
      'Revoked',
    ]) {
      expect(within(table).getByRole('columnheader', { name })).toBeInTheDocument()
    }
    // Row 2: a REVOKED subscription whose destination is REVOKED; row 1: ACTIVE subscription, ACTIVE destination.
    const dataRows = within(table).getAllByRole('row').slice(1)
    expect(dataRows).toHaveLength(3)
    const badge = (row: HTMLElement, kind: string) =>
      row.querySelector(`[data-kind="${kind}"]`)?.textContent
    expect(badge(dataRows[0]!, 'subscription')).toBe('Active')
    expect(badge(dataRows[0]!, 'destination')).toBe('Active')
    expect(badge(dataRows[1]!, 'subscription')).toBe('Revoked')
    expect(badge(dataRows[1]!, 'destination')).toBe('Revoked')
    // The paused endpoint is labelled.
    expect(within(dataRows[1]!).getByText('Paused')).toBeInTheDocument()
    // Nothing identifies the creator.
    expect(document.body.textContent).not.toContain('u-1')
    // The first request is the page; endpoints are never listed, and each distinct endpoint is read once.
    const first = new URL(requests(calls, 'GET', SUBS)[0]!.path, 'http://x')
    expect(Object.fromEntries(first.searchParams)).toEqual({ limit: '25', offset: '0' })
    expect(requests(calls, 'GET', EPS)).toHaveLength(0)
    await waitFor(() => {
      expect(requests(calls, 'GET', `${EPS}/${EP_ID}`)).toHaveLength(1)
      expect(requests(calls, 'GET', `${EPS}/${EP_ID_2}`)).toHaveLength(1)
    })
  })

  it('shows an active subscription of a revoked destination with both statuses, not one merged status', async () => {
    mount({
      routes: {
        [`GET ${SUBS}`]: () =>
          json(200, subscriptionList([subscriptionBody({ destinationStatus: 'REVOKED' })])),
      },
    })
    const table = await screen.findByRole('table')
    const row = within(table).getAllByRole('row')[1]!
    expect(row.querySelector('[data-kind="subscription"]')).toHaveTextContent('Active')
    expect(row.querySelector('[data-kind="destination"]')).toHaveTextContent('Revoked')
  })

  it('falls back to a short id when an endpoint cannot be read, and the page keeps working', async () => {
    mount({
      routes: {
        [`GET ${EPS}/${EP_ID}`]: () => apiError(404, 'not_found', 'resource not found'),
        [`GET ${EPS}/${EP_ID_2}`]: () => {
          throw new TypeError('Failed to fetch')
        },
      },
    })
    const table = await screen.findByRole('table')
    expect(await within(table).findAllByText(`Endpoint ${EP_ID.slice(0, 8)}`)).toHaveLength(2)
    expect(within(table).getByText(`Endpoint ${EP_ID_2.slice(0, 8)}`)).toBeInTheDocument()
    expect(within(table).getAllByRole('link', { name: /^Manage/ })).toHaveLength(3)
  })

  it('shows an empty state with a create action for someone who can create', async () => {
    mount({ routes: { [`GET ${SUBS}`]: () => json(200, subscriptionList([])) } })
    expect(await screen.findByTestId('subscriptions-empty')).toHaveTextContent(
      /No subscriptions yet/,
    )
    expect(screen.getByRole('button', { name: 'Create subscription' })).toBeInTheDocument()
  })

  it('shows an empty state without a create action for a VIEWER', async () => {
    mount({
      role: 'VIEWER',
      routes: { [`GET ${SUBS}`]: () => json(200, subscriptionList([])) },
    })
    expect(await screen.findByTestId('subscriptions-empty')).toHaveTextContent(
      /Someone who can create subscriptions/,
    )
    expect(screen.queryByRole('button', { name: 'Create subscription' })).not.toBeInTheDocument()
  })

  it('shows an error state with a retry that loads again', async () => {
    let fail = true
    const { calls } = mount({
      routes: {
        [`GET ${SUBS}`]: () =>
          fail ? apiError(500, 'internal_error', 'boom') : json(200, subscriptionList(rows)),
      },
    })
    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent(/Could not load subscriptions/)
    expect(screen.queryByRole('table')).not.toBeInTheDocument()
    fail = false
    const user = (await import('@testing-library/user-event')).default.setup()
    await user.click(within(alert).getByRole('button', { name: 'Try again' }))
    expect(await screen.findByRole('table')).toBeInTheDocument()
    expect(requests(calls, 'GET', SUBS)).toHaveLength(2)
  })

  it('treats a malformed answer as an error and shows no rows', async () => {
    mount({
      routes: {
        [`GET ${SUBS}`]: () =>
          json(200, subscriptionList([subscriptionBody({ organizationId: 'o2' })])),
      },
    })
    expect(await screen.findByRole('alert')).toHaveTextContent(/Could not load subscriptions/)
    expect(screen.queryByRole('table')).not.toBeInTheDocument()
  })

  it('filters by subscription status through the query and the URL', async () => {
    const { user, calls } = mount()
    await screen.findByRole('table')
    await user.selectOptions(screen.getByLabelText('Subscription status'), 'REVOKED')
    await waitFor(() => expect(screen.getByTestId('search')).toHaveTextContent('status=REVOKED'))
    await waitFor(() => {
      const last = requests(calls, 'GET', SUBS).at(-1)!
      expect(new URL(last.path, 'http://x').searchParams.get('status')).toBe('REVOKED')
    })
    await user.selectOptions(screen.getByLabelText('Subscription status'), '')
    await waitFor(() => expect(screen.getByTestId('search')).toHaveTextContent(/^$/))
  })

  it('ignores an unknown status, a malformed id and a malformed offset in the URL', async () => {
    const { calls } = mount(
      {},
      `${PAGE}?status=PAUSED&endpointId=nope&destinationId=also-nope&offset=7`,
    )
    await screen.findByRole('table')
    const url = new URL(requests(calls, 'GET', SUBS)[0]!.path, 'http://x')
    expect(Object.fromEntries(url.searchParams)).toEqual({ limit: '25', offset: '0' })
  })

  it('sends valid endpoint and destination filters from the URL', async () => {
    const { calls } = mount({}, `${PAGE}?endpointId=${EP_ID}&destinationId=${DEST_ID}&offset=25`)
    await screen.findByRole('table')
    const url = new URL(requests(calls, 'GET', SUBS)[0]!.path, 'http://x')
    expect(Object.fromEntries(url.searchParams)).toEqual({
      limit: '25',
      offset: '25',
      endpointId: EP_ID,
      destinationId: DEST_ID,
    })
  })

  it('loads endpoint and destination choices on request, 100 at a time, and filters by the choice', async () => {
    const { user, calls } = mount({
      routes: {
        [`GET ${EPS}`]: () =>
          json(
            200,
            endpointList([endpointBody(), endpointBody({ id: EP_ID_2, name: 'Refunds' })], 2),
          ),
        [`GET ${DESTS}`]: () => json(200, destinationList([destinationBody()], 1, 0, 100)),
      },
    })
    await screen.findByRole('table')
    // Nothing is loaded until it is asked for.
    expect(requests(calls, 'GET', EPS)).toHaveLength(0)
    const group = screen
      .getByLabelText('Ingestion endpoint')
      .closest('.paged-select') as HTMLElement
    await user.click(within(group).getByRole('button', { name: 'Load endpoints' }))
    expect(await within(group).findByText('All endpoints are loaded.')).toBeInTheDocument()
    const query = new URL(requests(calls, 'GET', EPS)[0]!.path, 'http://x').searchParams
    expect(Object.fromEntries(query)).toEqual({ limit: '100', offset: '0' })
    await user.selectOptions(screen.getByLabelText('Ingestion endpoint'), EP_ID_2)
    await waitFor(() => {
      const last = requests(calls, 'GET', SUBS).at(-1)!
      expect(new URL(last.path, 'http://x').searchParams.get('endpointId')).toBe(EP_ID_2)
    })
    expect(screen.getByTestId('search')).toHaveTextContent(`endpointId=${EP_ID_2}`)
  })

  it('pages with the existing offset pagination and the exact total', async () => {
    const { user, calls } = mount({
      routes: {
        [`GET ${SUBS}`]: (call) => {
          const offset = Number(new URL(call.path, 'http://x').searchParams.get('offset'))
          return json(200, subscriptionList(offset === 0 ? rows : [rows[0]!], 26, offset))
        },
      },
    })
    await screen.findByRole('table')
    expect(screen.getByText('26 subscriptions.')).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: 'Next' }))
    await waitFor(() => expect(screen.getByTestId('search')).toHaveTextContent('offset=25'))
    await waitFor(() => expect(requests(calls, 'GET', SUBS)).toHaveLength(2))
    expect(await screen.findByText('Page 2 (last page)')).toBeInTheDocument()
  })

  it('never shows subscriptions of the previous organization after a switch', async () => {
    const { user } = mount({
      routes: {
        'GET /api/v1/organizations/o2/subscriptions': () =>
          json(200, subscriptionList([subscriptionBody({ organizationId: 'o2', id: SUB_ID_2 })])),
      },
    })
    await screen.findByRole('table')
    expect(screen.getAllByText('Billing hook').length).toBeGreaterThan(0)
    await user.selectOptions(screen.getByLabelText('Organization'), 'o2')
    await waitFor(() => expect(screen.getByTestId('location')).toHaveTextContent('/o/o2'))
    expect(screen.queryByText('Orders intake')).not.toBeInTheDocument()
  })

  it('does not poll: one read of the page and no background refresh', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    const { calls } = mount()
    await screen.findByRole('table')
    await act(async () => {
      vi.advanceTimersByTime(120_000)
    })
    expect(requests(calls, 'GET', SUBS)).toHaveLength(1)
  })
})

describe('subscriptions navigation', () => {
  it.each(['OWNER', 'ADMIN', 'DEVELOPER', 'VIEWER'] as const)(
    '%s sees Subscriptions as a link in the Configuration group next to Destinations',
    async (role) => {
      mount({ role, routes: { [`GET ${SUBS}`]: () => json(200, subscriptionList([])) } })
      const nav = await screen.findByRole('navigation', { name: 'Main navigation' })
      const group = within(nav).getByRole('group', { name: 'Configuration' })
      expect(
        within(group)
          .getAllByRole('link')
          .map((link) => link.textContent),
      ).toEqual(['Destinations', 'Subscriptions'])
      expect(within(group).getByRole('link', { name: 'Subscriptions' })).toHaveAttribute(
        'href',
        '/o/o1/subscriptions',
      )
      expect(within(group).getByRole('link', { name: 'Subscriptions' })).toHaveAttribute(
        'aria-current',
        'page',
      )
    },
  )
})

// ---------------------------------------------------------------------------------------------------------------
// Create
// ---------------------------------------------------------------------------------------------------------------

const PAIR = { endpointId: EP_ID, destinationId: DEST_ID }

const pickerRoutes = (): Record<string, Handler> => ({
  [`GET ${EPS}`]: () =>
    json(
      200,
      endpointList([
        endpointBody(),
        endpointBody({ id: EP_ID_2, name: 'Refunds intake', status: 'PAUSED' }),
        endpointBody({
          id: '12121212-1212-4212-8212-121212121212',
          name: 'Dead intake',
          status: 'REVOKED',
          revokedAt: '2026-11-22T09:00:00Z',
        }),
      ]),
    ),
  [`GET ${DESTS}`]: () =>
    json(
      200,
      destinationList(
        [
          destinationBody(),
          destinationBody({ id: DEST_ID_2, name: 'Old hook', status: 'PAUSED' }),
          destinationBody({
            id: '34343434-3434-4434-8434-343434343434',
            name: 'Gone hook',
            status: 'REVOKED',
            revokedAt: '2026-11-22T09:00:00Z',
          }),
        ],
        3,
        0,
        100,
      ),
    ),
})

async function openCreate(user: UserEvent) {
  setCsrfToken('tok')
  await user.click(await screen.findByRole('button', { name: 'Create subscription' }))
  const dialog = screen.getByRole('dialog', { name: 'Create subscription' })
  await within(dialog).findByRole('option', { name: 'Orders intake (Active)' })
  await within(dialog).findByRole('option', { name: 'Billing hook (Active)' })
  return dialog
}

async function choose(
  user: UserEvent,
  dialog: HTMLElement,
  endpoint = EP_ID,
  destination = DEST_ID,
) {
  await user.selectOptions(within(dialog).getByLabelText('Ingestion endpoint'), endpoint)
  await user.selectOptions(within(dialog).getByLabelText('Destination'), destination)
}

const submit = (dialog: HTMLElement) =>
  within(dialog).getByRole('button', { name: 'Create subscription' })

const pairReads = (calls: Parameters<typeof requests>[0]) =>
  requests(calls, 'GET', SUBS).filter(
    (call) => new URL(call.path, 'http://x').searchParams.get('endpointId') !== null,
  )

describe('create subscription', () => {
  const created: Handler = () => json(201, { subscription: subscriptionBody() })
  const base = (
    post: Handler = created,
    pair: Handler = () => json(200, subscriptionList([])),
  ) => ({
    routes: {
      ...pickerRoutes(),
      [`POST ${SUBS}`]: post,
      [`GET ${SUBS}`]: (call: Parameters<Handler>[0]) =>
        new URL(call.path, 'http://x').searchParams.get('endpointId') !== null
          ? pair(call)
          : json(200, subscriptionList(rows)),
    },
  })

  it('offers non-revoked endpoints and destinations only, labelled with their status', async () => {
    const { user } = mount(base())
    const dialog = await openCreate(user)
    const options = (label: string) =>
      within(within(dialog).getByLabelText(label))
        .getAllByRole('option')
        .map((option) => option.textContent)
    expect(options('Ingestion endpoint')).toEqual([
      'Choose…',
      'Orders intake (Active)',
      'Refunds intake (Paused)',
    ])
    expect(options('Destination')).toEqual([
      'Choose…',
      'Billing hook (Active)',
      'Old hook (Paused)',
    ])
  })

  it('requires both choices and sends nothing before they are made', async () => {
    const { user, calls } = mount(base())
    const dialog = await openCreate(user)
    await user.click(submit(dialog))
    expect(await within(dialog).findByText('Choose an ingestion endpoint.')).toBeInTheDocument()
    expect(within(dialog).getByText('Choose a destination.')).toBeInTheDocument()
    expect(requests(calls, 'POST', SUBS)).toHaveLength(0)
    expect(pairReads(calls)).toHaveLength(0)
  })

  it('checks the pair, creates with exactly the two ids, refreshes the list and closes', async () => {
    const { user, calls } = mount(base())
    const dialog = await openCreate(user)
    await choose(user, dialog)
    await user.click(submit(dialog))
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    expect(requests(calls, 'POST', SUBS)).toHaveLength(1)
    expect(requests(calls, 'POST', SUBS)[0]!.body).toEqual(PAIR)
    const check = new URL(pairReads(calls)[0]!.path, 'http://x').searchParams
    expect(Object.fromEntries(check)).toEqual({
      limit: '1',
      offset: '0',
      status: 'ACTIVE',
      endpointId: EP_ID,
      destinationId: DEST_ID,
    })
    await waitFor(() =>
      expect(
        requests(calls, 'GET', SUBS).filter((c) => !c.path.includes('endpointId')),
      ).toHaveLength(2),
    )
    expect(screen.getByRole('heading', { name: 'Subscriptions', level: 1 })).toHaveFocus()
  })

  it('creates for a paused endpoint and a paused destination', async () => {
    const { user, calls } = mount(
      base(() =>
        json(201, {
          subscription: subscriptionBody({
            endpointId: EP_ID_2,
            destinationId: DEST_ID_2,
            destinationStatus: 'PAUSED',
          }),
        }),
      ),
    )
    const dialog = await openCreate(user)
    await choose(user, dialog, EP_ID_2, DEST_ID_2)
    await user.click(submit(dialog))
    await waitFor(() => expect(requests(calls, 'POST', SUBS)).toHaveLength(1))
    expect(requests(calls, 'POST', SUBS)[0]!.body).toEqual({
      endpointId: EP_ID_2,
      destinationId: DEST_ID_2,
    })
  })

  it('sends nothing when the pair already has an active subscription, and links to it', async () => {
    const { user, calls } = mount(
      base(created, () => json(200, subscriptionList([subscriptionBody()], 1, 0, 1))),
    )
    const dialog = await openCreate(user)
    await choose(user, dialog)
    await user.click(submit(dialog))
    const alert = await within(dialog).findByRole('alert')
    expect(alert).toHaveTextContent(/An active subscription already exists/)
    expect(within(alert).getByRole('link', { name: 'Open it' })).toHaveAttribute(
      'href',
      `/o/o1/subscriptions/${SUB_ID}`,
    )
    expect(requests(calls, 'POST', SUBS)).toHaveLength(0)
    expect(screen.getByRole('dialog', { name: 'Create subscription' })).toBeInTheDocument()
  })

  it('lets the server decide when the pair check fails', async () => {
    const { user, calls } = mount(base(created, () => apiError(500, 'internal_error', 'boom')))
    const dialog = await openCreate(user)
    await choose(user, dialog)
    await user.click(submit(dialog))
    await waitFor(() => expect(requests(calls, 'POST', SUBS)).toHaveLength(1))
  })

  it('shows 409 already_subscribed from the server in the dialog', async () => {
    const { user, calls } = mount(
      base(() =>
        apiError(
          409,
          'already_subscribed',
          'an active subscription already exists for this endpoint and destination',
        ),
      ),
    )
    const dialog = await openCreate(user)
    await choose(user, dialog)
    await user.click(submit(dialog))
    expect(await within(dialog).findByRole('alert')).toHaveTextContent(/already exists/)
    expect(requests(calls, 'POST', SUBS)).toHaveLength(1)
    expect(screen.getByRole('dialog', { name: 'Create subscription' })).toBeInTheDocument()
  })

  it.each([
    ['endpoint_revoked', 'Ingestion endpoint', 'the ingestion endpoint is revoked'],
    ['destination_revoked', 'Destination', 'the destination is revoked'],
  ])('shows 409 %s on the field it belongs to', async (code, field, message) => {
    const { user } = mount(base(() => apiError(409, code, message)))
    const dialog = await openCreate(user)
    await choose(user, dialog)
    await user.click(submit(dialog))
    const select = within(dialog).getByLabelText(field)
    await waitFor(() => expect(select).toHaveAttribute('aria-invalid', 'true'))
    expect(await within(dialog).findByText(message)).toBeInTheDocument()
  })

  it('reports a 404 as a definite rejection: nothing created, no unknown result, no list reload', async () => {
    const { user, calls } = mount(base(() => apiError(404, 'not_found', 'resource not found')))
    const dialog = await openCreate(user)
    await choose(user, dialog)
    await user.click(submit(dialog))
    const notice = await screen.findByRole('alert', { name: 'Subscription not created' })
    expect(notice).toHaveTextContent(/Nothing was created/)
    expect(screen.queryByRole('status', { name: 'The result is unknown' })).not.toBeInTheDocument()
    expect(requests(calls, 'POST', SUBS)).toHaveLength(1)
    expect(requests(calls, 'GET', SUBS).filter((c) => !c.path.includes('endpointId'))).toHaveLength(
      1,
    )
  })

  it('hides the create action after a 403 and says nothing was created', async () => {
    const { user } = mount(base(() => apiError(403, 'forbidden', 'you do not have permission')))
    const dialog = await openCreate(user)
    await choose(user, dialog)
    await user.click(submit(dialog))
    expect(await screen.findByRole('alert', { name: 'Not permitted' })).toHaveTextContent(
      /Nothing was created/,
    )
    expect(screen.queryByRole('button', { name: 'Create subscription' })).not.toBeInTheDocument()
  })

  it('does not offer creation to a VIEWER', async () => {
    mount({ role: 'VIEWER', ...base() })
    await screen.findByRole('table')
    expect(screen.queryByRole('button', { name: 'Create subscription' })).not.toBeInTheDocument()
  })

  const ambiguous: [string, Handler][] = [
    [
      'a lost connection',
      () => {
        throw new TypeError('Failed to fetch')
      },
    ],
    ['a proxy 502', () => apiError(502, 'bad_gateway', 'x')],
    [
      'an unreadable 201',
      () => json(201, { subscription: subscriptionBody({ status: 'REVOKED' }) }),
    ],
    ['a 200 that is not JSON', () => new Response('<html>', { status: 200 })],
  ]

  it.each(ambiguous)(
    'treats %s as unknown: one POST, one read of the pair, nothing submitted again',
    async (_name, post) => {
      const { user, calls } = mount(base(post))
      const dialog = await openCreate(user)
      await choose(user, dialog)
      await user.click(submit(dialog))
      const notice = await screen.findByRole('status', { name: 'The result is unknown' })
      expect(notice).toHaveTextContent(/It was not submitted again/)
      expect(requests(calls, 'POST', SUBS)).toHaveLength(1)
      // One check before sending and one read after the unknown result.
      expect(pairReads(calls)).toHaveLength(2)
      await waitFor(() =>
        expect(
          requests(calls, 'GET', SUBS).filter((c) => !c.path.includes('endpointId')),
        ).toHaveLength(2),
      )
    },
  )

  it('says the subscription exists when the read after an unknown result finds it', async () => {
    let reads = 0
    const { user, calls } = mount(
      base(
        () => apiError(502, 'bad_gateway', 'x'),
        () => {
          reads += 1
          return json(
            200,
            subscriptionList(reads === 1 ? [] : [subscriptionBody()], reads === 1 ? 0 : 1, 0, 1),
          )
        },
      ),
    )
    const dialog = await openCreate(user)
    await choose(user, dialog)
    await user.click(submit(dialog))
    const notice = await screen.findByRole('status', { name: 'The subscription exists' })
    expect(within(notice).getByRole('link', { name: 'Open it' })).toHaveAttribute(
      'href',
      `/o/o1/subscriptions/${SUB_ID}`,
    )
    expect(requests(calls, 'POST', SUBS)).toHaveLength(1)
  })

  it('sends one POST for a double submit in the same tick', async () => {
    const { user, calls } = mount(base())
    const dialog = await openCreate(user)
    await choose(user, dialog)
    const button = submit(dialog)
    await act(async () => {
      fireEvent.click(button)
      fireEvent.click(button)
      fireEvent.submit(button.closest('form')!)
    })
    await waitFor(() => expect(requests(calls, 'POST', SUBS)).toHaveLength(1))
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 30))
    })
    expect(requests(calls, 'POST', SUBS)).toHaveLength(1)
    expect(pairReads(calls)).toHaveLength(1)
  })

  it('treats a request that takes 30 seconds as unknown and aborts it', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    const { user, calls } = mount(
      base(
        (call) =>
          new Promise<Response>((_resolve, reject) => {
            call.signal?.addEventListener('abort', () =>
              reject(new DOMException('aborted', 'AbortError')),
            )
          }),
      ),
    )
    const dialog = await openCreate(user)
    await choose(user, dialog)
    await user.click(submit(dialog))
    await waitFor(() => expect(requests(calls, 'POST', SUBS)).toHaveLength(1))
    await act(async () => {
      vi.advanceTimersByTime(25_000)
    })
    expect(requests(calls, 'POST', SUBS)[0]!.signal?.aborted).toBe(false)
    await act(async () => {
      vi.advanceTimersByTime(6_000)
    })
    expect(await screen.findByRole('status', { name: 'The result is unknown' })).toBeInTheDocument()
    expect(requests(calls, 'POST', SUBS)[0]!.signal?.aborted).toBe(true)
    expect(requests(calls, 'POST', SUBS)).toHaveLength(1)
  })

  it('aborts a pending create when the organization changes and ignores a late answer', async () => {
    let answer: (response: Response) => void = () => undefined
    const late = new Promise<Response>((resolve) => {
      answer = resolve
    })
    const { user, calls } = mount(base(() => late))
    const dialog = await openCreate(user)
    await choose(user, dialog)
    await user.click(submit(dialog))
    await waitFor(() => expect(requests(calls, 'POST', SUBS)).toHaveLength(1))
    await user.selectOptions(screen.getByLabelText('Organization'), 'o2')
    await waitFor(() => expect(screen.getByTestId('location')).toHaveTextContent('/o/o2'))
    expect(requests(calls, 'POST', SUBS)[0]!.signal?.aborted).toBe(true)
    await act(async () => {
      answer(json(201, { subscription: subscriptionBody() }))
      await new Promise((resolve) => setTimeout(resolve, 20))
    })
    expect(
      screen.queryByRole('status', { name: 'The subscription exists' }),
    ).not.toBeInTheDocument()
    expect(requests(calls, 'POST', SUBS)).toHaveLength(1)
  })

  it('loads more choices 100 at a time with the exact total', async () => {
    const { user, calls } = mount({
      routes: {
        ...base().routes,
        [`GET ${EPS}`]: (call) => {
          const offset = Number(new URL(call.path, 'http://x').searchParams.get('offset'))
          return json(
            200,
            endpointList(
              offset === 0
                ? [endpointBody()]
                : [endpointBody({ id: EP_ID_2, name: 'Second page intake' })],
              150,
              offset,
            ),
          )
        },
      },
    })
    const dialog = await openCreate(user)
    const group = within(dialog)
      .getByLabelText('Ingestion endpoint')
      .closest('.paged-select') as HTMLElement
    await user.click(
      within(group).getByRole('button', { name: /Load more endpoints \(1 of 150\)/ }),
    )
    expect(
      await within(dialog).findByRole('option', { name: 'Second page intake (Active)' }),
    ).toBeInTheDocument()
    expect(
      requests(calls, 'GET', EPS).map((c) =>
        new URL(c.path, 'http://x').searchParams.get('offset'),
      ),
    ).toEqual(['0', '1'])
  })

  it('shows a load failure of a picker without blocking the dialog', async () => {
    const { user } = mount({
      routes: {
        ...base().routes,
        [`GET ${EPS}`]: () => apiError(500, 'internal_error', 'boom'),
      },
    })
    setCsrfToken('tok')
    await user.click(await screen.findByRole('button', { name: 'Create subscription' }))
    const dialog = screen.getByRole('dialog', { name: 'Create subscription' })
    expect(await within(dialog).findByText(/Could not load endpoints/)).toBeInTheDocument()
    expect(within(dialog).getByRole('button', { name: 'Cancel' })).toBeEnabled()
  })
})

// ---------------------------------------------------------------------------------------------------------------
// Tenant isolation: answers of a request that was started for another organization
// ---------------------------------------------------------------------------------------------------------------

describe('answers that arrive after the organization changed', () => {
  const ORG2 = '/api/v1/organizations/o2'
  const o2Row = () =>
    subscriptionBody({
      id: SUB_ID_2,
      organizationId: 'o2',
      destinationName: 'Org two hook',
    })

  /** Switches to organization o2 and opens its Subscriptions page by the sidebar link. */
  async function openOrgTwoSubscriptions(user: UserEvent) {
    await user.selectOptions(screen.getByLabelText('Organization'), 'o2')
    await waitFor(() => expect(screen.getByTestId('location')).toHaveTextContent('/o/o2'))
    const nav = await screen.findByRole('navigation', { name: 'Main navigation' })
    await user.click(within(nav).getByRole('link', { name: 'Subscriptions' }))
    await waitFor(() =>
      expect(screen.getByTestId('location')).toHaveTextContent('/o/o2/subscriptions'),
    )
  }

  it('never shows the subscriptions of organization A, answered late, on the page of organization B', async () => {
    let answerA: (response: Response) => void = () => undefined
    const lateA = new Promise<Response>((resolve) => {
      answerA = resolve
    })
    const { user, calls } = mountSubscriptions(PAGE, {
      routes: {
        [`GET ${SUBS}`]: () => lateA,
        [`GET ${ORG2}/subscriptions`]: () => json(200, subscriptionList([o2Row()])),
        [`GET ${ORG2}/ingestion-endpoints/${EP_ID}`]: () =>
          json(200, {
            endpoint: endpointBody({ organizationId: 'o2', name: 'Org two intake' }),
          }),
        ...endpointRoutes(),
      },
    })
    // The request of A is pending.
    expect(await screen.findByText('Loading subscriptions…')).toBeInTheDocument()
    expect(requests(calls, 'GET', SUBS)).toHaveLength(1)

    await openOrgTwoSubscriptions(user)
    const table = await screen.findByRole('table')
    expect(await within(table).findByText('Org two hook')).toBeInTheDocument()
    expect(await within(table).findByText('Org two intake')).toBeInTheDocument()
    // A's request was cancelled when its page went away.
    expect(requests(calls, 'GET', SUBS)[0]!.signal?.aborted).toBe(true)

    // A answers now, with rows that must never reach B's page.
    await act(async () => {
      answerA(json(200, subscriptionList(rows)))
      await new Promise((resolve) => setTimeout(resolve, 30))
    })
    expect(screen.queryByText('Billing hook')).not.toBeInTheDocument()
    expect(screen.queryByText('Old hook')).not.toBeInTheDocument()
    expect(screen.queryByText('Orders intake')).not.toBeInTheDocument()
    const body = within(screen.getByRole('table')).getAllByRole('row').slice(1)
    expect(body).toHaveLength(1)
    expect(within(body[0]!).getByText('Org two hook')).toBeInTheDocument()
    expect(screen.getByText('1 subscription.')).toBeInTheDocument()
  })

  it('never shows the endpoint name of organization A, answered late, for the same endpoint id in organization B', async () => {
    let answerA: (response: Response) => void = () => undefined
    const lateA = new Promise<Response>((resolve) => {
      answerA = resolve
    })
    // Worst case: both organizations have a row for an endpoint with the same id.
    const { user, calls } = mountSubscriptions(PAGE, {
      routes: {
        [`GET ${SUBS}`]: () => json(200, subscriptionList([subscriptionBody()])),
        [`GET ${EPS}/${EP_ID}`]: () => lateA,
        [`GET ${ORG2}/subscriptions`]: () => json(200, subscriptionList([o2Row()])),
        [`GET ${ORG2}/ingestion-endpoints/${EP_ID}`]: () =>
          json(200, {
            endpoint: endpointBody({ organizationId: 'o2', name: 'Org two intake' }),
          }),
      },
    })
    const first = await screen.findByRole('table')
    expect(within(first).getByText('Loading name…')).toBeInTheDocument()
    await waitFor(() => expect(requests(calls, 'GET', `${EPS}/${EP_ID}`)).toHaveLength(1))

    await openOrgTwoSubscriptions(user)
    const table = await screen.findByRole('table')
    expect(await within(table).findByText('Org two intake')).toBeInTheDocument()
    expect(requests(calls, 'GET', `${EPS}/${EP_ID}`)[0]!.signal?.aborted).toBe(true)

    // A's lookup answers now. Its name must not replace B's, and nothing of A is shown.
    await act(async () => {
      answerA(json(200, { endpoint: endpointBody({ name: 'Orders intake' }) }))
      await new Promise((resolve) => setTimeout(resolve, 30))
    })
    expect(screen.queryByText('Orders intake')).not.toBeInTheDocument()
    expect(within(screen.getByRole('table')).getByText('Org two intake')).toBeInTheDocument()
    // B's own lookup ran once, for B's organization.
    expect(requests(calls, 'GET', `${ORG2}/ingestion-endpoints/${EP_ID}`)).toHaveLength(1)
  })
})

// ---------------------------------------------------------------------------------------------------------------
// Lifecycle: re-subscribing after a revocation (ADR 26)
// ---------------------------------------------------------------------------------------------------------------

describe('re-subscription after a revocation', () => {
  const OLD_STARTED = '2026-11-20T10:00:00Z'
  const OLD_ENDED = '2026-11-22T09:00:00Z'
  const NEW_STARTED = '2026-11-23T10:00:00Z'
  const oldRecord = () => revokedSubscriptionBody({ createdAt: OLD_STARTED, revokedAt: OLD_ENDED })
  const newRecord = () => subscriptionBody({ id: SUB_ID_3, createdAt: NEW_STARTED })

  it('creates a new ACTIVE record next to the revoked one, which is neither reactivated nor overwritten', async () => {
    let created = false
    const { user, calls } = mountSubscriptions(PAGE, {
      routes: {
        ...pickerRoutes(),
        ...endpointRoutes(),
        [`GET ${SUBS}`]: (call) => {
          const query = new URL(call.path, 'http://x').searchParams
          if (query.get('endpointId') !== null && query.get('status') === 'ACTIVE') {
            // The pair check asks for ACTIVE subscriptions only: the revoked record does not match it.
            return json(200, subscriptionList([], 0, 0, 1))
          }
          return json(200, subscriptionList(created ? [newRecord(), oldRecord()] : [oldRecord()]))
        },
        [`POST ${SUBS}`]: () => {
          created = true
          return json(201, { subscription: newRecord() })
        },
      },
    })

    // Before: the pair has one REVOKED subscription and no ACTIVE one.
    const before = await screen.findByRole('table')
    const beforeRows = within(before).getAllByRole('row').slice(1)
    expect(beforeRows).toHaveLength(1)
    expect(beforeRows[0]!.querySelector('[data-kind="subscription"]')).toHaveTextContent('Revoked')

    const dialog = await openCreate(user)
    await choose(user, dialog)
    await user.click(submit(dialog))
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())

    // The ACTIVE-only pair check found nothing, so the create was allowed to proceed, once, with the pair only.
    const check = new URL(pairReads(calls)[0]!.path, 'http://x').searchParams
    expect(check.get('status')).toBe('ACTIVE')
    expect(requests(calls, 'POST', SUBS)).toHaveLength(1)
    expect(requests(calls, 'POST', SUBS)[0]!.body).toEqual({
      endpointId: EP_ID,
      destinationId: DEST_ID,
    })
    expect(screen.queryByText(/An active subscription already exists/)).not.toBeInTheDocument()

    // After: two records for the pair, the new one ACTIVE and the old one still REVOKED with its own interval.
    await waitFor(() =>
      expect(within(screen.getByRole('table')).getAllByRole('row').slice(1)).toHaveLength(2),
    )
    const [newest, previous] = within(screen.getByRole('table')).getAllByRole('row').slice(1)
    expect(newest!.querySelector('[data-kind="subscription"]')).toHaveTextContent('Active')
    expect(within(newest!).getByRole('link', { name: /^Manage/ })).toHaveAttribute(
      'href',
      `/o/o1/subscriptions/${SUB_ID_3}`,
    )
    expect(previous!.querySelector('[data-kind="subscription"]')).toHaveTextContent('Revoked')
    expect(within(previous!).getByRole('link', { name: /^Manage/ })).toHaveAttribute(
      'href',
      `/o/o1/subscriptions/${SUB_ID}`,
    )
    expect(within(previous!).getByText('2026-11-22 09:00:00 UTC')).toBeInTheDocument()

    // Nothing was written except the one POST: no DELETE, PATCH or PUT, and nothing addressed to the old record.
    const writes = calls.filter((call) => call.method !== 'GET' && !call.path.includes('/auth/'))
    expect(writes.map((call) => `${call.method} ${call.path}`)).toEqual([`POST ${SUBS}`])
    expect(calls.some((call) => call.path.includes(`/${SUB_ID}`))).toBe(false)
  })

  it('does not treat an answer that is the revoked record as a successful re-subscription', async () => {
    // A server that "reactivated" the old record would answer with its id. The page accepts only an ACTIVE answer
    // for the pair and shows an inconsistent one as an unknown result.
    const { user, calls } = mountSubscriptions(PAGE, {
      routes: {
        ...pickerRoutes(),
        ...endpointRoutes(),
        [`GET ${SUBS}`]: (call) =>
          new URL(call.path, 'http://x').searchParams.get('status') === 'ACTIVE'
            ? json(200, subscriptionList([], 0, 0, 1))
            : json(200, subscriptionList([oldRecord()])),
        [`POST ${SUBS}`]: () => json(201, { subscription: oldRecord() }),
      },
    })
    const dialog = await openCreate(user)
    await choose(user, dialog)
    await user.click(submit(dialog))
    expect(await screen.findByRole('status', { name: 'The result is unknown' })).toBeInTheDocument()
    expect(requests(calls, 'POST', SUBS)).toHaveLength(1)
    // The revoked record is shown as revoked, as the server lists it.
    const table = await screen.findByRole('table')
    expect(
      within(table).getAllByRole('row')[1]!.querySelector('[data-kind="subscription"]'),
    ).toHaveTextContent('Revoked')
  })
})
