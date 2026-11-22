// Shapes of the JSON the backend actually returns (internal/httpapi, internal/tenancy, internal/identity).

export type Role = 'OWNER' | 'ADMIN' | 'DEVELOPER' | 'VIEWER'

export interface User {
  id: string
  email: string
  displayName: string
  status: string
  createdAt: string
}

export interface SessionInfo {
  expiresAt: string
  /** Present only in the register and login responses. */
  csrfToken?: string
}

export interface Organization {
  id: string
  name: string
  slug: string
  createdAt: string
  updatedAt: string
}

export interface OrganizationWithRole extends Organization {
  role: Role
}

/** The body of register, login and GET /auth/me. The backend omits empty optional fields. */
export interface AuthResult {
  user: User
  session: SessionInfo
  organization?: Organization
  organizations?: OrganizationWithRole[]
}
