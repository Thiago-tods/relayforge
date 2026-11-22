import { createContext, useContext } from 'react'
import type { RegisterInput } from '../api/auth'
import type { OrganizationWithRole, User } from '../api/types'

export type AuthStatus = 'loading' | 'authenticated' | 'anonymous' | 'error'

export interface AuthState {
  status: AuthStatus
  user: User | null
  organizations: OrganizationWithRole[]
  /** ISO time at which the absolute session lifetime ends. */
  expiresAt: string | null
  /** Why the user was signed out, shown on the sign-in page. */
  notice: string | null
  /** Set when status is 'error' (the session could not be checked). */
  errorMessage: string | null
}

export interface AuthValue extends AuthState {
  login: (email: string, password: string) => Promise<void>
  register: (input: RegisterInput) => Promise<OrganizationWithRole>
  logout: () => Promise<void>
  addOrganization: (organization: OrganizationWithRole) => void
  retry: () => void
}

export const AuthContext = createContext<AuthValue | null>(null)

export function useAuth(): AuthValue {
  const value = useContext(AuthContext)
  if (!value) throw new Error('useAuth must be used inside AuthProvider')
  return value
}
