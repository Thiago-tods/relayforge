import { act, screen, waitFor, within } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { apiError, json, meBody, noContent, org, stubFetch, USER } from './test/fetchStub'
import { fakeChannel, renderApp, routes } from './test/renderApp'

vi.mock('./auth/crossTab', () => ({ openAuthChannel: vi.fn() }))

const ME = 'GET /api/v1/auth/me'
const LOGIN = 'POST /api/v1/auth/login'
const LOGOUT = 'POST /api/v1/auth/logout'
const CSRF = 'POST /api/v1/auth/csrf'
const signedOut = () => apiError(401, 'unauthenticated', 'authentication is required')
const signedIn = (...orgs: ReturnType<typeof org>[]) => json(200, meBody(orgs))
const csrf = () => json(200, { session: { csrfToken: 'tok-1' } })

describe('routing and session', () => {
  it('sends a signed-out visitor to sign-in', async () => {
    fakeChannel()
    stubFetch(routes({ [ME]: signedOut }).handler)
    renderApp('/orgs')
    expect(await screen.findByRole('heading', { name: 'Sign in' })).toBeInTheDocument()
  })

  it('shows the organizations of a signed-in user', async () => {
    fakeChannel()
    stubFetch(routes({ [ME]: () => signedIn(org('o1'), org('o2', 'VIEWER')) }).handler)
    renderApp('/')
    expect(await screen.findByRole('heading', { name: 'Organizations' })).toBeInTheDocument()
    const list = screen.getByRole('list', { name: 'Your organizations' })
    expect(within(list).getByText('Org o1')).toBeInTheDocument()
    expect(within(list).getByText('VIEWER')).toBeInTheDocument()
  })

  it('keeps a signed-in user away from the sign-in page', async () => {
    fakeChannel()
    stubFetch(routes({ [ME]: () => signedIn(org('o1'), org('o2')) }).handler)
    renderApp('/login')
    expect(await screen.findByRole('heading', { name: 'Organizations' })).toBeInTheDocument()
  })

  it('opens the only organization of a signed-in user straight away', async () => {
    fakeChannel()
    stubFetch(routes({ [ME]: () => signedIn(org('o1')) }).handler)
    renderApp('/login')
    expect(await screen.findByRole('heading', { name: 'Org o1' })).toBeInTheDocument()
  })

  it('offers a retry when the session cannot be checked', async () => {
    fakeChannel()
    let down = true
    stubFetch(
      routes({
        [ME]: () => (down ? apiError(503, 'unavailable', 'db is down') : signedIn(org('o1'))),
      }).handler,
    )
    const { user } = renderApp('/orgs')
    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent('Could not check your session')
    expect(alert).not.toHaveTextContent('db is down')
    down = false
    await user.click(screen.getByRole('button', { name: 'Try again' }))
    expect(await screen.findByRole('heading', { name: 'Organizations' })).toBeInTheDocument()
  })
})

describe('sign in', () => {
  it('signs in, sends no CSRF header and lands in the only organization', async () => {
    fakeChannel()
    const { handler } = routes({
      [ME]: signedOut,
      [LOGIN]: () =>
        json(200, {
          user: USER,
          session: { expiresAt: '2099-01-01T00:00:00Z', csrfToken: 't' },
          organizations: [org('o1')],
        }),
    })
    const calls = stubFetch(handler)
    const { user } = renderApp('/login')
    await user.type(await screen.findByLabelText('Email'), ' ada@example.com ')
    await user.type(screen.getByLabelText('Password'), 'correct horse battery')
    await user.click(screen.getByRole('button', { name: 'Sign in' }))
    expect(await screen.findByRole('heading', { name: 'Org o1' })).toBeInTheDocument()
    const login = calls.find((c) => c.method === 'POST')
    expect(login?.body).toEqual({ email: 'ada@example.com', password: 'correct horse battery' })
    expect(login?.headers.has('X-CSRF-Token')).toBe(false)
  })

  it('returns to the page that required sign-in', async () => {
    fakeChannel()
    stubFetch(
      routes({
        [ME]: signedOut,
        [LOGIN]: () =>
          json(200, {
            user: USER,
            session: { expiresAt: '2099-01-01T00:00:00Z', csrfToken: 't' },
            organizations: [org('o1')],
          }),
      }).handler,
    )
    const { user } = renderApp('/o/o1')
    await user.type(await screen.findByLabelText('Email'), 'ada@example.com')
    await user.type(screen.getByLabelText('Password'), 'correct horse battery')
    await user.click(screen.getByRole('button', { name: 'Sign in' }))
    expect(await screen.findByRole('heading', { name: 'Org o1' })).toBeInTheDocument()
    expect(screen.getByTestId('location')).toHaveTextContent('/o/o1')
  })

  it('shows the server message and re-enables the button after a failure', async () => {
    fakeChannel()
    stubFetch(
      routes({
        [ME]: signedOut,
        [LOGIN]: () => apiError(401, 'invalid_credentials', 'invalid email or password'),
      }).handler,
    )
    const { user } = renderApp('/login')
    await user.type(await screen.findByLabelText('Email'), 'ada@example.com')
    await user.type(screen.getByLabelText('Password'), 'wrong password here')
    await user.click(screen.getByRole('button', { name: 'Sign in' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('invalid email or password')
    expect(screen.getByRole('button', { name: 'Sign in' })).toBeEnabled()
    expect(screen.getByRole('heading', { name: 'Sign in' })).toBeInTheDocument()
  })

  it('validates before sending anything', async () => {
    fakeChannel()
    const calls = stubFetch(routes({ [ME]: signedOut }).handler)
    const { user } = renderApp('/login')
    await user.click(await screen.findByRole('button', { name: 'Sign in' }))
    expect(screen.getByText('Enter your email address.')).toBeInTheDocument()
    expect(screen.getByText('Enter your password.')).toBeInTheDocument()
    expect(calls.filter((c) => c.method === 'POST')).toHaveLength(0)
    // Focus moves to the first invalid field, which announces its label and its linked error.
    expect(screen.getByLabelText('Email')).toHaveFocus()
    expect(screen.getByLabelText('Email')).toHaveAccessibleDescription('Enter your email address.')
  })

  it('does not submit twice while a sign-in is pending', async () => {
    fakeChannel()
    let release: () => void = () => undefined
    const gate = new Promise<void>((resolve) => {
      release = resolve
    })
    const calls = stubFetch(
      routes({
        [ME]: signedOut,
        [LOGIN]: async () => {
          await gate
          return json(200, {
            user: USER,
            session: { expiresAt: '2099-01-01T00:00:00Z', csrfToken: 't' },
            organizations: [],
          })
        },
      }).handler,
    )
    const { user } = renderApp('/login')
    await user.type(await screen.findByLabelText('Email'), 'ada@example.com')
    await user.type(screen.getByLabelText('Password'), 'correct horse battery')
    const button = screen.getByRole('button', { name: 'Sign in' })
    await user.click(button)
    await user.click(button)
    expect(button).toBeDisabled()
    release()
    await screen.findByRole('heading', { name: 'Organizations' })
    expect(calls.filter((c) => c.method === 'POST')).toHaveLength(1)
  })

  it('validates the registration form (12 to 128 character password, slug shape)', async () => {
    fakeChannel()
    const calls = stubFetch(routes({ [ME]: signedOut }).handler)
    const { user } = renderApp('/register')
    await user.type(await screen.findByLabelText('Your name'), 'Ada')
    await user.type(screen.getByLabelText('Email'), 'ada@example.com')
    await user.type(screen.getByLabelText('Password'), 'short')
    await user.type(screen.getByLabelText('Organization slug (optional)'), 'Bad Slug')
    await user.click(screen.getByRole('button', { name: 'Create account' }))
    expect(screen.getByText('Use at least 12 characters.')).toBeInTheDocument()
    expect(
      screen.getByText('Use 3 to 48 lowercase letters, digits or single hyphens.'),
    ).toBeInTheDocument()
    expect(calls.filter((c) => c.method === 'POST')).toHaveLength(0)
    // The name and email are valid, so the first invalid field is the password.
    expect(screen.getByLabelText('Password')).toHaveFocus()
  })

  it('registers and opens the new organization', async () => {
    fakeChannel()
    const calls = stubFetch(
      routes({
        [ME]: signedOut,
        'POST /api/v1/auth/register': () =>
          json(201, {
            user: USER,
            session: { expiresAt: '2099-01-01T00:00:00Z', csrfToken: 't' },
            organization: { ...org('o9'), role: undefined },
          }),
      }).handler,
    )
    const { user } = renderApp('/register')
    await user.type(await screen.findByLabelText('Your name'), 'Ada')
    await user.type(screen.getByLabelText('Email'), 'ada@example.com')
    await user.type(screen.getByLabelText('Password'), 'a long enough password')
    await user.click(screen.getByRole('button', { name: 'Create account' }))
    expect(await screen.findByRole('heading', { name: 'Org o9' })).toBeInTheDocument()
    expect(calls.find((c) => c.method === 'POST')?.body).toMatchObject({
      displayName: 'Ada',
      organizationName: '',
      organizationSlug: '',
    })
  })
})

describe('sign out and lost sessions', () => {
  it('signs out with the CSRF token and tells the other tabs', async () => {
    const channel = fakeChannel()
    const calls = stubFetch(
      routes({ [ME]: () => signedIn(org('o1')), [CSRF]: csrf, [LOGOUT]: noContent }).handler,
    )
    const { user } = renderApp('/orgs')
    await user.click(await screen.findByRole('button', { name: 'Sign out' }))
    expect(await screen.findByRole('heading', { name: 'Sign in' })).toBeInTheDocument()
    expect(calls.find((c) => c.path.endsWith('/auth/logout'))?.headers.get('X-CSRF-Token')).toBe(
      'tok-1',
    )
    expect(channel.post).toHaveBeenCalledWith('signed-out')
  })

  it('ends the session locally when the server answers 401 to a request', async () => {
    const channel = fakeChannel()
    stubFetch(
      routes({
        [ME]: () => signedIn(org('o1')),
        [CSRF]: csrf,
        'POST /api/v1/organizations': signedOut,
      }).handler,
    )
    const { user } = renderApp('/orgs')
    await user.type(await screen.findByLabelText('Name'), 'New org')
    await user.click(screen.getByRole('button', { name: 'Create organization' }))
    expect(await screen.findByRole('heading', { name: 'Sign in' })).toBeInTheDocument()
    expect(screen.getByText('Your session has ended. Sign in again.')).toBeInTheDocument()
    expect(channel.post).toHaveBeenCalledWith('signed-out')
  })

  // Another tab signed in as a different user, so the shared cookie is now theirs. This tab must refuse to act.
  function cookieChangedHands() {
    let reads = 0
    return routes({
      [ME]: () => {
        reads += 1
        return reads === 1
          ? signedIn(org('o1'))
          : json(200, { ...meBody([org('o2')]), user: { ...USER, id: 'u-2' } })
      },
      [CSRF]: csrf,
      'POST /api/v1/organizations': () => json(201, { organization: org('new') }),
      [LOGOUT]: noContent,
    })
  }

  it('does not create anything as another user after another tab signed in as them', async () => {
    fakeChannel()
    const { handler } = cookieChangedHands()
    const calls = stubFetch(handler)
    const { user } = renderApp('/orgs')
    await user.type(await screen.findByLabelText('Name'), 'New org')
    await user.click(screen.getByRole('button', { name: 'Create organization' }))
    expect(await screen.findByRole('heading', { name: 'Sign in' })).toBeInTheDocument()
    expect(screen.getByText(/different user in another tab/)).toBeInTheDocument()
    expect(calls.some((c) => c.path === '/api/v1/organizations')).toBe(false)
  })

  it("does not end the other user's session when signing out of a stale tab", async () => {
    const channel = fakeChannel()
    const { handler } = cookieChangedHands()
    const calls = stubFetch(handler)
    const { user } = renderApp('/orgs')
    await user.click(await screen.findByRole('button', { name: 'Sign out' }))
    expect(await screen.findByRole('heading', { name: 'Sign in' })).toBeInTheDocument()
    expect(screen.getByText(/different user in another tab/)).toBeInTheDocument()
    expect(calls.some((c) => c.path === '/api/v1/auth/logout')).toBe(false)
    expect(channel.post).not.toHaveBeenCalledWith('signed-out')
  })

  it('signs out when another tab signed out', async () => {
    const channel = fakeChannel()
    stubFetch(routes({ [ME]: () => signedIn(org('o1')) }).handler)
    renderApp('/orgs')
    await screen.findByRole('heading', { name: 'Organizations' })
    act(() => channel.receive('signed-out'))
    expect(await screen.findByRole('heading', { name: 'Sign in' })).toBeInTheDocument()
    expect(screen.getByText('You were signed out in another tab.')).toBeInTheDocument()
  })

  it('drops a tab whose session now belongs to a different user', async () => {
    const channel = fakeChannel()
    let reads = 0
    stubFetch(
      routes({
        [ME]: () => {
          reads += 1
          return reads === 1
            ? signedIn(org('o1'))
            : json(200, { ...meBody([org('o2')]), user: { ...USER, id: 'u-2' } })
        },
      }).handler,
    )
    renderApp('/orgs')
    await screen.findByRole('heading', { name: 'Organizations' })
    act(() => channel.receive('signed-in'))
    expect(await screen.findByRole('heading', { name: 'Sign in' })).toBeInTheDocument()
    expect(screen.getByText(/different user in another tab/)).toBeInTheDocument()
  })

  it('stays signed in when another tab signed in as the same user', async () => {
    const channel = fakeChannel()
    stubFetch(routes({ [ME]: () => signedIn(org('o1')) }).handler)
    renderApp('/orgs')
    await screen.findByRole('heading', { name: 'Organizations' })
    act(() => channel.receive('signed-in'))
    await waitFor(() =>
      expect(screen.queryByRole('heading', { name: 'Sign in' })).not.toBeInTheDocument(),
    )
    expect(screen.getByRole('heading', { name: 'Organizations' })).toBeInTheDocument()
  })

  it('signs out when the absolute session lifetime ends', async () => {
    fakeChannel()
    const soon = new Date(Date.now() + 300).toISOString()
    stubFetch(routes({ [ME]: () => json(200, meBody([org('o1')], soon)) }).handler)
    renderApp('/orgs')
    expect(
      await screen.findByRole('heading', { name: 'Sign in' }, { timeout: 3000 }),
    ).toBeInTheDocument()
    expect(screen.getByText('Your session expired. Sign in again.')).toBeInTheDocument()
  })
})

describe('organization isolation and shell', () => {
  it('shows "not found" for an organization the user does not belong to, without requesting it', async () => {
    fakeChannel()
    const calls = stubFetch(routes({ [ME]: () => signedIn(org('o1')) }).handler)
    renderApp('/o/someone-elses-org')
    expect(await screen.findByText(/does not exist or you are not a member/)).toBeInTheDocument()
    expect(calls.map((c) => `${c.method} ${c.path}`)).toEqual([ME])
  })

  it('switches organizations from the top bar', async () => {
    fakeChannel()
    stubFetch(routes({ [ME]: () => signedIn(org('o1'), org('o2')) }).handler)
    const { user } = renderApp('/o/o1')
    expect(await screen.findByRole('heading', { name: 'Org o1' })).toBeInTheDocument()
    await user.selectOptions(screen.getByRole('combobox', { name: 'Organization' }), 'o2')
    expect(await screen.findByRole('heading', { name: 'Org o2' })).toBeInTheDocument()
  })

  it('opens and closes the mobile navigation with the button and Escape', async () => {
    fakeChannel()
    stubFetch(routes({ [ME]: () => signedIn(org('o1')) }).handler)
    const { user } = renderApp('/orgs')
    const button = await screen.findByRole('button', { name: 'Menu' })
    expect(button).toHaveAttribute('aria-expanded', 'false')
    await user.click(button)
    expect(button).toHaveAttribute('aria-expanded', 'true')
    const nav = screen.getByRole('navigation', { name: 'Main navigation' })
    expect(nav).toHaveClass('open')
    // Opening the drawer moves focus into it.
    expect(within(nav).getByRole('link', { name: 'Organizations' })).toHaveFocus()
    await user.keyboard('{Escape}')
    expect(button).toHaveAttribute('aria-expanded', 'false')
    expect(button).toHaveFocus()
  })

  it('closes the drawer when a link is followed and moves focus to the page content', async () => {
    fakeChannel()
    stubFetch(routes({ [ME]: () => signedIn(org('o1')) }).handler)
    const { user } = renderApp('/orgs')
    const button = await screen.findByRole('button', { name: 'Menu' })
    await user.click(button)
    // The link points at the page that is already open, so only the click handler can close the drawer.
    await user.click(screen.getByRole('link', { name: 'Organizations' }))
    expect(button).toHaveAttribute('aria-expanded', 'false')
    expect(document.getElementById('main')).toHaveFocus()
  })

  it('shows "not found" for unknown pages', async () => {
    fakeChannel()
    stubFetch(routes({ [ME]: () => signedIn(org('o1')) }).handler)
    renderApp('/no/such/page')
    expect(await screen.findByRole('heading', { name: 'Page not found' })).toBeInTheDocument()
  })
})

describe('theme', () => {
  it('switches theme, remembers it, and applies it to the document', async () => {
    fakeChannel()
    stubFetch(routes({ [ME]: signedOut }).handler)
    const { user, unmount } = renderApp('/login')
    expect(document.documentElement.dataset.theme).toBe('light')
    await user.click(await screen.findByRole('button', { name: 'Switch to dark theme' }))
    expect(document.documentElement.dataset.theme).toBe('dark')
    expect(window.localStorage.getItem('relayforge.theme')).toBe('dark')
    unmount()

    stubFetch(routes({ [ME]: signedOut }).handler)
    renderApp('/login')
    await screen.findByRole('button', { name: 'Switch to light theme' })
    expect(document.documentElement.dataset.theme).toBe('dark')
  })
})
