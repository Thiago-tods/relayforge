import { useMemo } from 'react'
import { Outlet, useParams } from 'react-router-dom'
import { useAuth } from '../auth/authContext'
import { NotFoundPage } from '../app/NotFoundPage'
import { OrgContext } from './orgContext'
import type { OrgValue } from './orgContext'
import { can } from './permissions'

/**
 * Scopes everything below it to one organization from the URL. The organization must be one the signed-in user
 * belongs to (the list comes from the server), so a foreign id renders "not found" and requests nothing. The
 * subtree is keyed by the id, so switching organizations discards all state of the previous one.
 */
export function OrgLayout() {
  const { orgId } = useParams()
  const { organizations } = useAuth()
  const organization = organizations.find((o) => o.id === orgId)

  const value = useMemo<OrgValue | null>(
    () =>
      organization
        ? { organization, can: (permission) => can(organization.role, permission) }
        : null,
    [organization],
  )

  if (!value)
    return <NotFoundPage message="This organization does not exist or you are not a member." />
  return (
    <OrgContext.Provider value={value}>
      <Outlet key={value.organization.id} />
    </OrgContext.Provider>
  )
}
