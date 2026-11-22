import { ATTEMPT_OUTCOMES, FAILURE_REASONS } from '../../api/operationsTypes'
import { formatUtc } from '../../lib/format'

// The wording of the Activity page. Each sentence says what the backend records and nothing more
// (internal/delivery/failure_reason.go, retry.go, attempt_activity.go). The page never says that a destination or
// the platform is "healthy", "stuck" or "dead".

export const DELIVERY_COUNTS_NOTE = 'These are delivery counts, not attempt counts.'

export const SUMMARY_INTRO =
  'Delivery counts for one window. The two groups below use different dates: when a delivery was created, and when it reached a final status.'

export const CREATED_NOTE =
  "Deliveries created in the window, grouped by each delivery's current status. This is not a count of what happened in the window."

export const COMPLETED_NOTE =
  'Deliveries that reached a final status in the window. A delivery that is redriven later leaves this group.'

export const FAILURE_RATE_HELP =
  'Failed divided by succeeded plus failed. Cancelled is not counted.'

export const FAILURE_RATE_UNAVAILABLE =
  'Not available: nothing completed in this window, or a count reached its limit.'

export const FAILURES_INTRO =
  'Deliveries that are failed right now, newest completion first. A redriven delivery leaves this list until it fails again.'

export const ATTEMPTS_INTRO =
  'Recorded attempts, newest start first. An attempt is one try to send a delivery. It is not the same as the delivery ending in failure.'

export const ATTEMPTS_NO_DESTINATION_NOTE =
  'Attempts can be filtered by delivery, not by destination.'

export const DELIVERY_WINDOW_NOTE =
  'Only attempts that started inside this section’s window are shown. Attempts of the delivery outside it are not included.'

/** Said next to the attempts of a delivery whose lifetime is longer than the 31 days the attempt feed can span. */
export function truncatedAttemptsWarning(createdAt: string): string {
  return `Older attempts are excluded. This delivery was created ${formatUtc(createdAt)}, more than 31 days before its latest failure, and the attempt feed can look back at most 31 days. The attempts below are not the delivery's complete history.`
}

export const FEED_FRESHNESS_NOTE =
  'A page is one snapshot. Rows that finish late can be missing from later pages. Press Refresh to start again from the newest rows.'

export const DEFAULT_WINDOW_HINT =
  'Times are read as UTC. Leave both empty to use the default window.'

export const LOWER_BOUND_NOTE =
  'Counts marked ≥ 100,000 are lower bounds: the real number is at least that. Every other count is exact.'

export const SUMMARY_TIMEOUT_HINT = 'Try a shorter time window.'
export const FEED_TIMEOUT_HINT = 'Try a shorter time window or a narrower filter.'

interface Described {
  label: string
  meaning: string
}

const REASONS: Record<(typeof FAILURE_REASONS)[number], Described> = {
  DESTINATION_REJECTED: {
    label: 'Destination rejected',
    meaning: 'The destination answered with a final 4xx response.',
  },
  REDIRECT_REFUSED: {
    label: 'Redirect refused',
    meaning: 'The destination answered with a redirect. Redirects are never followed.',
  },
  EGRESS_BLOCKED: {
    label: 'Egress blocked',
    meaning: 'The egress guard refused the destination address or URL.',
  },
  RETRIES_EXHAUSTED: {
    label: 'Retries exhausted',
    meaning: 'The attempt budget ran out while results were still retryable.',
  },
  INTERNAL_ERROR: {
    label: 'Internal error',
    meaning: 'A platform fault, or a final result no other reason describes.',
  },
  UNRECORDED: {
    label: 'Not recorded',
    meaning: 'The delivery failed before failure reasons were recorded.',
  },
}

const OUTCOMES: Record<(typeof ATTEMPT_OUTCOMES)[number], Described> = {
  SUCCESS: { label: 'Success', meaning: 'A 2xx response.' },
  HTTP_ERROR: { label: 'HTTP error', meaning: 'A 4xx or 5xx response.' },
  REDIRECT: { label: 'Redirect', meaning: 'A 3xx response. Redirects are never followed.' },
  NETWORK_ERROR: { label: 'Network error', meaning: 'A connection, TLS or protocol failure.' },
  TIMEOUT: { label: 'Timeout', meaning: 'A deadline expired.' },
  BLOCKED_BY_POLICY: {
    label: 'Blocked by policy',
    meaning: 'The egress guard refused the address.',
  },
  ABANDONED: {
    label: 'Abandoned',
    meaning: 'A worker did not finish this attempt. This does not mean the destination failed.',
  },
}

function lookup(table: Record<string, Described>, value: string): Described {
  return table[value] ?? { label: value, meaning: 'A value this page does not describe.' }
}

export function reasonInfo(value: string): Described {
  return lookup(REASONS, value)
}

export function outcomeInfo(value: string): Described {
  return lookup(OUTCOMES, value)
}

export const REASON_OPTIONS = FAILURE_REASONS.map((value) => ({
  value,
  label: REASONS[value].label,
}))

export const OUTCOME_OPTIONS = ATTEMPT_OUTCOMES.map((value) => ({
  value,
  label: OUTCOMES[value].label,
}))
