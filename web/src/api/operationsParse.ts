import { ApiError } from './errors'
import type {
  AttemptFeed,
  AttemptItem,
  DeliverySummary,
  DestinationOption,
  DestinationPage,
  FailureFeed,
  FailureItem,
  FeedPage,
  ReasonCounts,
  StatusCounts,
} from './operationsTypes'

// A light shape check of the responses of the activity endpoints. It verifies that the fields this app reads exist
// and have the expected primitive type, so a malformed body becomes an error state instead of a crash or a wrong
// number. It does not restrict enumerated values (a new outcome or reason is shown as text) and it copies only the
// fields it names, so nothing else the server sends (such as a destination URL) is ever kept.

const MESSAGE = 'The server returned a response this page could not read.'

function invalid(): never {
  throw new ApiError(200, 'invalid_response', MESSAGE)
}

type Raw = Record<string, unknown>

function record(value: unknown): Raw {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) return invalid()
  return value as Raw
}

function text(source: Raw, key: string): string {
  const value = source[key]
  return typeof value === 'string' ? value : invalid()
}

function count(source: Raw, key: string): number {
  const value = source[key]
  return typeof value === 'number' && Number.isFinite(value) && value >= 0 ? value : invalid()
}

function optionalText(source: Raw, key: string): string | null {
  const value = source[key]
  if (value === undefined || value === null) return null
  return typeof value === 'string' ? value : invalid()
}

function optionalNumber(source: Raw, key: string): number | null {
  const value = source[key]
  if (value === undefined || value === null) return null
  return typeof value === 'number' && Number.isFinite(value) ? value : invalid()
}

function flag(source: Raw, key: string): boolean {
  const value = source[key]
  return typeof value === 'boolean' ? value : invalid()
}

function array(source: Raw, key: string): unknown[] {
  const value = source[key]
  return Array.isArray(value) ? value : invalid()
}

function statusCounts(value: unknown): StatusCounts {
  const source = record(value)
  return {
    PENDING: count(source, 'PENDING'),
    IN_FLIGHT: count(source, 'IN_FLIGHT'),
    SUCCEEDED: count(source, 'SUCCEEDED'),
    FAILED: count(source, 'FAILED'),
    CANCELLED: count(source, 'CANCELLED'),
  }
}

function reasonCounts(value: unknown): ReasonCounts {
  const source = record(value)
  return {
    DESTINATION_REJECTED: count(source, 'DESTINATION_REJECTED'),
    REDIRECT_REFUSED: count(source, 'REDIRECT_REFUSED'),
    EGRESS_BLOCKED: count(source, 'EGRESS_BLOCKED'),
    RETRIES_EXHAUSTED: count(source, 'RETRIES_EXHAUSTED'),
    INTERNAL_ERROR: count(source, 'INTERNAL_ERROR'),
    UNRECORDED: count(source, 'UNRECORDED'),
  }
}

export function parseDeliverySummary(value: unknown): DeliverySummary {
  const source = record(value)
  const created = record(source.created)
  const completed = record(source.completed)
  return {
    generatedAt: text(source, 'generatedAt'),
    from: text(source, 'from'),
    to: text(source, 'to'),
    created: { total: count(created, 'total'), byStatus: statusCounts(created.byStatus) },
    completed: {
      succeeded: count(completed, 'succeeded'),
      failed: count(completed, 'failed'),
      cancelled: count(completed, 'cancelled'),
      failureRate: optionalNumber(completed, 'failureRate'),
      byFailureReason: reasonCounts(completed.byFailureReason),
    },
    capped: flag(source, 'capped'),
  }
}

function page<T>(value: unknown, item: (raw: unknown) => T): FeedPage<T> {
  const source = record(value)
  return {
    generatedAt: text(source, 'generatedAt'),
    from: text(source, 'from'),
    to: text(source, 'to'),
    items: array(source, 'items').map(item),
    nextCursor: nextCursor(source),
  }
}

/** The cursor of the next page: a non-empty string, or null on the last page. An empty string is malformed. */
function nextCursor(source: Raw): string | null {
  const value = optionalText(source, 'nextCursor')
  return value === '' ? invalid() : value
}

function failureItem(value: unknown): FailureItem {
  const source = record(value)
  return {
    deliveryId: text(source, 'deliveryId'),
    eventId: text(source, 'eventId'),
    destinationId: text(source, 'destinationId'),
    destinationName: text(source, 'destinationName'),
    failureReason: text(source, 'failureReason'),
    attemptCount: count(source, 'attemptCount'),
    lastHttpStatus: optionalNumber(source, 'lastHttpStatus'),
    lastErrorClass: optionalText(source, 'lastErrorClass'),
    createdAt: text(source, 'createdAt'),
    completedAt: text(source, 'completedAt'),
  }
}

function attemptItem(value: unknown): AttemptItem {
  const source = record(value)
  return {
    attemptId: text(source, 'attemptId'),
    deliveryId: text(source, 'deliveryId'),
    eventId: text(source, 'eventId'),
    destinationId: text(source, 'destinationId'),
    destinationName: text(source, 'destinationName'),
    attemptNumber: count(source, 'attemptNumber'),
    startedAt: text(source, 'startedAt'),
    finishedAt: text(source, 'finishedAt'),
    durationMs: count(source, 'durationMs'),
    outcome: text(source, 'outcome'),
    httpStatus: optionalNumber(source, 'httpStatus'),
    errorClass: optionalText(source, 'errorClass'),
    terminalReason: optionalText(source, 'terminalReason'),
  }
}

export function parseFailureFeed(value: unknown): FailureFeed {
  return page(value, failureItem)
}

export function parseAttemptFeed(value: unknown): AttemptFeed {
  return page(value, attemptItem)
}

function destinationOption(value: unknown): DestinationOption {
  const source = record(value)
  // Only these three fields are read. The destination's url is deliberately never copied.
  return { id: text(source, 'id'), name: text(source, 'name'), status: text(source, 'status') }
}

export function parseDestinationPage(value: unknown): DestinationPage {
  const source = record(value)
  return {
    items: array(source, 'items').map(destinationOption),
    total: count(source, 'total'),
    limit: count(source, 'limit'),
    offset: count(source, 'offset'),
  }
}
