import { useEffect, useRef, useState } from 'react'
import { UNREADABLE_SUCCESS } from '../../api/deliveries'
import { isAbortError, isApiError } from '../../api/errors'

/** The client stops waiting after this long. The result is then unknown (ambiguous). */
export const MUTATION_TIMEOUT_MS = 30_000

/** An error the dialog can show and fix: the request was rejected and nothing changed. */
export interface MutationError {
  message: string
  /** The form field the message belongs to, or null for the form as a whole. */
  field: 'name' | 'url' | null
}

/** What to do with a failed request. Only `stay` keeps the dialog open. */
export type Reaction =
  | { kind: 'stay'; error: MutationError }
  /** 403: the user may no longer do this. Nothing changed. */
  | { kind: 'forbidden' }
  /** 404: the destination does not exist (any more) in this organization. Nothing changed. */
  | { kind: 'gone' }
  /** 409 destination_revoked: the destination is revoked and immutable. Nothing changed. */
  | { kind: 'revoked' }
  /** The result is unknown: it may have been applied. Never repeat the request; read the state once. */
  | { kind: 'ambiguous' }

/**
 * Classifies an error of a destination mutation. Known rejections come from the handler's JSON error envelope (or
 * the CSRF check before it), so nothing changed. A lost connection, a timeout, an unreadable 2xx, a 5xx (a proxy
 * 502 or 504, an internal error) or an error of an unknown kind is ambiguous.
 */
export function classifyMutationError(error: unknown, timedOut: boolean): Reaction {
  if (isAbortError(error)) return { kind: 'ambiguous' }
  if (!isApiError(error)) return { kind: 'ambiguous' }
  if (timedOut || error.status === 0 || error.code === UNREADABLE_SUCCESS) {
    return { kind: 'ambiguous' }
  }
  const key = `${error.status} ${error.code}`
  switch (key) {
    case '400 validation_failed': {
      const text = error.message.toLowerCase()
      const field = text.includes('url') ? 'url' : text.includes('name') ? 'name' : null
      return { kind: 'stay', error: { message: error.message, field } }
    }
    case '409 destination_name_taken':
      return { kind: 'stay', error: { message: error.message, field: 'name' } }
    case '403 csrf_invalid':
      return {
        kind: 'stay',
        error: {
          message:
            'Your session security check failed. Reload the page and try again. Nothing was changed.',
          field: null,
        },
      }
    case '403 forbidden':
      return { kind: 'forbidden' }
    case '404 not_found':
      return { kind: 'gone' }
    case '409 destination_revoked':
      return { kind: 'revoked' }
    case '503 lock_timeout':
      return {
        kind: 'stay',
        error: {
          message: 'The server was busy and did not start the change. Try again.',
          field: null,
        },
      }
  }
  if (error.status >= 500) return { kind: 'ambiguous' }
  // Any other answer of the API before the handler could change anything (400 invalid_json, 413, 429, 401).
  return { kind: 'stay', error: { message: error.message, field: null } }
}

interface Handlers<T> {
  onSuccess: (result: T) => void
  /** Called for every reaction except `stay`, which the hook shows itself. */
  onReaction: (reaction: Exclude<Reaction, { kind: 'stay' }>) => void
}

/**
 * Runs one destination mutation at a time. A synchronous guard blocks a second submission in the same tick (double
 * click, Enter plus click), the request has a 30 second timeout, and leaving (unmount or a new `scope`) aborts it
 * and ignores its result. The request is never repeated here: an ambiguous result goes to `onReaction` and the
 * owner reads the state once.
 */
export function useDestinationMutation(scope: string) {
  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState<MutationError | null>(null)
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
    const run = { controller: new AbortController(), cancelled: false }
    active.current = run
    let timedOut = false
    const timer = setTimeout(() => {
      timedOut = true
      run.controller.abort()
    }, MUTATION_TIMEOUT_MS)
    try {
      const result = await call(run.controller.signal)
      clearTimeout(timer)
      if (run.cancelled) return
      inFlight.current = false
      active.current = null
      setSubmitting(false)
      handlers.onSuccess(result)
    } catch (caught) {
      clearTimeout(timer)
      if (run.cancelled) return
      inFlight.current = false
      active.current = null
      setSubmitting(false)
      const reaction = classifyMutationError(caught, timedOut)
      if (reaction.kind === 'stay') setError(reaction.error)
      else handlers.onReaction(reaction)
    }
  }

  return { submitting, error, run, clearError: () => setError(null) }
}
