import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import type { ReactNode } from 'react'
import * as authApi from '../api/auth'
import type { RegisterInput } from '../api/auth'
import { resetClient, setIdentityGuard, setUnauthenticatedHandler } from '../api/client'
import { ApiError, isAbortError, isApiError, userMessage } from '../api/errors'
import type { AuthResult, OrganizationWithRole } from '../api/types'
import { useToast } from '../ui/toastContext'
import { AuthContext } from './authContext'
import type { AuthState, AuthValue } from './authContext'
import { openAuthChannel } from './crossTab'
import type { AuthChannel } from './crossTab'

const LOADING: AuthState = {
  status: 'loading',
  user: null,
  organizations: [],
  expiresAt: null,
  notice: null,
  errorMessage: null,
}

const DIFFERENT_USER = 'You signed in as a different user in another tab. Sign in again here.'

const WARNING_LEAD_MS = 5 * 60 * 1000
const MAX_TIMER_MS = 2 ** 31 - 1

/** Runs callback at `at` (epoch ms). setTimeout cannot wait longer than 24.8 days, so long waits re-arm. */
function runAt(at: number, callback: () => void): () => void {
  let handle = 0
  const arm = () => {
    const wait = at - Date.now()
    if (wait <= 0) {
      callback()
      return
    }
    handle = window.setTimeout(arm, Math.min(wait, MAX_TIMER_MS))
  }
  arm()
  return () => window.clearTimeout(handle)
}

export function AuthProvider({ children }: { children: ReactNode }) {
  const toast = useToast()
  const [state, setState] = useState<AuthState>(LOADING)
  const [attempt, setAttempt] = useState(0)
  const userId = useRef<string | null>(null)
  const channel = useRef<AuthChannel | null>(null)

  const apply = useCallback((result: AuthResult, organizations?: OrganizationWithRole[]) => {
    userId.current = result.user.id
    setState({
      status: 'authenticated',
      user: result.user,
      organizations: organizations ?? result.organizations ?? [],
      expiresAt: result.session.expiresAt,
      notice: null,
      errorMessage: null,
    })
  }, [])

  // Forgets everything about the signed-in user, including the CSRF token held by the API client.
  const clear = useCallback((notice: string | null) => {
    userId.current = null
    resetClient()
    setState({ ...LOADING, status: 'anonymous', notice })
  }, [])

  // Re-reads who the cookie belongs to and returns true when it is still this tab's user. If it is someone else
  // (another tab signed in as another user), this tab clears itself: it must not keep showing or acting for the
  // previous user. A failed read (a 401 is already handled by the unauthenticated handler) is thrown to the caller.
  const checkIdentity = useCallback(async (): Promise<boolean> => {
    if (userId.current === null) return true
    const result = await authApi.me()
    if (userId.current === null) return true
    if (result.user.id !== userId.current) {
      clear(DIFFERENT_USER)
      return false
    }
    apply(result)
    return true
  }, [apply, clear])

  // For events that only suggest the session may have changed: a failed read leaves the state as it is.
  const verifyIdentity = useCallback(async () => {
    try {
      await checkIdentity()
    } catch {
      // Nothing to do: the next request reports a lost session itself.
    }
  }, [checkIdentity])

  // Awaited by the API client before it sends a mutation under a token this tab has just obtained. If the session
  // changed hands, the mutation is refused here, so it can never run as another user.
  const guardMutation = useCallback(async () => {
    if (!(await checkIdentity())) throw new ApiError(409, 'session_changed', DIFFERENT_USER)
  }, [checkIdentity])

  // Bootstrap: who am I? (GET /auth/me needs no CSRF token; one is fetched lazily by the first mutation.)
  useEffect(() => {
    const controller = new AbortController()
    authApi
      .me(controller.signal, true)
      .then((result) => apply(result))
      .catch((error: unknown) => {
        if (controller.signal.aborted || isAbortError(error)) return
        if (isApiError(error) && error.status === 401) {
          clear(null)
          return
        }
        setState({ ...LOADING, status: 'error', errorMessage: userMessage(error) })
      })
    return () => controller.abort()
  }, [apply, clear, attempt])

  useEffect(() => {
    const opened = openAuthChannel((signal) => {
      if (signal === 'signed-out') {
        clear('You were signed out in another tab.')
      } else {
        void verifyIdentity()
      }
    })
    channel.current = opened
    setUnauthenticatedHandler(() => {
      clear('Your session has ended. Sign in again.')
      opened.post('signed-out')
    })
    setIdentityGuard(guardMutation)
    const onVisible = () => {
      if (document.visibilityState === 'visible' && userId.current !== null) void verifyIdentity()
    }
    document.addEventListener('visibilitychange', onVisible)
    return () => {
      document.removeEventListener('visibilitychange', onVisible)
      setUnauthenticatedHandler(null)
      setIdentityGuard(null)
      opened.close()
      channel.current = null
    }
  }, [clear, verifyIdentity, guardMutation])

  // The session has an absolute lifetime (ADR 8) and is never extended, so warn before it ends and then sign out.
  const expiresAt = state.expiresAt
  useEffect(() => {
    if (expiresAt === null) return undefined
    const end = Date.parse(expiresAt)
    if (Number.isNaN(end)) return undefined
    const stopWarning = runAt(end - WARNING_LEAD_MS, () => {
      if (end - Date.now() > 0) {
        toast.push(
          'info',
          'Your session will end soon. Finish what you are doing, then sign in again.',
        )
      }
    })
    const stopExpiry = runAt(end, () => clear('Your session expired. Sign in again.'))
    return () => {
      stopWarning()
      stopExpiry()
    }
  }, [expiresAt, clear, toast])

  const login = useCallback(
    async (email: string, password: string) => {
      const result = await authApi.login(email, password)
      apply(result)
      channel.current?.post('signed-in')
    },
    [apply],
  )

  const register = useCallback(
    async (input: RegisterInput) => {
      const result = await authApi.register(input)
      if (!result.organization) throw new Error('The registration response had no organization')
      const organization: OrganizationWithRole = { ...result.organization, role: 'OWNER' }
      apply(result, [organization])
      channel.current?.post('signed-in')
      return organization
    },
    [apply],
  )

  const logout = useCallback(async () => {
    try {
      await authApi.logout()
    } catch (error) {
      // The session now belongs to another user: this tab is already cleared, and the server session (theirs) must
      // be left alone, so there is nothing to confirm or to announce to other tabs.
      if (isApiError(error) && error.code === 'session_changed') return
      if (!isApiError(error) || error.status !== 401) {
        toast.push(
          'error',
          'Signed out on this device, but the server could not confirm it. The session ends when it expires.',
        )
      }
    }
    clear(null)
    channel.current?.post('signed-out')
  }, [clear, toast])

  const addOrganization = useCallback((organization: OrganizationWithRole) => {
    setState((current) => ({ ...current, organizations: [...current.organizations, organization] }))
  }, [])

  const retry = useCallback(() => {
    setState(LOADING)
    setAttempt((n) => n + 1)
  }, [])

  const value = useMemo<AuthValue>(
    () => ({ ...state, login, register, logout, addOrganization, retry }),
    [state, login, register, logout, addOrganization, retry],
  )
  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>
}
