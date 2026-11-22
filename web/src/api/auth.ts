import { request, setCsrfToken } from './client'
import type { AuthResult } from './types'

function keepToken(result: AuthResult): AuthResult {
  setCsrfToken(result.session.csrfToken ?? null)
  return result
}

export async function login(email: string, password: string): Promise<AuthResult> {
  const result = await request<AuthResult>('/auth/login', {
    method: 'POST',
    body: { email, password },
    csrf: false,
    quiet401: true,
  })
  return keepToken(result)
}

export interface RegisterInput {
  email: string
  password: string
  displayName: string
  organizationName: string
  organizationSlug: string
}

export async function register(input: RegisterInput): Promise<AuthResult> {
  const result = await request<AuthResult>('/auth/register', {
    method: 'POST',
    body: input,
    csrf: false,
    quiet401: true,
  })
  return keepToken(result)
}

export function logout(): Promise<void> {
  return request<void>('/auth/logout', { method: 'POST', quiet401: true })
}

/** The current user and organizations. It does not return a CSRF token. */
export function me(signal?: AbortSignal, quiet401 = false): Promise<AuthResult> {
  return request<AuthResult>('/auth/me', { signal, quiet401 })
}
