import type { QueueSnapshot } from '../../api/operationsTypes'

// The wording of the queue page. Each sentence says what the backend counts and nothing more (internal/delivery/
// operations.go). The queue has no notion of "healthy" or "stuck" and the page must not suggest one.

export interface QueueMetric {
  key: keyof Pick<QueueSnapshot, 'pending' | 'due' | 'delayed' | 'inFlight' | 'expiredLeases'>
  label: string
  help: string
}

export const QUEUE_METRICS: readonly QueueMetric[] = [
  {
    key: 'pending',
    label: 'Pending',
    help: 'All PENDING deliveries, including delayed retries.',
  },
  {
    key: 'due',
    label: 'Due',
    help: "Scheduled time reached. A paused destination's deliveries still count as due.",
  },
  {
    key: 'delayed',
    label: 'Delayed',
    help: 'PENDING deliveries whose scheduled time is still in the future.',
  },
  {
    key: 'inFlight',
    label: 'In flight',
    help: 'All IN_FLIGHT deliveries, including those whose lease has expired.',
  },
  {
    key: 'expiredLeases',
    label: 'Expired leases',
    help: 'Lease expired and awaiting recovery. This does not establish that the worker has stopped running.',
  },
]

export const OLDEST_DUE = {
  label: 'Oldest due age',
  help: 'How long the longest-waiting due delivery has been waiting past its scheduled time.',
  none: 'None due',
}

export const COUNTS_NOTE = 'These are delivery counts, not attempt counts.'

export const LOWER_BOUND_NOTE =
  'Counts marked ≥ 100,000 are lower bounds: the real number is at least that. Every other count is exact.'

export const TWO_READS_NOTE =
  'The queue snapshot and the overdue-age distribution are two separate reads, so their numbers can differ slightly.'

export const AGE_EXPLANATION =
  "Overdue age is the time since a due delivery's scheduled time (its next attempt time), not the time since it was created. A retried delivery's clock restarts when its retry becomes due."
