import { useEffect, useRef, useState } from 'react'
import { UNREADABLE_SUCCESS } from '../../api/deliveries'
import { isAbortError, isApiError } from '../../api/errors'

/** The client stops waiting after this long. The result is then unknown (ambiguous). */
export const SUBSCRIPTION_MUTATION_TIMEOUT_MS = 30_000

/** An error the dialog can show and fix: the request was rejected and nothing changed. */
export interface SubscriptionMutationError {
  message: string
  /** The form field the message belongs to, or null for the form as a whole. */
  field: 'endpoint' | 'destination' | null
  /** The API error code, so the owner can offer a next step (for example for `already_subscribed`). */
  code: string
}

/** What to do with a failed request. Only `stay` keeps the dialog open. */
export type SubscriptionReaction =
  | { kind: 'stay'; error: SubscriptionMutationError }
  /** 403: the user may no longer do this. Nothing changed. */
  | { kind: 'forbidden' }
  /** 404: the subscription, endpoint or destination does not exist in this organization. Nothing changed. */
  | { kind: 'gone' }
  /** 409 subscription_revoked: it was revoked before. Nothing changed. */
  | { kind: 'alreadyRevoked' }
  /** The result is unknown: it may have been applied. Never repeat the request; read the state once. */
  | { kind: 'ambiguous' }

/**
 * Classifies an error of a subscription mutation. Known rejections come from the handler's JSON error envelope (or
 * the CSRF check before it), so nothing changed. A lost connection, a timeout, an unreadable 2xx, a 5xx (a proxy
 * 502 or 504, an internal error) or an error of an unknown kind is ambiguous.
 */
export function classifySubscriptionError(error: unknown, timedOut: boolean): SubscriptionReaction {
  if (isAbortError(error)) return { kind: 'ambiguous' }
  if (!isApiError(error)) return { kind: 'ambiguous' }
  if (timedOut || error.status === 0 || error.code === UNREADABLE_SUCCESS) {
    return { kind: 'ambiguous' }
  }
  const stay = (field: SubscriptionMutationError['field'], message = error.message) =>
    ({ kind: 'stay', error: { message, field, code: error.code } }) as const
  switch (`${error.status} ${error.code}`) {
    case '409 already_subscribed':
      return stay(null)
    case '409 endpoint_revoked':
      return stay('endpoint')
    case '409 destination_revoked':
      return stay('destination')
    case '403 csrf_invalid':
      return stay(
        null,
        'Your session security check failed. Reload the page and try again. Nothing was changed.',
      )
    case '403 forbidden':
      return { kind: 'forbidden' }
    case '404 not_found':
      return { kind: 'gone' }
    case '409 subscription_revoked':
      return { kind: 'alreadyRevoked' }
  }
  if (error.status >= 500) return { kind: 'ambiguous' }
  // Any other answer of the API before the handler could change anything (400, 413, 415, 429, 401).
  return stay(null)
}

interface Handlers<T> {
  onSuccess: (result: T) => void
  /** Called for every reaction except `stay`, which the hook shows itself. */
  onReaction: (reaction: Exclude<SubscriptionReaction, { kind: 'stay' }>) => void
}

/**
 * Runs one subscription mutation at a time. A synchronous guard blocks a second submission in the same tick (double
 * click, Enter plus click), the request has a 30 second timeout, and leaving (unmount or a new `scope`) aborts it
 * and ignores its result. The request is never repeated here: an ambiguous result goes to `onReaction` and the
 * owner reads the state once.
 */
export function useSubscriptionMutation(scope: string) {
  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState<SubscriptionMutationError | null>(null)
  const inFlight = useRef(false)
  const active = useRef<{ controller: AbortController; cancelled: boolean } | null>(null)

  useEffect(() => {
    return () => {
      if (active.current) {
        active.current.cancelled = true
        active.current.controller.abort()
        active.current = null
      }
      inFlight.current = false
    }
  }, [scope])

  const run = async <T>(call: (signal: AbortSignal) => Promise<T>, handlers: Handlers<T>) => {
    if (inFlight.current) return
    inFlight.current = true
    setSubmitting(true)
    setError(null)
    const current = { controller: new AbortController(), cancelled: false }
    active.current = current
    let timedOut = false
    const timer = setTimeout(() => {
      timedOut = true
      current.controller.abort()
    }, SUBSCRIPTION_MUTATION_TIMEOUT_MS)
    try {
      const result = await call(current.controller.signal)
      clearTimeout(timer)
      if (current.cancelled) return
      inFlight.current = false
      active.current = null
      setSubmitting(false)
      handlers.onSuccess(result)
    } catch (caught) {
      clearTimeout(timer)
      if (current.cancelled) return
      inFlight.current = false
      active.current = null
      setSubmitting(false)
      const reaction = classifySubscriptionError(caught, timedOut)
      if (reaction.kind === 'stay') setError(reaction.error)
      else handlers.onReaction(reaction)
    }
  }

  return { submitting, error, run, clearError: () => setError(null) }
}
