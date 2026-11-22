import { createContext, useContext } from 'react'
import type { OrganizationWithRole } from '../api/types'
import type { Permission } from './permissions'

export interface OrgValue {
  organization: OrganizationWithRole
  /** Whether the UI should offer an action. The backend still authorizes every request. */
  can: (permission: Permission) => boolean
}

export const OrgContext = createContext<OrgValue | null>(null)

export function useOrg(): OrgValue {
  const value = useContext(OrgContext)
  if (!value) throw new Error('useOrg must be used inside OrgLayout')
  return value
}
