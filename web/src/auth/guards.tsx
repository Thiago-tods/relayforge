import { Navigate, Outlet, useLocation } from 'react-router-dom'
import { ErrorState, LoadingState } from '../ui/states'
import { useAuth } from './authContext'
import { landingPath } from './landing'

/** Renders the nested routes only for a signed-in user. Everyone else is sent to /login. */
export function RequireAuth() {
  const auth = useAuth()
  const location = useLocation()
  if (auth.status === 'loading') return <LoadingState label="Checking your session" />
  if (auth.status === 'error') {
    return (
      <ErrorState
        title="Could not check your session"
        message={auth.errorMessage ?? 'Try again.'}
        onRetry={auth.retry}
      />
    )
  }
  if (auth.status === 'anonymous') {
    return <Navigate to="/login" replace state={{ from: location.pathname + location.search }} />
  }
  return <Outlet />
}

/**
 * The sign-in and registration pages are for signed-out users. A user who is signed in (including one who has just
 * signed in on these pages) is sent on from here, so there is a single redirect and no race with the form.
 */
export function PublicOnly() {
  const auth = useAuth()
  const location = useLocation()
  if (auth.status === 'loading') return <LoadingState label="Checking your session" />
  if (auth.status === 'authenticated') {
    return <Navigate to={landingPath(location.state, auth.organizations)} replace />
  }
  return <Outlet />
}
