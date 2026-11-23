// Shapes of the operational read models (internal/delivery/operations.go and queue_age.go). Every number is a
// delivery count. A count that reaches 100,000 is saturated: it means "at least 100,000" (see lib/format.ts).

/** The queue health of one organization at generatedAt. */
export interface QueueSnapshot {
  generatedAt: string
  /** Every PENDING delivery, delayed ones included. */
  pending: number
  /** PENDING with a scheduled time that has been reached. Due does not mean claimable. */
  due: number
  /** PENDING with a scheduled time still in the future. */
  delayed: number
  /** Exact age of the longest-waiting due delivery. 0 when none is due. This is a minimum, not a count. */
  oldestDueAgeSeconds: number
  /** Every IN_FLIGHT delivery, expired leases included. */
  inFlight: number
  /** IN_FLIGHT deliveries whose lease has expired and that await recovery. */
  expiredLeases: number
  capped: boolean
}

/** One fixed range of overdue time. minSeconds is inclusive, maxSeconds exclusive and null for the last bucket. */
export interface AgeBucket {
  label: string
  minSeconds: number
  maxSeconds: number | null
  count: number
}

/** How long due deliveries have been overdue, in fixed buckets, at generatedAt. */
export interface QueueAge {
  generatedAt: string
  buckets: AgeBucket[]
  capped: boolean
}

// Delivery activity (internal/delivery/operations.go, failures.go, attempt_activity.go). Enumerated values are typed
// as plain strings on purpose: a value the backend adds later is shown as text instead of breaking the page.

/** The five persisted failure reasons, plus UNRECORDED (a failed delivery whose reason was never recorded). */
export const FAILURE_REASONS = [
  'DESTINATION_REJECTED',
  'REDIRECT_REFUSED',
  'EGRESS_BLOCKED',
  'RETRIES_EXHAUSTED',
  'INTERNAL_ERROR',
  'UNRECORDED',
] as const

/** The seven attempt outcomes. */
export const ATTEMPT_OUTCOMES = [
  'SUCCESS',
  'HTTP_ERROR',
  'REDIRECT',
  'NETWORK_ERROR',
  'TIMEOUT',
  'BLOCKED_BY_POLICY',
  'ABANDONED',
] as const

export interface StatusCounts {
  PENDING: number
  IN_FLIGHT: number
  SUCCEEDED: number
  FAILED: number
  CANCELLED: number
}

export interface ReasonCounts {
  DESTINATION_REJECTED: number
  REDIRECT_REFUSED: number
  EGRESS_BLOCKED: number
  RETRIES_EXHAUSTED: number
  INTERNAL_ERROR: number
  UNRECORDED: number
}

/** Delivery counts for [from, to). Two cohorts on two columns: created_at (Created) and completed_at (Completed). */
export interface DeliverySummary {
  generatedAt: string
  from: string
  to: string
  created: { total: number; byStatus: StatusCounts }
  completed: {
    succeeded: number
    failed: number
    cancelled: number
    /** null when nothing completed, or when a count is saturated. */
    failureRate: number | null
    byFailureReason: ReasonCounts
  }
  capped: boolean
}

/** One delivery that is FAILED right now. */
export interface FailureItem {
  deliveryId: string
  eventId: string
  destinationId: string
  destinationName: string
  failureReason: string
  attemptCount: number
  lastHttpStatus: number | null
  lastErrorClass: string | null
  createdAt: string
  completedAt: string
}

/** One recorded attempt. Only stored fields: nothing here is derived. */
export interface AttemptItem {
  attemptId: string
  deliveryId: string
  eventId: string
  destinationId: string
  destinationName: string
  attemptNumber: number
  startedAt: string
  finishedAt: string
  durationMs: number
  outcome: string
  httpStatus: number | null
  errorClass: string | null
  /** Set only on an attempt that ended a failure episode of its delivery. */
  terminalReason: string | null
}

/** One page of a cursor feed. nextCursor is opaque and null on the last page. */
export interface FeedPage<T> {
  generatedAt: string
  from: string
  to: string
  items: T[]
  nextCursor: string | null
}

export type FailureFeed = FeedPage<FailureItem>
export type AttemptFeed = FeedPage<AttemptItem>

/** What a destination selector may know: never the URL. */
export interface DestinationOption {
  id: string
  name: string
  status: string
}

export interface DestinationPage {
  items: DestinationOption[]
  total: number
  limit: number
  offset: number
}
