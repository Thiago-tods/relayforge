import { act, screen, waitFor, within } from '@testing-library/react'
import type { UserEvent } from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import { setCsrfToken } from '../../api/client'
import { DEST_ID, destinationBody, destinationList, SECRET } from '../../test/destinationFixtures'
import { mountDestinations, requests } from '../../test/destinationHarness'
import { apiError, json } from '../../test/fetchStub'
import type { Handler } from '../../test/fetchStub'

vi.mock('../../auth/crossTab', () => ({ openAuthChannel: vi.fn() }))

// Stubbed fetch only. These tests cover the one-time secret handling, the mutation timeout, abort on leaving and the
// outcome of a create. The jsdom <dialog> is a stand-in (see test/setup.ts): real focus trapping and the browser's
// own Escape and close handling are not exercised here.

const LIST = '/api/v1/organizations/o1/destinations'
const ONE = `${LIST}/${DEST_ID}`
const LIST_PAGE = '/o/o1/destinations'
const DETAIL_PAGE = `/o/o1/destinations/${DEST_ID}`

const created: Handler = () =>
  json(201, { destination: destinationBody({ signingSecret: SECRET }) })
const rotated: Handler = () =>
  json(200, { destination: destinationBody({ signingSecret: SECRET }) })
const emptyList: Handler = () => json(200, destinationList([destinationBody()]))
const detail: Handler = () => json(200, { destination: destinationBody() })

/** A request that never answers; it fails only when the page aborts it. */
const hangs: Handler = (call) =>
  new Promise<Response>((_resolve, reject) => {
    call.signal?.addEventListener('abort', () => reject(new DOMException('aborted', 'AbortError')))
  })

async function submitCreate(user: UserEvent) {
  setCsrfToken('tok')
  await user.click(await screen.findByRole('button', { name: 'Create destination' }))
  const form = screen.getByRole('dialog')
  await user.type(within(form).getByLabelText('Name'), 'New hook')
  await user.type(within(form).getByLabelText('HTTPS URL'), 'https://new.example.com/h?token=abc')
  await user.click(within(form).getByRole('button', { name: 'Create destination' }))
}

async function showCreatedSecret(routes: Record<string, Handler> = {}) {
  const mounted = mountDestinations(LIST_PAGE, {
    routes: { [`GET ${LIST}`]: emptyList, [`POST ${LIST}`]: created, ...routes },
  })
  await submitCreate(mounted.user)
  const dialog = await screen.findByRole('dialog', { name: 'Destination created' })
  return { ...mounted, dialog }
}

async function showRotatedSecret(routes: Record<string, Handler> = {}) {
  const mounted = mountDestinations(DETAIL_PAGE, {
    routes: { [`GET ${ONE}`]: detail, [`POST ${ONE}/rotate-secret`]: rotated, ...routes },
  })
  const button = await screen.findByRole('button', { name: 'Rotate signing secret' })
  setCsrfToken('tok')
  await mounted.user.click(button)
  await mounted.user.click(
    within(screen.getByRole('dialog')).getByRole('button', { name: 'Rotate secret' }),
  )
  const dialog = await screen.findByRole('dialog', { name: 'New signing secret' })
  return { ...mounted, dialog }
}

/** The browser closes the dialog by itself: the native `close` event reaches a dialog that is still mounted. */
function closeNatively(dialog: HTMLElement) {
  act(() => (dialog as HTMLDialogElement).close())
}

function expectNoSecretAnywhere() {
  expect(screen.queryByTestId('signing-secret')).not.toBeInTheDocument()
  expect(document.querySelector('dialog')).toBeNull()
  expect(document.body.textContent).not.toContain(SECRET)
  expect(window.getSelection()?.toString() ?? '').not.toContain(SECRET)
  expect(JSON.stringify({ ...window.localStorage, ...window.sessionStorage })).not.toContain(SECRET)
  expect(document.cookie).not.toContain(SECRET)
}

async function acknowledge(user: UserEvent, dialog: HTMLElement) {
  await user.click(within(dialog).getByRole('checkbox'))
  await user.click(within(dialog).getByRole('button', { name: 'Done' }))
}

/** Runs `body` with navigator.clipboard replaced, and restores whatever was there before. */
async function withClipboard(clipboard: unknown, body: () => Promise<void>) {
  const own = Object.getOwnPropertyDescriptor(navigator, 'clipboard')
  Object.defineProperty(navigator, 'clipboard', { value: clipboard, configurable: true })
  try {
    await body()
  } finally {
    if (own) Object.defineProperty(navigator, 'clipboard', own)
    else Reflect.deleteProperty(navigator, 'clipboard')
  }
}

describe('an unexpected native close of the secret dialog', () => {
  it('discards a created secret from the DOM and the state, and says what to do', async () => {
    const { dialog } = await showCreatedSecret()
    expect(within(dialog).getByTestId('signing-secret')).toHaveTextContent(SECRET)
    closeNatively(dialog)
    expectNoSecretAnywhere()
    const notice = await screen.findByRole('alert', { name: 'The signing secret was discarded' })
    expect(notice).toHaveTextContent(/rotate its signing secret/)
    expect(notice).not.toHaveTextContent(SECRET)
    expect(screen.getByRole('heading', { name: 'Destinations', level: 1 })).toHaveFocus()
  })

  it('discards a rotated secret and tells the user to rotate again', async () => {
    const { dialog, calls } = await showRotatedSecret()
    closeNatively(dialog)
    expectNoSecretAnywhere()
    const notice = await screen.findByRole('alert', { name: 'The signing secret was discarded' })
    expect(notice).toHaveTextContent(/Rotate the signing secret again/)
    expect(requests(calls, 'POST', `${ONE}/rotate-secret`)).toHaveLength(1)
  })

  it('leaves no secret when the dialog is closed again after it was discarded', async () => {
    const { dialog } = await showCreatedSecret()
    closeNatively(dialog)
    closeNatively(dialog)
    expectNoSecretAnywhere()
  })

  it('does not report a discard after a normal acknowledged dismissal', async () => {
    const { user, dialog } = await showCreatedSecret()
    await acknowledge(user, dialog)
    expectNoSecretAnywhere()
    expect(
      screen.queryByRole('alert', { name: 'The signing secret was discarded' }),
    ).not.toBeInTheDocument()
  })

  it('still needs the acknowledgment for a normal dismissal', async () => {
    const { dialog } = await showCreatedSecret()
    expect(within(dialog).getByRole('button', { name: 'Done' })).toBeDisabled()
    expect(within(dialog).getByTestId('signing-secret')).toHaveTextContent(SECRET)
  })
})

describe('the secret after Done', () => {
  it('is gone from the DOM, storage and selection for a created destination', async () => {
    const { user, dialog } = await showCreatedSecret()
    await acknowledge(user, dialog)
    expectNoSecretAnywhere()
  })

  it('is gone from the DOM, storage and selection for a rotated secret', async () => {
    const { user, dialog } = await showRotatedSecret()
    await acknowledge(user, dialog)
    expectNoSecretAnywhere()
    expect(screen.getByRole('heading', { name: 'Billing hook', level: 1 })).toHaveFocus()
  })
})

describe('copying the secret', () => {
  it('explains that copying failed when the clipboard is unavailable, and selects the secret', async () => {
    const { user, dialog } = await showCreatedSecret()
    await withClipboard(undefined, async () => {
      await user.click(within(dialog).getByRole('button', { name: 'Copy secret' }))
      expect(await within(dialog).findByText(/Could not copy/)).toBeInTheDocument()
      expect(window.getSelection()?.toString()).toBe(SECRET)
    })
    // The dialog stays open and still needs the acknowledgment.
    expect(within(dialog).getByRole('button', { name: 'Done' })).toBeDisabled()
    await acknowledge(user, dialog)
    expectNoSecretAnywhere()
  })

  it('falls back the same way when the clipboard rejects the write', async () => {
    const writeText = vi.fn().mockRejectedValue(new DOMException('denied', 'NotAllowedError'))
    const { user, dialog } = await showRotatedSecret()
    await withClipboard({ writeText }, async () => {
      await user.click(within(dialog).getByRole('button', { name: 'Copy secret' }))
      expect(await within(dialog).findByText(/Could not copy/)).toBeInTheDocument()
      expect(writeText).toHaveBeenCalledTimes(1)
      expect(window.getSelection()?.toString()).toBe(SECRET)
      expect(within(dialog).queryByText(/Secret copied/)).not.toBeInTheDocument()
    })
    expect(screen.getByRole('dialog', { name: 'New signing secret' })).toBeInTheDocument()
  })

  it('writes only the secret to the clipboard and reports success without it', async () => {
    const writeText = vi.fn().mockResolvedValue(undefined)
    const { user, dialog } = await showCreatedSecret()
    await withClipboard({ writeText }, async () => {
      await user.click(within(dialog).getByRole('button', { name: 'Copy secret' }))
      expect(await within(dialog).findByText('Secret copied to the clipboard.')).toBeInTheDocument()
    })
    expect(writeText).toHaveBeenCalledWith(SECRET)
    expect(document.querySelector('.toast-region')?.textContent ?? '').not.toContain(SECRET)
  })
})

describe('console output', () => {
  it('never receives the secret across create, copy failure, rotate and discard', async () => {
    const spies = (['log', 'info', 'warn', 'error', 'debug', 'trace'] as const).map((method) =>
      vi.spyOn(console, method).mockImplementation(() => undefined),
    )
    const writeText = vi.fn().mockRejectedValue(new Error('denied'))
    const created = await showCreatedSecret()
    await withClipboard({ writeText }, async () => {
      await created.user.click(within(created.dialog).getByRole('button', { name: 'Copy secret' }))
      await within(created.dialog).findByText(/Could not copy/)
    })
    closeNatively(created.dialog)
    await screen.findByRole('alert', { name: 'The signing secret was discarded' })

    const rotated = await showRotatedSecret()
    await acknowledge(rotated.user, rotated.dialog)
    expectNoSecretAnywhere()

    const logged = spies.flatMap((spy) => spy.mock.calls)
    expect(JSON.stringify(logged, (_key, value: unknown) => String(value))).not.toContain(SECRET)
  })
})

describe('the mutation timeout', () => {
  it('treats a request that takes 30 seconds as unknown, aborts it and never repeats it', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    const { user, calls } = mountDestinations(DETAIL_PAGE, {
      routes: { [`GET ${ONE}`]: detail, [`POST ${ONE}/pause`]: hangs },
    })
    const button = await screen.findByRole('button', { name: 'Pause' })
    setCsrfToken('tok')
    await user.click(button)
    await user.click(
      within(screen.getByRole('dialog')).getByRole('button', { name: 'Pause destination' }),
    )
    const pause = () => requests(calls, 'POST', `${ONE}/pause`)
    await waitFor(() => expect(pause()).toHaveLength(1))

    await act(async () => {
      vi.advanceTimersByTime(25_000)
    })
    expect(pause()[0]!.signal?.aborted).toBe(false)
    expect(screen.getByRole('dialog')).toBeInTheDocument()

    await act(async () => {
      vi.advanceTimersByTime(6_000)
    })
    const notice = await screen.findByRole('status', { name: 'The result is unknown' })
    expect(notice).toHaveTextContent(/not repeated/)
    expect(pause()[0]!.signal?.aborted).toBe(true)
    await waitFor(() => expect(requests(calls, 'GET', ONE)).toHaveLength(2))
    expect(pause()).toHaveLength(1)
  })

  it('treats a create that takes 30 seconds as unknown, and no secret is ever shown', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    const { user, calls } = mountDestinations(LIST_PAGE, {
      routes: { [`GET ${LIST}`]: emptyList, [`POST ${LIST}`]: hangs },
    })
    await submitCreate(user)
    await waitFor(() => expect(requests(calls, 'POST', LIST)).toHaveLength(1))
    await act(async () => {
      vi.advanceTimersByTime(31_000)
    })
    const notice = await screen.findByRole('status', { name: 'The result is unknown' })
    expect(notice).toHaveTextContent(/may or may not have been created/)
    expect(notice).toHaveTextContent(/rotate it from its page/)
    expect(requests(calls, 'POST', LIST)).toHaveLength(1)
    await waitFor(() => expect(requests(calls, 'GET', LIST)).toHaveLength(2))
    expect(screen.queryByTestId('signing-secret')).not.toBeInTheDocument()
  })
})

describe('leaving while a mutation is pending', () => {
  it('aborts a pending create when the organization changes and ignores a late answer', async () => {
    let answer: (response: Response) => void = () => undefined
    const late = new Promise<Response>((resolve) => {
      answer = resolve
    })
    const { user, calls } = mountDestinations(LIST_PAGE, {
      routes: { [`GET ${LIST}`]: emptyList, [`POST ${LIST}`]: () => late },
    })
    await submitCreate(user)
    await waitFor(() => expect(requests(calls, 'POST', LIST)).toHaveLength(1))
    await user.selectOptions(screen.getByLabelText('Organization'), 'o2')
    await waitFor(() => expect(screen.getByTestId('location')).toHaveTextContent('/o/o2'))
    expect(requests(calls, 'POST', LIST)[0]!.signal?.aborted).toBe(true)

    // The server answers after the page was left: the secret must never reach the DOM.
    await act(async () => {
      answer(await created(requests(calls, 'POST', LIST)[0]!))
      await new Promise((resolve) => setTimeout(resolve, 20))
    })
    expect(screen.queryByTestId('signing-secret')).not.toBeInTheDocument()
    expect(document.body.textContent).not.toContain(SECRET)
    expect(requests(calls, 'POST', LIST)).toHaveLength(1)
  })

  it('aborts a pending rotation when the page is left and ignores a late answer', async () => {
    let answer: (response: Response) => void = () => undefined
    const late = new Promise<Response>((resolve) => {
      answer = resolve
    })
    const { user, calls } = mountDestinations(DETAIL_PAGE, {
      routes: { [`GET ${ONE}`]: detail, [`POST ${ONE}/rotate-secret`]: () => late },
    })
    const button = await screen.findByRole('button', { name: 'Rotate signing secret' })
    setCsrfToken('tok')
    await user.click(button)
    await user.click(
      within(screen.getByRole('dialog')).getByRole('button', { name: 'Rotate secret' }),
    )
    await waitFor(() => expect(requests(calls, 'POST', `${ONE}/rotate-secret`)).toHaveLength(1))
    await user.selectOptions(screen.getByLabelText('Organization'), 'o2')
    await waitFor(() => expect(screen.getByTestId('location')).toHaveTextContent('/o/o2'))
    expect(requests(calls, 'POST', `${ONE}/rotate-secret`)[0]!.signal?.aborted).toBe(true)

    await act(async () => {
      answer(await rotated(requests(calls, 'POST', `${ONE}/rotate-secret`)[0]!))
      await new Promise((resolve) => setTimeout(resolve, 20))
    })
    expect(screen.queryByTestId('signing-secret')).not.toBeInTheDocument()
    expect(document.body.textContent).not.toContain(SECRET)
    expect(requests(calls, 'POST', `${ONE}/rotate-secret`)).toHaveLength(1)
  })
})

describe('the outcome of a create', () => {
  const definite: [string, Handler, RegExp][] = [
    [
      'a 404',
      () => apiError(404, 'not_found', 'resource not found'),
      /could not be found, or you no longer have access/,
    ],
    [
      'a 409 destination_revoked',
      () => apiError(409, 'destination_revoked', 'the destination is revoked'),
      /conflict with a revoked destination/,
    ],
  ]
  it.each(definite)(
    'reports %s as a rejection: nothing was created, no unknown result, no reload',
    async (_name, post, text) => {
      const { user, calls } = mountDestinations(LIST_PAGE, {
        routes: { [`GET ${LIST}`]: emptyList, [`POST ${LIST}`]: post },
      })
      await submitCreate(user)
      const notice = await screen.findByRole('alert', { name: 'Destination not created' })
      expect(notice).toHaveTextContent(text)
      expect(notice).toHaveTextContent(/Nothing was created/)
      // Safe: nothing the user typed is repeated.
      expect(notice).not.toHaveTextContent(/New hook|new\.example\.com|token/)
      expect(
        screen.queryByRole('status', { name: 'The result is unknown' }),
      ).not.toBeInTheDocument()
      expect(requests(calls, 'POST', LIST)).toHaveLength(1)
      expect(requests(calls, 'GET', LIST)).toHaveLength(1)
    },
  )

  const ambiguous: [string, Handler][] = [
    [
      'a lost connection',
      () => {
        throw new TypeError('Failed to fetch')
      },
    ],
    ['a proxy 502', () => apiError(502, 'bad_gateway', 'x')],
    ['an unreadable 201', () => json(201, { destination: destinationBody() })],
  ]
  it.each(ambiguous)('still reports %s as unknown, once, with a list refresh', async (_n, post) => {
    const { user, calls } = mountDestinations(LIST_PAGE, {
      routes: { [`GET ${LIST}`]: emptyList, [`POST ${LIST}`]: post },
    })
    await submitCreate(user)
    expect(await screen.findByRole('status', { name: 'The result is unknown' })).toBeInTheDocument()
    await waitFor(() => expect(requests(calls, 'GET', LIST)).toHaveLength(2))
    expect(requests(calls, 'POST', LIST)).toHaveLength(1)
  })
})
