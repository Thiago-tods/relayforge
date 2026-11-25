import { isAbortError, isApiError, userMessage } from '../../api/errors'
import { MAX_ATTEMPTS_CEILING, REDRIVE_BUDGET_STEP, UNREADABLE_SUCCESS } from '../../api/deliveries'
import type { DeliveryRecord, RedriveResult } from '../../api/deliveryTypes'
import type { RedriveFlow } from './useRedrive'

// What the redrive UI says and does for each result of the redrive request (ADR 34). Confirmed behavior comes from
// the ADR and the backend; figures computed here are estimates and never replace the server's answer.

export { MAX_ATTEMPTS_CEILING, REDRIVE_BUDGET_STEP }

/** The budget the UI expects after a redrive. Advisory only: the server's `redrive.maxAttempts` is authoritative. */
export function expectedBudget(maxAttempts: number): number {
  return Math.min(maxAttempts + REDRIVE_BUDGET_STEP, MAX_ATTEMPTS_CEILING)
}

/** Whether the budget looks exhausted. Advisory: the server still answers retry_budget_exhausted on its own. */
export function budgetLooksExhausted(maxAttempts: number): boolean {
  return maxAttempts >= MAX_ATTEMPTS_CEILING
}

export type Tone = 'success' | 'info' | 'error'

export interface Outcome {
  tone: Tone
  title: string
  message: string
}

/** Why the result of a redrive request is unknown. */
export type AmbiguityCause = 'timeout' | 'network' | 'unreadable' | 'server'

/** What to do with an error of the redrive request. */
export type Reaction =
  /** The request was rejected before anything changed. The dialog stays open so the user can correct and retry. */
  | { kind: 'stay'; message: string; field: boolean }
  /** The request was rejected before anything changed and the dialog has nothing more to offer. */
  | { kind: 'close'; outcome: Outcome; refresh: boolean; permissionLost: boolean }
  /** The request may or may not have been applied. Check the state; never repeat the request. */
  | { kind: 'ambiguous'; cause: AmbiguityCause }

function closing(outcome: Outcome, refresh: boolean, permissionLost = false): Reaction {
  return { kind: 'close', outcome, refresh, permissionLost }
}

/**
 * Classifies an error of the redrive request. Known rejections come from the handler's JSON error envelope (or from
 * the CSRF check before it), so nothing was changed. Everything else is ambiguous: a lost connection, a timeout, an
 * unreadable success answer, a generic 5xx from the server or a proxy, or an error of an unknown kind.
 */
export function reactToError(error: unknown, timedOut: boolean): Reaction {
  if (isAbortError(error)) return { kind: 'ambiguous', cause: timedOut ? 'timeout' : 'network' }
  if (!isApiError(error)) return { kind: 'ambiguous', cause: 'server' }
  if (error.status === 0) return { kind: 'ambiguous', cause: 'network' }
  if (error.code === UNREADABLE_SUCCESS) return { kind: 'ambiguous', cause: 'unreadable' }

  switch (`${error.status} ${error.code}`) {
    case '400 validation_failed':
      return { kind: 'stay', message: error.message, field: true }
    case '403 csrf_invalid':
      return {
        kind: 'stay',
        message:
          'Your session security check failed. Reload the page and try again. Nothing was changed.',
        field: false,
      }
    case '403 forbidden':
      return closing(
        {
          tone: 'error',
          title: 'Not permitted',
          message:
            'You no longer have permission to redrive deliveries in this organization. Nothing was changed.',
        },
        false,
        true,
      )
    case '404 not_found':
      return closing(
        {
          tone: 'error',
          title: 'Delivery not found',
          message:
            'This delivery was not found in this organization, or you do not have access to it. Nothing was changed.',
        },
        false,
      )
    case '409 delivery_not_failed':
      return closing(
        {
          tone: 'info',
          title: 'The delivery is no longer failed',
          message:
            'It is no longer FAILED, so this request changed nothing. It may already have been redriven. The latest state is shown below.',
        },
        true,
      )
    case '409 delivery_changed':
      return closing(
        {
          tone: 'info',
          title: 'The delivery changed',
          message:
            'It changed since you opened the dialog, so this request changed nothing. Review the latest state before you redrive it again.',
        },
        true,
      )
    case '409 destination_revoked':
      return closing(
        {
          tone: 'error',
          title: 'The destination is revoked',
          message:
            'A revoked destination cannot receive this delivery again, so it cannot be redriven. Nothing was changed.',
        },
        false,
      )
    case '409 retry_budget_exhausted':
      return closing(
        {
          tone: 'error',
          title: 'Retry budget exhausted',
          message:
            'This delivery already has the maximum retry budget, so it cannot be redriven again. Nothing was changed.',
        },
        true,
      )
    case '409 event_payload_unavailable':
      return closing(
        {
          tone: 'error',
          title: 'Event payload unavailable',
          message:
            'The server cannot send this delivery again because its event payload is unavailable. This is a server-side problem: contact your administrator. Nothing was changed.',
        },
        false,
      )
    case '503 lock_timeout':
      return {
        kind: 'stay',
        message:
          'The server was busy and did not start the redrive. Nothing was changed. Try again.',
        field: false,
      }
  }

  // A server failure that is not a deliberate lock_timeout (a proxy 502 or 504, an internal error): the handler may
  // have committed before the answer was lost.
  if (error.status >= 500) return { kind: 'ambiguous', cause: 'server' }
  // Any other error answered by the API before the handler could change anything (400 invalid_json, 413, 429, 401).
  return { kind: 'stay', message: userMessage(error), field: false }
}

export function ambiguityMessage(cause: AmbiguityCause): string {
  switch (cause) {
    case 'timeout':
      return 'The server did not answer within 30 seconds.'
    case 'network':
      return 'The connection was lost before an answer arrived.'
    case 'unreadable':
      return 'The server answered, but the response could not be read.'
    case 'server':
      return 'The server returned an unexpected error.'
  }
}

/** What the page tells the user about a flow, outside the dialog. Null while there is nothing to report. */
export function noticeFor(flow: RedriveFlow): Outcome | null {
  switch (flow.phase) {
    case 'checking':
      return {
        tone: 'info',
        title: 'Checking the delivery state',
        message: `${ambiguityMessage(flow.cause)} The redrive may or may not have been applied. The current state is being checked.`,
      }
    case 'unverified':
      return {
        tone: 'error',
        title: 'State unverified',
        message: `${ambiguityMessage(flow.cause)} The redrive may or may not have been applied, and the current state could not be read, so this is not known. Redrive stays hidden until a check succeeds.`,
      }
    case 'outcome':
      return flow.outcome
    default:
      return null
  }
}

export function successOutcome(result: RedriveResult): Outcome {
  return {
    tone: 'success',
    title: 'Redrive accepted',
    message:
      `The delivery is now PENDING and its attempt budget changed from ${result.previousMaxAttempts} to ${result.maxAttempts}. ` +
      'The attempt count and attempt history are kept. Sending happens asynchronously: the first new attempt is due immediately, ' +
      'and later attempts follow the normal retry backoff. This does not guarantee that the destination accepts the delivery.',
  }
}

/**
 * Compares the delivery read after an ambiguous redrive with the snapshot taken when the dialog opened. The state can
 * suggest that a redrive happened, but it cannot prove that this particular request caused it (another user or the
 * worker may have), so the wording never claims authorship.
 */
export function compareAfterAmbiguity(snapshot: DeliveryRecord, latest: DeliveryRecord): Outcome {
  const unchanged =
    latest.status === 'FAILED' &&
    latest.attemptCount === snapshot.attemptCount &&
    latest.maxAttempts === snapshot.maxAttempts
  if (unchanged) {
    return {
      tone: 'info',
      title: 'The redrive does not appear to have been applied',
      message:
        'The delivery is still FAILED with the same attempt count and retry budget. If you still want to redrive it, open Redrive again and confirm with a new reason.',
    }
  }
  if (latest.status === 'FAILED' && latest.attemptCount > snapshot.attemptCount) {
    return {
      tone: 'info',
      title: 'The delivery was processed again and failed again',
      message: `It now shows ${latest.attemptCount} attempts (it showed ${snapshot.attemptCount}) and is FAILED. It was redriven, by this request or another, and the new attempts failed. You can open Redrive again for this new failure.`,
    }
  }
  if (
    (latest.status === 'PENDING' || latest.status === 'IN_FLIGHT') &&
    latest.maxAttempts > snapshot.maxAttempts
  ) {
    return {
      tone: 'info',
      title: 'The delivery appears to have been redriven',
      message: `It is ${latest.status} and its attempt budget rose from ${snapshot.maxAttempts} to ${latest.maxAttempts}. That matches a redrive, but the delivery cannot show whether this request or another one caused it. Sending happens asynchronously and does not guarantee delivery.`,
    }
  }
  if (latest.status === 'SUCCEEDED') {
    return {
      tone: 'info',
      title: 'The delivery is now SUCCEEDED',
      message:
        'The delivery is no longer FAILED. The state does not show whether a redrive caused this, so no redrive is offered.',
    }
  }
  if (latest.status === 'CANCELLED') {
    return {
      tone: 'info',
      title: 'The delivery is now CANCELLED',
      message:
        'The delivery is no longer FAILED. The state does not show whether a redrive caused this, so no redrive is offered.',
    }
  }
  return {
    tone: 'info',
    title: 'The delivery changed',
    message: `The delivery is now ${latest.status} with ${latest.attemptCount} of ${latest.maxAttempts} attempts (it showed FAILED with ${snapshot.attemptCount} of ${snapshot.maxAttempts}). The state does not show whether a redrive caused this.`,
  }
}
