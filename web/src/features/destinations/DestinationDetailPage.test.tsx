import { act, fireEvent, screen, waitFor, within } from '@testing-library/react'
import type { UserEvent } from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import { setCsrfToken } from '../../api/client'
import { DEST_ID, destinationBody, revokedBody, SECRET } from '../../test/destinationFixtures'
import { mountDestinations, requests } from '../../test/destinationHarness'
import type { HarnessSetup } from '../../test/destinationHarness'
import { apiError, json } from '../../test/fetchStub'
import type { Handler } from '../../test/fetchStub'

vi.mock('../../auth/crossTab', () => ({ openAuthChannel: vi.fn() }))

// Stubbed fetch only: these tests prove the UI's behavior for the answers it is given, not the real backend.

const ONE = `/api/v1/organizations/o1/destinations/${DEST_ID}`
const PAGE = `/o/o1/destinations/${DEST_ID}`
const get = (over: Record<string, unknown> = {}) => ({
  [`GET ${ONE}`]: () => json(200, { destination: destinationBody(over) }),
})
const answer =
  (over: Record<string, unknown> = {}, status = 200): Handler =>
  () =>
    json(status, { destination: destinationBody(over) })

function mount(setup: HarnessSetup & { base?: Record<string, unknown> } = {}) {
  const { base, ...rest } = setup
  return mountDestinations(PAGE, { ...rest, routes: { ...get(base), ...rest.routes } })
}

async function ready(user: UserEvent, name: string) {
  const button = await screen.findByRole('button', { name })
  setCsrfToken('tok')
  await user.click(button)
  return screen.getByRole('dialog')
}

describe('destination detail', () => {
  it('shows the details, the sanitized URL and the security information', async () => {
    mount()
    expect(
      await screen.findByRole('heading', { name: 'Billing hook', level: 1 }),
    ).toBeInTheDocument()
    const overview = screen.getByRole('region', { name: 'Overview' })
    expect(within(overview).getByText('https://hooks.example.com/billing')).toBeInTheDocument()
    expect(within(overview).getAllByText('Active').length).toBeGreaterThan(0)
    expect(within(overview).getByText('Created').nextElementSibling).toHaveTextContent(
      '2026-11-20 10:00:00 UTC',
    )
    expect(within(overview).getByText('Last updated').nextElementSibling).toHaveTextContent(
      '2026-11-21 11:30:00 UTC',
    )
    const security = screen.getByRole('region', { name: 'Security' })
    expect(security).toHaveTextContent(/never shown again/)
    expect(security).toHaveTextContent(
      /Pending deliveries use the updated URL on their next attempt/,
    )
    expect(screen.queryByTestId('query-hidden-notice')).not.toBeInTheDocument()
    expect(document.body.textContent).not.toMatch(/rfd_/)
  })

  it('explains hidden query parameters and never tries to show them', async () => {
    mount({ base: { urlHasQuery: true } })
    const notice = await screen.findByTestId('query-hidden-notice')
    expect(notice).toHaveTextContent(/query parameters/)
    expect(notice).toHaveTextContent(/hidden for security/)
    expect(document.body.textContent).not.toContain('?')
  })

  it('shows a safe not-found state with a retry', async () => {
    mountDestinations(PAGE, {
      routes: { [`GET ${ONE}`]: () => apiError(404, 'not_found', 'resource not found') },
    })
    expect(
      await screen.findByRole('heading', { name: 'Destination not found' }),
    ).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Try again' })).toBeInTheDocument()
  })

  it('rejects an answer for another destination instead of showing it', async () => {
    mountDestinations(PAGE, {
      routes: {
        [`GET ${ONE}`]: () =>
          json(200, { destination: destinationBody({ id: 'other', name: 'Intruder' }) }),
      },
    })
    expect(
      await screen.findByRole('heading', { name: 'Could not load destination' }),
    ).toBeInTheDocument()
    expect(screen.queryByText('Intruder')).not.toBeInTheDocument()
  })

  it('shows a revoked destination as permanent, without any action', async () => {
    mount({ base: { status: 'REVOKED', revokedAt: '2026-11-22T09:00:00Z' } })
    expect(await screen.findByText(/Revocation is permanent/)).toBeInTheDocument()
    const actions = screen.getByRole('region', { name: 'Actions' })
    expect(within(actions).queryByRole('button')).not.toBeInTheDocument()
    expect(screen.getByText('Revoked', { selector: 'dt' })).toBeInTheDocument()
  })
})

describe('destination actions by role', () => {
  const matrix: [string, string[], string[]][] = [
    ['OWNER', ['Rename', 'Change URL', 'Pause', 'Rotate signing secret', 'Revoke'], []],
    ['ADMIN', ['Rename', 'Change URL', 'Pause', 'Rotate signing secret', 'Revoke'], []],
    ['DEVELOPER', ['Rename', 'Change URL', 'Pause'], ['Rotate signing secret', 'Revoke']],
    ['VIEWER', [], ['Rename', 'Change URL', 'Pause', 'Rotate signing secret', 'Revoke']],
  ]
  it.each(matrix)('%s sees exactly the permitted actions', async (role, shown, hidden) => {
    mount({ role: role as 'OWNER' })
    await screen.findByRole('heading', { name: 'Billing hook', level: 1 })
    for (const name of shown) expect(screen.getByRole('button', { name })).toBeInTheDocument()
    for (const name of hidden)
      expect(screen.queryByRole('button', { name })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Activate' })).not.toBeInTheDocument()
    if (role === 'VIEWER') expect(screen.getByText(/read-only access/)).toBeInTheDocument()
  })

  it.each(['OWNER', 'ADMIN', 'DEVELOPER'] as const)(
    '%s sees Activate (not Pause) for a paused destination',
    async (role) => {
      mount({ role, base: { status: 'PAUSED' } })
      expect(await screen.findByRole('button', { name: 'Activate' })).toBeInTheDocument()
      expect(screen.queryByRole('button', { name: 'Pause' })).not.toBeInTheDocument()
    },
  )

  it('sends nothing for a VIEWER however the page is used', async () => {
    const { user, calls } = mount({ role: 'VIEWER' })
    await screen.findByRole('heading', { name: 'Billing hook', level: 1 })
    await user.tab()
    await user.keyboard('{Enter}{Escape}')
    expect(calls.some((call) => call.method !== 'GET')).toBe(false)
  })
})

describe('rename and change URL', () => {
  it('renames with only the name and shows the answer at once', async () => {
    const { user, calls } = mount({
      routes: { [`PATCH ${ONE}`]: answer({ name: 'Renamed hook' }) },
    })
    const dialog = await ready(user, 'Rename')
    expect(within(dialog).getByLabelText('Name')).toHaveValue('Billing hook')
    expect(within(dialog).getByLabelText('Name')).toHaveFocus()
    await user.clear(within(dialog).getByLabelText('Name'))
    await user.type(within(dialog).getByLabelText('Name'), 'Renamed hook')
    await user.click(within(dialog).getByRole('button', { name: 'Save name' }))
    expect(
      await screen.findByRole('heading', { name: 'Renamed hook', level: 1 }),
    ).toBeInTheDocument()
    expect(requests(calls, 'PATCH', ONE)[0]!.body).toEqual({ name: 'Renamed hook' })
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    expect(screen.getByRole('heading', { name: 'Renamed hook', level: 1 })).toHaveFocus()
    expect(requests(calls, 'GET', ONE)).toHaveLength(1)
  })

  it('validates an empty name and shows a taken name from the server', async () => {
    const { user } = mount({
      routes: {
        [`PATCH ${ONE}`]: () =>
          apiError(409, 'destination_name_taken', 'a destination with this name already exists'),
      },
    })
    const dialog = await ready(user, 'Rename')
    await user.clear(within(dialog).getByLabelText('Name'))
    await user.click(within(dialog).getByRole('button', { name: 'Save name' }))
    expect(within(dialog).getByText('Enter a name.')).toBeInTheDocument()
    await user.type(within(dialog).getByLabelText('Name'), 'Taken')
    await user.click(within(dialog).getByRole('button', { name: 'Save name' }))
    expect(
      await within(dialog).findByText('a destination with this name already exists'),
    ).toBeInTheDocument()
    expect(screen.getByRole('dialog')).toBeInTheDocument()
  })

  it('changes the URL with an empty field, never prefilled from the hidden-query display', async () => {
    const { user, calls } = mount({
      base: { urlHasQuery: true },
      routes: { [`PATCH ${ONE}`]: answer({ url: 'https://new.example.com/h', urlHasQuery: true }) },
    })
    const dialog = await ready(user, 'Change URL')
    expect(within(dialog).getByLabelText('New URL')).toHaveValue('')
    expect(dialog).toHaveTextContent(/Pending deliveries use the new URL on their next attempt/)
    expect(dialog).toHaveTextContent(/query parameters are hidden/)
    await user.click(within(dialog).getByRole('button', { name: 'Save URL' }))
    expect(within(dialog).getByText('Enter the destination URL.')).toBeInTheDocument()
    expect(requests(calls, 'PATCH', ONE)).toHaveLength(0)
    await user.type(within(dialog).getByLabelText('New URL'), 'https://new.example.com/h?token=abc')
    await user.click(within(dialog).getByRole('button', { name: 'Save URL' }))
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    expect(requests(calls, 'PATCH', ONE)[0]!.body).toEqual({
      url: 'https://new.example.com/h?token=abc',
    })
    expect(await screen.findAllByText('https://new.example.com/h')).not.toHaveLength(0)
    expect(document.body.textContent).not.toContain('token=abc')
  })

  it('shows a backend URL rejection inline', async () => {
    const { user } = mount({
      routes: {
        [`PATCH ${ONE}`]: () =>
          apiError(400, 'validation_failed', 'destination URL is not allowed: blocked_ip_address'),
      },
    })
    const dialog = await ready(user, 'Change URL')
    await user.type(within(dialog).getByLabelText('New URL'), 'https://10.0.0.1/x')
    await user.click(within(dialog).getByRole('button', { name: 'Save URL' }))
    expect(
      await within(dialog).findByText('destination URL is not allowed: blocked_ip_address'),
    ).toBeInTheDocument()
    expect(within(dialog).getByLabelText('New URL')).toBeInvalid()
  })
})

describe('pause and activate', () => {
  it('sends nothing until the pause is confirmed, and Cancel sends nothing', async () => {
    const { user, calls } = mount({
      routes: { [`POST ${ONE}/pause`]: answer({ status: 'PAUSED' }) },
    })
    const dialog = await ready(user, 'Pause')
    expect(dialog).toHaveAccessibleName('Pause this destination?')
    expect(dialog).toHaveAccessibleDescription(/stops new delivery attempts.*in flight finish/)
    expect(requests(calls, 'POST', `${ONE}/pause`)).toHaveLength(0)
    await user.click(within(dialog).getByRole('button', { name: 'Cancel' }))
    expect(requests(calls, 'POST', `${ONE}/pause`)).toHaveLength(0)
    expect(screen.getByRole('button', { name: 'Pause' })).toHaveFocus()
  })

  it('pauses, then offers Activate, which activates again', async () => {
    const { user, calls } = mount({
      routes: {
        [`POST ${ONE}/pause`]: answer({ status: 'PAUSED' }),
        [`POST ${ONE}/activate`]: answer({ status: 'ACTIVE' }),
      },
    })
    let dialog = await ready(user, 'Pause')
    await user.click(within(dialog).getByRole('button', { name: 'Pause destination' }))
    expect(await screen.findByRole('button', { name: 'Activate' })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Pause' })).not.toBeInTheDocument()
    expect(screen.getAllByText('Paused').length).toBeGreaterThan(0)
    expect(screen.getByRole('heading', { name: 'Billing hook', level: 1 })).toHaveFocus()
    dialog = await ready(user, 'Activate')
    expect(dialog).toHaveAccessibleDescription(/becomes active again/)
    await user.click(within(dialog).getByRole('button', { name: 'Activate destination' }))
    expect(await screen.findByRole('button', { name: 'Pause' })).toBeInTheDocument()
    expect(requests(calls, 'POST', `${ONE}/pause`)).toHaveLength(1)
    expect(requests(calls, 'POST', `${ONE}/activate`)).toHaveLength(1)
  })

  it('sends one request for a double submit in the same tick', async () => {
    const { user, calls } = mount({
      routes: { [`POST ${ONE}/pause`]: answer({ status: 'PAUSED' }) },
    })
    const dialog = await ready(user, 'Pause')
    const form = dialog.querySelector('form') as HTMLFormElement
    act(() => {
      fireEvent.submit(form)
      fireEvent.submit(form)
    })
    await screen.findByRole('button', { name: 'Activate' })
    expect(requests(calls, 'POST', `${ONE}/pause`)).toHaveLength(1)
  })

  it('disables the controls and ignores Escape while a request runs', async () => {
    let release: ((response: Response) => void) | undefined
    const { user } = mount({
      routes: {
        [`POST ${ONE}/pause`]: () =>
          new Promise<Response>((resolve) => {
            release = resolve
          }),
      },
    })
    const dialog = await ready(user, 'Pause')
    await user.click(within(dialog).getByRole('button', { name: 'Pause destination' }))
    expect(within(dialog).getByRole('button', { name: 'Pause destination' })).toBeDisabled()
    expect(within(dialog).getByRole('button', { name: 'Cancel' })).toBeDisabled()
    await user.keyboard('{Escape}')
    expect(screen.getByRole('dialog')).toBeInTheDocument()
    await act(async () =>
      release?.(json(200, { destination: destinationBody({ status: 'PAUSED' }) })),
    )
    expect(await screen.findByRole('button', { name: 'Activate' })).toBeInTheDocument()
  })
})

describe('rotate signing secret', () => {
  const rotate = (): Handler => () =>
    json(200, { destination: destinationBody({ signingSecret: SECRET }) })

  it('explains the effect, then shows the new secret exactly once', async () => {
    const setItem = vi.spyOn(Storage.prototype, 'setItem')
    const { user, calls } = mount({ routes: { [`POST ${ONE}/rotate-secret`]: rotate() } })
    const dialog = await ready(user, 'Rotate signing secret')
    expect(dialog).toHaveAccessibleDescription(/New delivery attempts use the new secret/)
    expect(requests(calls, 'POST', `${ONE}/rotate-secret`)).toHaveLength(0)
    await user.click(within(dialog).getByRole('button', { name: 'Rotate secret' }))
    const secretDialog = await screen.findByRole('dialog', { name: 'New signing secret' })
    expect(within(secretDialog).getByTestId('signing-secret')).toHaveTextContent(SECRET)
    expect(secretDialog).toHaveTextContent(/cannot be retrieved later/)
    await user.keyboard('{Escape}')
    expect(screen.getByRole('dialog', { name: 'New signing secret' })).toBeInTheDocument()
    expect(within(secretDialog).getByRole('button', { name: 'Done' })).toBeDisabled()
    await user.click(within(secretDialog).getByRole('checkbox'))
    await user.click(within(secretDialog).getByRole('button', { name: 'Done' }))
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    expect(document.body.textContent).not.toContain(SECRET)
    expect(setItem).not.toHaveBeenCalled()
    expect(JSON.stringify({ ...window.localStorage, ...window.sessionStorage })).not.toContain(
      SECRET,
    )
    expect(document.cookie).not.toContain(SECRET)
    expect(window.location.href).not.toContain(SECRET)
    expect(document.querySelector('.toast-region')!.textContent).not.toContain(SECRET)
    expect(requests(calls, 'GET', ONE)).toHaveLength(1)
  })

  it('discards the secret when the organization changes', async () => {
    const { user } = mount({ routes: { [`POST ${ONE}/rotate-secret`]: rotate() } })
    const dialog = await ready(user, 'Rotate signing secret')
    await user.click(within(dialog).getByRole('button', { name: 'Rotate secret' }))
    await screen.findByRole('dialog', { name: 'New signing secret' })
    await user.selectOptions(screen.getByLabelText('Organization'), 'o2')
    await waitFor(() => expect(screen.getByTestId('location')).toHaveTextContent('/o/o2'))
    expect(document.body.textContent).not.toContain(SECRET)
  })

  const lost: [string, Handler][] = [
    [
      'a lost connection',
      () => {
        throw new TypeError('Failed to fetch')
      },
    ],
    ['a proxy 504', () => apiError(504, 'gateway_timeout', 'timeout')],
    ['a 200 without the secret', () => json(200, { destination: destinationBody() })],
    ['a 200 that is not JSON', () => new Response('<html>', { status: 200 })],
  ]
  it.each(lost)(
    'treats %s as unknown: one POST, one read, no secret, no repeat',
    async (_name, post) => {
      const { user, calls } = mount({ routes: { [`POST ${ONE}/rotate-secret`]: post } })
      const dialog = await ready(user, 'Rotate signing secret')
      await user.click(within(dialog).getByRole('button', { name: 'Rotate secret' }))
      const notice = await screen.findByRole('status', { name: 'The result is unknown' })
      expect(notice).toHaveTextContent(/may or may not have been rotated/)
      expect(notice).toHaveTextContent(/rotate again/)
      await waitFor(() => expect(requests(calls, 'GET', ONE)).toHaveLength(2))
      expect(requests(calls, 'POST', `${ONE}/rotate-secret`)).toHaveLength(1)
      expect(screen.queryByTestId('signing-secret')).not.toBeInTheDocument()
    },
  )
})

describe('revoke', () => {
  it('requires the explicit acknowledgment, explains the consequences, and sends nothing before confirming', async () => {
    const { user, calls } = mount({
      routes: {
        [`DELETE ${ONE}`]: answer({ status: 'REVOKED', revokedAt: '2026-11-22T09:00:00Z' }),
      },
    })
    const dialog = await ready(user, 'Revoke')
    expect(dialog).toHaveAccessibleName('Revoke this destination?')
    expect(dialog).toHaveTextContent(/permanent/)
    expect(dialog).toHaveTextContent(/cannot be reactivated/)
    expect(dialog).toHaveTextContent(/pending deliveries are cancelled/i)
    expect(dialog).toHaveTextContent(/delivery history stays available/)
    const confirm = within(dialog).getByRole('button', { name: 'Revoke destination' })
    expect(confirm).toBeDisabled()
    fireEvent.submit(dialog.querySelector('form') as HTMLFormElement)
    expect(requests(calls, 'DELETE', ONE)).toHaveLength(0)
    await user.click(within(dialog).getByRole('checkbox'))
    expect(confirm).toBeEnabled()
    expect(requests(calls, 'DELETE', ONE)).toHaveLength(0)
    await user.click(confirm)
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    expect(requests(calls, 'DELETE', ONE)).toHaveLength(1)
    expect(screen.getByText(/Revocation is permanent/)).toBeInTheDocument()
    const actions = screen.getByRole('region', { name: 'Actions' })
    expect(within(actions).queryByRole('button')).not.toBeInTheDocument()
  })

  it('sends one DELETE for a double submit in the same tick', async () => {
    const { user, calls } = mount({
      routes: {
        [`DELETE ${ONE}`]: answer({ status: 'REVOKED', revokedAt: '2026-11-22T09:00:00Z' }),
      },
    })
    const dialog = await ready(user, 'Revoke')
    await user.click(within(dialog).getByRole('checkbox'))
    const form = dialog.querySelector('form') as HTMLFormElement
    act(() => {
      fireEvent.submit(form)
      fireEvent.submit(form)
    })
    await waitFor(() => expect(screen.getByText(/Revocation is permanent/)).toBeInTheDocument())
    expect(requests(calls, 'DELETE', ONE)).toHaveLength(1)
  })

  it('does nothing when the dialog is cancelled', async () => {
    const { user, calls } = mount()
    const dialog = await ready(user, 'Revoke')
    await user.click(within(dialog).getByRole('checkbox'))
    await user.click(within(dialog).getByRole('button', { name: 'Cancel' }))
    expect(calls.some((call) => call.method === 'DELETE')).toBe(false)
    expect(screen.getByRole('button', { name: 'Revoke' })).toHaveFocus()
  })
})

describe('authorization and conflicts of a mutation', () => {
  it('hides every control after a 403 and says nothing was changed', async () => {
    const { user } = mount({
      routes: { [`POST ${ONE}/pause`]: () => apiError(403, 'forbidden', 'forbidden') },
    })
    const dialog = await ready(user, 'Pause')
    await user.click(within(dialog).getByRole('button', { name: 'Pause destination' }))
    expect(await screen.findByRole('alert', { name: 'Not permitted' })).toHaveTextContent(
      'Nothing was changed',
    )
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Pause' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Rename' })).not.toBeInTheDocument()
    expect(screen.getByText(/can no longer change/)).toBeInTheDocument()
  })

  it('shows a reload message after csrf_invalid and keeps the dialog open', async () => {
    const { user } = mount({
      routes: { [`POST ${ONE}/pause`]: () => apiError(403, 'csrf_invalid', 'bad token') },
    })
    const dialog = await ready(user, 'Pause')
    await user.click(within(dialog).getByRole('button', { name: 'Pause destination' }))
    expect(await within(dialog).findByRole('alert')).toHaveTextContent('Reload the page')
    expect(screen.getByRole('dialog')).toBeInTheDocument()
  })

  it('reads the destination again after destination_revoked and explains it', async () => {
    let reads = 0
    const { user, calls } = mount({
      routes: {
        [`GET ${ONE}`]: () => {
          reads += 1
          return json(200, {
            destination: reads === 1 ? destinationBody() : revokedBody(),
          })
        },
        [`POST ${ONE}/pause`]: () =>
          apiError(409, 'destination_revoked', 'the destination is revoked'),
      },
    })
    const dialog = await ready(user, 'Pause')
    await user.click(within(dialog).getByRole('button', { name: 'Pause destination' }))
    expect(
      await screen.findByRole('alert', { name: 'The destination is revoked' }),
    ).toBeInTheDocument()
    await waitFor(() => expect(requests(calls, 'GET', ONE)).toHaveLength(2))
    expect(await screen.findByText(/Revocation is permanent/)).toBeInTheDocument()
  })

  it('shows a 404 after a mutation as not found and reads once', async () => {
    const { user, calls } = mount({
      routes: { [`DELETE ${ONE}`]: () => apiError(404, 'not_found', 'resource not found') },
    })
    const dialog = await ready(user, 'Revoke')
    await user.click(within(dialog).getByRole('checkbox'))
    await user.click(within(dialog).getByRole('button', { name: 'Revoke destination' }))
    expect(await screen.findByRole('alert', { name: 'Destination not found' })).toBeInTheDocument()
    expect(requests(calls, 'DELETE', ONE)).toHaveLength(1)
  })

  const unknown: [string, string, Handler][] = [
    [
      'pause',
      'Pause',
      () => {
        throw new TypeError('Failed to fetch')
      },
    ],
    ['pause (unreadable 200)', 'Pause', () => json(200, { destination: destinationBody() })],
    ['pause (proxy 502)', 'Pause', () => apiError(502, 'bad_gateway', 'x')],
  ]
  it.each(unknown)(
    'treats an unknown %s result as ambiguous: one POST, one read, no repeat',
    async (_name, action, post) => {
      const { user, calls } = mount({ routes: { [`POST ${ONE}/pause`]: post } })
      const dialog = await ready(user, action)
      await user.click(within(dialog).getByRole('button', { name: 'Pause destination' }))
      const notice = await screen.findByRole('status', { name: 'The result is unknown' })
      expect(notice).toHaveTextContent(/may or may not have been applied/)
      expect(notice).toHaveTextContent(/not repeated/)
      await waitFor(() => expect(requests(calls, 'GET', ONE)).toHaveLength(2))
      expect(requests(calls, 'POST', `${ONE}/pause`)).toHaveLength(1)
      expect(screen.getByRole('heading', { name: 'Billing hook', level: 1 })).toHaveFocus()
    },
  )

  it('stops and starts no read when the organization changes during a request', async () => {
    const { user, calls } = mount({
      routes: {
        [`POST ${ONE}/pause`]: (call) =>
          new Promise<Response>((_resolve, reject) => {
            call.signal?.addEventListener('abort', () =>
              reject(new DOMException('aborted', 'AbortError')),
            )
          }),
      },
    })
    const dialog = await ready(user, 'Pause')
    await user.click(within(dialog).getByRole('button', { name: 'Pause destination' }))
    await user.selectOptions(screen.getByLabelText('Organization'), 'o2')
    await waitFor(() => expect(screen.getByTestId('location')).toHaveTextContent('/o/o2'))
    expect(requests(calls, 'POST', `${ONE}/pause`)[0]!.signal?.aborted).toBe(true)
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 20))
    })
    expect(requests(calls, 'GET', ONE)).toHaveLength(1)
    expect(screen.queryByText('Billing hook')).not.toBeInTheDocument()
  })
})
