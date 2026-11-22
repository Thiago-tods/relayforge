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
