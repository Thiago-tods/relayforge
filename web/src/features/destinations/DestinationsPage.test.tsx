import { act, fireEvent, screen, waitFor, within } from '@testing-library/react'
import type { UserEvent } from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import { setCsrfToken } from '../../api/client'
import {
  DEST_ID,
  DEST_ID_2,
  destinationBody,
  destinationList,
  revokedBody,
  SECRET,
} from '../../test/destinationFixtures'
import { mountDestinations, requests } from '../../test/destinationHarness'
import { apiError, json } from '../../test/fetchStub'

vi.mock('../../auth/crossTab', () => ({ openAuthChannel: vi.fn() }))

// Stubbed fetch only: these tests prove the UI's behavior for the answers it is given, not the real backend.

const LIST = '/api/v1/organizations/o1/destinations'
const PAGE = '/o/o1/destinations'

const rows = [
  destinationBody(),
  destinationBody({
    id: DEST_ID_2,
    name: 'Old hook',
    status: 'PAUSED',
    url: 'https://old.example.com/in',
    urlHasQuery: true,
  }),
]

async function openCreate(user: UserEvent) {
  setCsrfToken('tok')
  await user.click(await screen.findByRole('button', { name: 'Create destination' }))
  return screen.getByRole('dialog')
}

async function fillCreate(
  user: UserEvent,
  name = 'New hook',
  url = 'https://new.example.com/h?token=abc',
) {
  const dialog = screen.getByRole('dialog')
  await user.type(within(dialog).getByLabelText('Name'), name)
  await user.type(within(dialog).getByLabelText('HTTPS URL'), url)
  return dialog
}

describe('destinations list', () => {
  it('shows a loading state, then the destinations with sanitized URLs, status and dates', async () => {
    const { calls } = mountDestinations(PAGE, {
      routes: { [`GET ${LIST}`]: () => json(200, destinationList(rows, 2)) },
    })
    expect(await screen.findByText('Loading destinations…')).toBeInTheDocument()
    const table = await screen.findByRole('table')
    expect(within(table).getByRole('link', { name: 'Billing hook' })).toHaveAttribute(
      'href',
      `/o/o1/destinations/${DEST_ID}`,
    )
    expect(within(table).getByText('https://hooks.example.com/billing')).toBeInTheDocument()
    expect(within(table).getByText('Active')).toBeInTheDocument()
    expect(within(table).getByText('Paused')).toBeInTheDocument()
    expect(within(table).getAllByText('2026-11-20 10:00:00 UTC').length).toBe(2)
    expect(within(table).getByText('(query hidden)')).toBeInTheDocument()
    expect(screen.getByRole('heading', { name: 'Destinations', level: 1 })).toBeInTheDocument()
    expect(screen.getByText(/2 destinations/)).toBeInTheDocument()
    const request = requests(calls, 'GET', LIST)[0]!
    expect(new URL(request.path, 'http://x').searchParams.get('limit')).toBe('25')
    expect(new URL(request.path, 'http://x').searchParams.get('offset')).toBe('0')
  })

  it('shows an empty state with a create action for someone who can create', async () => {
    mountDestinations(PAGE, { routes: { [`GET ${LIST}`]: () => json(200, destinationList([])) } })
    expect(await screen.findByText('No destinations yet.')).toBeInTheDocument()
    expect(screen.getAllByRole('button', { name: 'Create destination' }).length).toBeGreaterThan(0)
  })

  it('shows an empty state without a create action for a VIEWER', async () => {
    mountDestinations(PAGE, {
      role: 'VIEWER',
      routes: { [`GET ${LIST}`]: () => json(200, destinationList([])) },
    })
    expect(await screen.findByText('No destinations yet.')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Create destination' })).not.toBeInTheDocument()
  })

  it('shows an error state with a retry that loads again', async () => {
    let answers = 0
    const { user, calls } = mountDestinations(PAGE, {
      routes: {
        [`GET ${LIST}`]: () => {
          answers += 1
          return answers === 1
            ? apiError(500, 'internal_error', 'boom')
            : json(200, destinationList(rows, 2))
        },
      },
    })
    expect(
      await screen.findByRole('heading', { name: 'Could not load destinations' }),
    ).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: 'Try again' }))
    expect(await screen.findByRole('table')).toBeInTheDocument()
    expect(requests(calls, 'GET', LIST)).toHaveLength(2)
  })

  it('treats a malformed list answer as an error and shows no rows', async () => {
    mountDestinations(PAGE, {
      routes: {
        [`GET ${LIST}`]: () =>
          json(200, destinationList([destinationBody({ signingSecret: SECRET })])),
      },
    })
    expect(
      await screen.findByRole('heading', { name: 'Could not load destinations' }),
    ).toBeInTheDocument()
    expect(screen.queryByText(SECRET)).not.toBeInTheDocument()
    expect(screen.queryByRole('table')).not.toBeInTheDocument()
  })

  it('filters by status through the query and the URL, and All removes the filter', async () => {
    const { user, calls } = mountDestinations(PAGE, {
      routes: { [`GET ${LIST}`]: () => json(200, destinationList(rows, 2)) },
    })
    await screen.findByRole('table')
    await user.selectOptions(screen.getByLabelText('Status'), 'PAUSED')
    await waitFor(() => expect(screen.getByTestId('search')).toHaveTextContent('status=PAUSED'))
    await waitFor(() => {
      const last = requests(calls, 'GET', LIST).at(-1)!
      expect(new URL(last.path, 'http://x').searchParams.get('status')).toBe('PAUSED')
    })
    await user.selectOptions(screen.getByLabelText('Status'), 'All')
    await waitFor(() => expect(screen.getByTestId('search')).toHaveTextContent(/^$/))
    await waitFor(() => {
      const last = requests(calls, 'GET', LIST).at(-1)!
      expect(new URL(last.path, 'http://x').searchParams.has('status')).toBe(false)
    })
  })

  it('shows an empty message for a filter without matches', async () => {
    mountDestinations(`${PAGE}?status=REVOKED`, {
      routes: { [`GET ${LIST}`]: () => json(200, destinationList([])) },
    })
    expect(await screen.findByText('No revoked destinations.')).toBeInTheDocument()
  })

  it('ignores an unknown status or a malformed offset in the URL', async () => {
    const { calls } = mountDestinations(`${PAGE}?status=DELETED&offset=7`, {
      routes: { [`GET ${LIST}`]: () => json(200, destinationList(rows, 2)) },
    })
    await screen.findByRole('table')
    const url = new URL(requests(calls, 'GET', LIST)[0]!.path, 'http://x')
    expect(url.searchParams.has('status')).toBe(false)
    expect(url.searchParams.get('offset')).toBe('0')
  })

  it('pages with the existing offset pagination and the exact total', async () => {
    const { user, calls } = mountDestinations(PAGE, {
      routes: {
        [`GET ${LIST}`]: (call) => {
          const offset = Number(new URL(call.path, 'http://x').searchParams.get('offset'))
          return json(
            200,
            destinationList(
              offset === 0 ? rows : [destinationBody({ id: 'x3', name: 'Third' })],
              26,
              offset,
            ),
          )
        },
      },
    })
    await screen.findByRole('table')
    expect(screen.getByRole('button', { name: 'Previous' })).toBeDisabled()
    await user.click(screen.getByRole('button', { name: 'Next' }))
    expect(await screen.findByRole('link', { name: 'Third' })).toBeInTheDocument()
    const last = requests(calls, 'GET', LIST).at(-1)!
    expect(new URL(last.path, 'http://x').searchParams.get('offset')).toBe('25')
    expect(screen.getByRole('button', { name: 'Next' })).toBeDisabled()
    expect(screen.getByText('Page 2 (last page)')).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: 'Previous' }))
    expect(await screen.findByRole('link', { name: 'Billing hook' })).toBeInTheDocument()
  })

  it('never shows destinations of the previous organization after a switch', async () => {
    let release: ((response: Response) => void) | undefined
    const { user, calls } = mountDestinations(PAGE, {
      routes: {
        [`GET ${LIST}`]: () =>
          new Promise<Response>((resolve) => {
            release = resolve
          }),
      },
    })
    await screen.findByText('Loading destinations…')
    await user.selectOptions(screen.getByLabelText('Organization'), 'o2')
    await waitFor(() => expect(screen.getByTestId('location')).toHaveTextContent('/o/o2'))
    expect(requests(calls, 'GET', LIST)[0]!.signal?.aborted).toBe(true)
    await act(async () => release?.(json(200, destinationList(rows, 2))))
    expect(screen.queryByText('Billing hook')).not.toBeInTheDocument()
  })
})

describe('destinations navigation', () => {
  it.each(['OWNER', 'ADMIN', 'DEVELOPER', 'VIEWER'] as const)(
    '%s sees the Configuration group with a Destinations link and a non-link Subscriptions label',
    async (role) => {
      mountDestinations(PAGE, {
        role,
        routes: { [`GET ${LIST}`]: () => json(200, destinationList([])) },
      })
      const nav = await screen.findByRole('navigation', { name: 'Main navigation' })
      const group = within(nav).getByRole('group', { name: 'Configuration' })
      expect(within(group).getByRole('link', { name: 'Destinations' })).toHaveAttribute(
        'href',
        '/o/o1/destinations',
      )
      expect(within(group).getByText(/Subscriptions/)).toBeInTheDocument()
      expect(within(group).queryByRole('link', { name: /Subscriptions/ })).not.toBeInTheDocument()
    },
  )
})

describe('create destination', () => {
  it.each([
    ['OWNER', true],
    ['ADMIN', true],
    ['DEVELOPER', true],
    ['VIEWER', false],
  ] as const)('%s create action visible: %s', async (role, visible) => {
    mountDestinations(PAGE, {
      role,
      routes: { [`GET ${LIST}`]: () => json(200, destinationList(rows, 2)) },
    })
    await screen.findByRole('table')
    expect(screen.queryByRole('button', { name: 'Create destination' }) !== null).toBe(visible)
  })

  it('validates the form before sending anything', async () => {
    const { user, calls } = mountDestinations(PAGE, {
      routes: { [`GET ${LIST}`]: () => json(200, destinationList(rows, 2)) },
    })
    await screen.findByRole('table')
    const dialog = await openCreate(user)
    await user.click(within(dialog).getByRole('button', { name: 'Create destination' }))
    expect(within(dialog).getByText('Enter a name.')).toBeInTheDocument()
    expect(within(dialog).getByText('Enter the destination URL.')).toBeInTheDocument()
    expect(within(dialog).getByLabelText('Name')).toHaveFocus()
    fireEvent.change(within(dialog).getByLabelText('Name'), { target: { value: 'n'.repeat(101) } })
    fireEvent.change(within(dialog).getByLabelText('HTTPS URL'), {
      target: { value: 'https://x.example' },
    })
    await user.click(within(dialog).getByRole('button', { name: 'Create destination' }))
    expect(within(dialog).getByText('Use at most 100 characters.')).toBeInTheDocument()
    expect(requests(calls, 'POST', LIST)).toHaveLength(0)
  })

  it('counts 100 astral characters as 100 and sends the trimmed values', async () => {
    const { user, calls } = mountDestinations(PAGE, {
      routes: {
        [`GET ${LIST}`]: () => json(200, destinationList(rows, 2)),
        [`POST ${LIST}`]: () =>
          json(201, { destination: destinationBody({ signingSecret: SECRET }) }),
      },
    })
    await screen.findByRole('table')
    const dialog = await openCreate(user)
    fireEvent.change(within(dialog).getByLabelText('Name'), {
      target: { value: `  ${'😀'.repeat(100)}  ` },
    })
    fireEvent.change(within(dialog).getByLabelText('HTTPS URL'), {
      target: { value: '  https://x.example/h  ' },
    })
    await user.click(within(dialog).getByRole('button', { name: 'Create destination' }))
    await screen.findByText('Destination created')
    expect(requests(calls, 'POST', LIST)[0]!.body).toEqual({
      name: '😀'.repeat(100),
      url: 'https://x.example/h',
    })
  })

  it('creates, shows the one-time secret, requires acknowledgment, and then forgets it', async () => {
    const setItem = vi.spyOn(Storage.prototype, 'setItem')
    const { user, calls } = mountDestinations(PAGE, {
      routes: {
        [`GET ${LIST}`]: () => json(200, destinationList(rows, 2)),
        [`POST ${LIST}`]: () =>
          json(201, { destination: destinationBody({ signingSecret: SECRET }) }),
      },
    })
    await screen.findByRole('table')
    await openCreate(user)
    await fillCreate(user)
    await user.click(
      within(screen.getByRole('dialog')).getByRole('button', { name: 'Create destination' }),
    )

    const secretDialog = await screen.findByRole('dialog', { name: 'Destination created' })
    expect(within(secretDialog).getByTestId('signing-secret')).toHaveTextContent(SECRET)
    expect(secretDialog).toHaveAccessibleDescription(/cannot be retrieved later/)
    expect(requests(calls, 'POST', LIST)[0]!.body).toEqual({
      name: 'New hook',
      url: 'https://new.example.com/h?token=abc',
    })
    // The list was refreshed after the creation.
    await waitFor(() => expect(requests(calls, 'GET', LIST)).toHaveLength(2))

    // It cannot be dismissed: Escape and the native cancel event do nothing, and Done needs the acknowledgment.
    await user.keyboard('{Escape}')
    fireEvent(secretDialog, new Event('cancel', { cancelable: true }))
    expect(screen.getByRole('dialog', { name: 'Destination created' })).toBeInTheDocument()
    expect(within(secretDialog).getByRole('button', { name: 'Done' })).toBeDisabled()

    // Copy never puts the secret into a message, a label or storage.
    await user.click(within(secretDialog).getByRole('button', { name: 'Copy secret' }))
    expect(
      await within(secretDialog).findByText('Secret copied to the clipboard.'),
    ).toBeInTheDocument()
    expect(await navigator.clipboard.readText()).toBe(SECRET)
    expect(
      within(secretDialog).getByRole('button', { name: 'Copy secret' }).getAttribute('aria-label'),
    ).toBeNull()

    expect(setItem).not.toHaveBeenCalled()
    expect(JSON.stringify({ ...window.localStorage })).not.toContain(SECRET)
    expect(JSON.stringify({ ...window.sessionStorage })).not.toContain(SECRET)
    expect(document.cookie).not.toContain(SECRET)
    expect(window.location.href).not.toContain(SECRET)
    expect(screen.getByTestId('location').textContent).not.toContain(SECRET)
    expect(screen.getByTestId('search').textContent).not.toContain(SECRET)

    await user.click(within(secretDialog).getByRole('checkbox'))
    await user.click(within(secretDialog).getByRole('button', { name: 'Done' }))
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    expect(document.body.textContent).not.toContain(SECRET)
    expect(screen.getByRole('heading', { name: 'Destinations', level: 1 })).toHaveFocus()
  })

  it('never shows the secret in a toast', async () => {
    const { user } = mountDestinations(PAGE, {
      routes: {
        [`GET ${LIST}`]: () => json(200, destinationList(rows, 2)),
        [`POST ${LIST}`]: () =>
          json(201, { destination: destinationBody({ signingSecret: SECRET }) }),
      },
    })
    await screen.findByRole('table')
    await openCreate(user)
    await fillCreate(user)
    await user.click(
      within(screen.getByRole('dialog')).getByRole('button', { name: 'Create destination' }),
    )
    await screen.findByRole('dialog', { name: 'Destination created' })
    const region = document.querySelector('.toast-region')!
    expect(region.textContent).toContain('Destination created.')
    expect(region.textContent).not.toContain(SECRET)
  })

  it('discards the secret when the organization changes', async () => {
    const { user } = mountDestinations(PAGE, {
      routes: {
        [`GET ${LIST}`]: () => json(200, destinationList(rows, 2)),
        [`POST ${LIST}`]: () =>
          json(201, { destination: destinationBody({ signingSecret: SECRET }) }),
      },
    })
    await screen.findByRole('table')
    await openCreate(user)
    await fillCreate(user)
    await user.click(
      within(screen.getByRole('dialog')).getByRole('button', { name: 'Create destination' }),
    )
    await screen.findByRole('dialog', { name: 'Destination created' })
    await user.selectOptions(screen.getByLabelText('Organization'), 'o2')
    await waitFor(() => expect(screen.getByTestId('location')).toHaveTextContent('/o/o2'))
    expect(document.body.textContent).not.toContain(SECRET)
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
  })

  it('shows a taken name on the name field and keeps the dialog open', async () => {
    const { user, calls } = mountDestinations(PAGE, {
      routes: {
        [`GET ${LIST}`]: () => json(200, destinationList(rows, 2)),
        [`POST ${LIST}`]: () =>
          apiError(409, 'destination_name_taken', 'a destination with this name already exists'),
      },
    })
    await screen.findByRole('table')
    await openCreate(user)
    const dialog = await fillCreate(user)
    await user.click(within(dialog).getByRole('button', { name: 'Create destination' }))
    expect(
      await within(dialog).findByText('a destination with this name already exists'),
    ).toBeInTheDocument()
    expect(within(dialog).getByLabelText('Name')).toHaveFocus()
    expect(within(dialog).getByLabelText('Name')).toBeInvalid()
    expect(requests(calls, 'POST', LIST)).toHaveLength(1)
  })

  it('shows the backend URL rejection on the URL field without echoing anything else', async () => {
    const { user } = mountDestinations(PAGE, {
      routes: {
        [`GET ${LIST}`]: () => json(200, destinationList(rows, 2)),
        [`POST ${LIST}`]: () =>
          apiError(400, 'validation_failed', 'destination URL is not allowed: scheme_not_allowed'),
      },
    })
    await screen.findByRole('table')
    await openCreate(user)
    const dialog = await fillCreate(user, 'Plain', 'http://insecure.example/h')
    await user.click(within(dialog).getByRole('button', { name: 'Create destination' }))
    expect(
      await within(dialog).findByText('destination URL is not allowed: scheme_not_allowed'),
    ).toBeInTheDocument()
    expect(within(dialog).getByLabelText('HTTPS URL')).toBeInvalid()
  })

  it('hides the create action after a 403 and says nothing was created', async () => {
    const { user } = mountDestinations(PAGE, {
      routes: {
        [`GET ${LIST}`]: () => json(200, destinationList(rows, 2)),
        [`POST ${LIST}`]: () => apiError(403, 'forbidden', 'forbidden'),
      },
    })
    await screen.findByRole('table')
    await openCreate(user)
    await fillCreate(user)
    await user.click(
      within(screen.getByRole('dialog')).getByRole('button', { name: 'Create destination' }),
    )
    expect(await screen.findByRole('alert')).toHaveTextContent('Nothing was created')
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Create destination' })).not.toBeInTheDocument()
  })

  const ambiguous: [string, () => Response][] = [
    [
      'a lost connection',
      () => {
        throw new TypeError('Failed to fetch')
      },
    ],
    ['a proxy 502', () => apiError(502, 'bad_gateway', 'bad gateway')],
    ['a 201 that is not JSON', () => new Response('<html>', { status: 201 })],
    ['a 201 without the secret', () => json(201, { destination: destinationBody() })],
  ]
  it.each(ambiguous)(
    'treats %s as unknown: one POST, a refreshed list, no secret, no repeat',
    async (_name, post) => {
      const { user, calls } = mountDestinations(PAGE, {
        routes: {
          [`GET ${LIST}`]: () => json(200, destinationList(rows, 2)),
          [`POST ${LIST}`]: post,
        },
      })
      await screen.findByRole('table')
      await openCreate(user)
      await fillCreate(user)
      await user.click(
        within(screen.getByRole('dialog')).getByRole('button', { name: 'Create destination' }),
      )
      const notice = await screen.findByRole('status', { name: 'The result is unknown' })
      expect(notice).toHaveTextContent(/may or may not have been created/)
      expect(notice).toHaveTextContent(/rotate it/)
      await waitFor(() => expect(requests(calls, 'GET', LIST)).toHaveLength(2))
      expect(requests(calls, 'POST', LIST)).toHaveLength(1)
      expect(screen.queryByTestId('signing-secret')).not.toBeInTheDocument()
      expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    },
  )

  it('sends one POST for a double submit in the same tick', async () => {
    const { user, calls } = mountDestinations(PAGE, {
      routes: {
        [`GET ${LIST}`]: () => json(200, destinationList(rows, 2)),
        [`POST ${LIST}`]: () =>
          json(201, { destination: destinationBody({ signingSecret: SECRET }) }),
      },
    })
    await screen.findByRole('table')
    await openCreate(user)
    const dialog = await fillCreate(user)
    const form = dialog.querySelector('form') as HTMLFormElement
    act(() => {
      fireEvent.submit(form)
      fireEvent.submit(form)
    })
    await screen.findByRole('dialog', { name: 'Destination created' })
    expect(requests(calls, 'POST', LIST)).toHaveLength(1)
  })

  it('shows revoked destinations without management actions in the table', async () => {
    mountDestinations(PAGE, {
      routes: { [`GET ${LIST}`]: () => json(200, destinationList([revokedBody()], 1)) },
    })
    const table = await screen.findByRole('table')
    expect(within(table).getByText('Revoked')).toBeInTheDocument()
  })
})
