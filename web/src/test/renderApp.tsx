import { render } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter, useLocation } from 'react-router-dom'
import { vi } from 'vitest'
import type { Mock } from 'vitest'
import { App } from '../App'
import { openAuthChannel } from '../auth/crossTab'
import type { AuthSignal } from '../auth/crossTab'
import { apiError } from './fetchStub'
import type { Handler } from './fetchStub'

/** Answers "METHOD /path" from a table. Anything not in the table is a 404 and is listed in `unexpected`. */
export function routes(table: Record<string, Handler>): { handler: Handler; unexpected: string[] } {
  const unexpected: string[] = []
  const handler: Handler = (call) => {
    const answer = table[`${call.method} ${call.path}`]
    if (!answer) {
      unexpected.push(`${call.method} ${call.path}`)
      return apiError(404, 'not_found', 'resource not found')
    }
    return answer(call)
  }
  return { handler, unexpected }
}

export interface FakeChannel {
  post: Mock<(signal: AuthSignal) => void>
  close: Mock<() => void>
  /** Delivers a signal as if another tab had posted it. */
  receive: (signal: AuthSignal) => void
}

/** Replaces the BroadcastChannel wrapper (the module is mocked in the test file that uses this). */
export function fakeChannel(): FakeChannel {
  const channel: FakeChannel = { post: vi.fn(), close: vi.fn(), receive: () => undefined }
  vi.mocked(openAuthChannel).mockImplementation((onSignal) => {
    channel.receive = onSignal
    return channel
  })
  return channel
}

// eslint-disable-next-line react-refresh/only-export-components -- a test helper, not a hot-reloaded module
function Where() {
  const location = useLocation()
  return <output data-testid="location">{location.pathname}</output>
}

export function renderApp(path: string) {
  const user = userEvent.setup()
  const view = render(
    <MemoryRouter initialEntries={[path]}>
      <App />
      <Where />
    </MemoryRouter>,
  )
  return { user, ...view }
}
