import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { useState } from 'react'
import { Link, MemoryRouter, Route, Routes } from 'react-router-dom'
import { describe, expect, it, vi } from 'vitest'
import { AuthContext } from '../auth/authContext'
import type { AuthValue } from '../auth/authContext'
import { org, USER } from '../test/fetchStub'
import { OrgLayout } from './OrgLayout'
import { useOrg } from './orgContext'

function Probe() {
  const { organization, can } = useOrg()
  const [count, setCount] = useState(0)
  return (
    <div>
      <p>{`${organization.id}:${count}`}</p>
      <button type="button" onClick={() => setCount((n) => n + 1)}>
        count
      </button>
      {can('deliveries:redrive') ? <button type="button">Redrive</button> : null}
      <Link to="/o/b">to b</Link>
      <Link to="/o/a">to a</Link>
    </div>
  )
}

function renderAt(organizations: ReturnType<typeof org>[]) {
  const auth: AuthValue = {
    status: 'authenticated',
    user: USER,
    organizations,
    expiresAt: null,
    notice: null,
    errorMessage: null,
    login: vi.fn(),
    register: vi.fn(),
    logout: vi.fn(),
    addOrganization: vi.fn(),
    retry: vi.fn(),
  }
  return render(
    <AuthContext.Provider value={auth}>
      <MemoryRouter initialEntries={['/o/a']}>
        <Routes>
          <Route path="/o/:orgId" element={<OrgLayout />}>
            <Route index element={<Probe />} />
          </Route>
        </Routes>
      </MemoryRouter>
    </AuthContext.Provider>,
  )
}

describe('OrgLayout', () => {
  it('discards the state of the previous organization when the organization changes', async () => {
    const user = userEvent.setup()
    renderAt([org('a'), org('b')])
    await user.click(screen.getByRole('button', { name: 'count' }))
    expect(screen.getByText('a:1')).toBeInTheDocument()
    await user.click(screen.getByRole('link', { name: 'to b' }))
    expect(screen.getByText('b:0')).toBeInTheDocument()
  })

  it('offers redrive only to roles that hold the permission', () => {
    const view = renderAt([org('a', 'VIEWER')])
    expect(screen.queryByRole('button', { name: 'Redrive' })).not.toBeInTheDocument()
    view.unmount()
    renderAt([org('a', 'DEVELOPER')])
    expect(screen.getByRole('button', { name: 'Redrive' })).toBeInTheDocument()
  })
})
