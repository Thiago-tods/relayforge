import { useEffect, useRef, useState } from 'react'
import { NavLink, Outlet, useLocation, useMatch, useNavigate } from 'react-router-dom'
import { useAuth } from '../auth/authContext'
import { Button } from '../ui/Button'
import { ThemeToggle } from './ThemeToggle'

/** The signed-in frame: sidebar (a drawer on narrow screens), top bar with organization switcher, theme and account. */
export function Shell() {
  const { user, organizations, logout } = useAuth()
  const navigate = useNavigate()
  const location = useLocation()
  const match = useMatch('/o/:orgId/*')
  const orgId = match?.params.orgId ?? ''
  // The drawer is open only for the page it was opened on, so navigating closes it without an effect.
  const [openOn, setOpenOn] = useState<string | null>(null)
  const menuOpen = openOn === location.pathname
  const setMenuOpen = (open: boolean) => setOpenOn(open ? location.pathname : null)
  const [signingOut, setSigningOut] = useState(false)
  const menuButton = useRef<HTMLButtonElement>(null)
  const sidebar = useRef<HTMLElement>(null)
  const main = useRef<HTMLElement>(null)
  const wasOpen = useRef(false)

  // Opening the drawer moves focus to its first link. When it closes because a link was followed, focus moves to
  // the page content instead of being lost with the hidden drawer.
  useEffect(() => {
    if (menuOpen) {
      sidebar.current?.querySelector<HTMLElement>('a')?.focus()
    } else if (wasOpen.current && sidebar.current?.contains(document.activeElement)) {
      main.current?.focus()
    }
    wasOpen.current = menuOpen
  }, [menuOpen])

  // Escape closes the drawer and returns focus to the button that opened it.
  useEffect(() => {
    if (!menuOpen) return undefined
    const onKey = (event: KeyboardEvent) => {
      if (event.key === 'Escape') {
        setOpenOn(null)
        menuButton.current?.focus()
      }
    }
    document.addEventListener('keydown', onKey)
    return () => document.removeEventListener('keydown', onKey)
  }, [menuOpen])

  async function signOut() {
    if (signingOut) return
    setSigningOut(true)
    await logout()
  }

  return (
    <div className="shell">
      <a className="skip-link" href="#main">
        Skip to content
      </a>
      <header className="topbar">
        <button
          ref={menuButton}
          type="button"
          className="btn btn-ghost menu-button"
          aria-expanded={menuOpen}
          aria-controls="sidebar"
          onClick={() => setMenuOpen(!menuOpen)}
        >
          <span className="visually-hidden">Menu</span>
          <span aria-hidden="true">☰</span>
        </button>
        <span className="brand">RelayForge</span>
        <div className="topbar-spacer" />
        {organizations.length > 0 ? (
          <label className="org-switch">
            <span className="visually-hidden">Organization</span>
            <select
              value={orgId}
              onChange={(e) =>
                void navigate(e.target.value === '' ? '/orgs' : `/o/${e.target.value}`)
              }
            >
              <option value="">All organizations</option>
              {organizations.map((organization) => (
                <option key={organization.id} value={organization.id}>
                  {organization.name}
                </option>
              ))}
            </select>
          </label>
        ) : null}
        <ThemeToggle />
        <span className="account" title={user?.email}>
          {user?.displayName}
        </span>
        <Button variant="secondary" busy={signingOut} onClick={() => void signOut()}>
          Sign out
        </Button>
      </header>
      <div className="shell-body">
        <nav
          ref={sidebar}
          id="sidebar"
          className={menuOpen ? 'sidebar open' : 'sidebar'}
          aria-label="Main navigation"
          onClick={() => setMenuOpen(false)}
        >
          <NavLink to="/orgs" end>
            Organizations
          </NavLink>
          {orgId !== '' ? (
            <NavLink to={`/o/${orgId}`} end>
              Overview
            </NavLink>
          ) : null}
        </nav>
        {menuOpen ? (
          <div className="scrim" aria-hidden="true" onClick={() => setMenuOpen(false)} />
        ) : null}
        <main ref={main} id="main" className="content" tabIndex={-1}>
          <Outlet />
        </main>
      </div>
    </div>
  )
}
