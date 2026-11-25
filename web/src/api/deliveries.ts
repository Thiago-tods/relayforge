import { request } from './client'
import { ApiError, isApiError } from './errors'
import type {
  DeliveryAttempt,
  DeliveryDetail,
  DeliveryRecord,
  RedriveRequest,
  RedriveResult,
} from './deliveryTypes'

function record(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

function requiredString(value: unknown, field: string): string {
  if (typeof value !== 'string' || value.length === 0) throw invalid(`delivery.${field}`)
  return value
}

function nullableString(value: unknown, field: string): string | null {
  if (value === undefined || value === null) return null
  if (typeof value !== 'string') throw invalid(`delivery.${field}`)
  return value
}

function nullableNumber(value: unknown, field: string): number | null {
  if (value === undefined || value === null) return null
  if (typeof value !== 'number' || !Number.isInteger(value)) throw invalid(`delivery.${field}`)
  return value
}

function requiredInteger(value: unknown, field: string): number {
  if (typeof value !== 'number' || !Number.isInteger(value) || value < 0) throw invalid(field)
  return value
}

function invalid(field: string): ApiError {
  return new ApiError(
    500,
    'invalid_response',
    `The server returned an invalid delivery detail (${field}).`,
  )
}

function parseDelivery(value: unknown): DeliveryRecord {
  if (!record(value)) throw invalid('delivery')
  const createdAt = requiredString(value.createdAt, 'createdAt')
  const updatedAt = requiredString(value.updatedAt, 'updatedAt')
  if (Number.isNaN(Date.parse(createdAt)) || Number.isNaN(Date.parse(updatedAt)))
    throw invalid('timestamps')
  const nextAttemptAt = nullableString(value.nextAttemptAt, 'nextAttemptAt')
  const completedAt = nullableString(value.completedAt, 'completedAt')
  if (nextAttemptAt !== null && Number.isNaN(Date.parse(nextAttemptAt)))
    throw invalid('nextAttemptAt')
  if (completedAt !== null && Number.isNaN(Date.parse(completedAt))) throw invalid('completedAt')
  return {
    id: requiredString(value.id, 'id'),
    organizationId: requiredString(value.organizationId, 'organizationId'),
    eventId: requiredString(value.eventId, 'eventId'),
    destinationId: requiredString(value.destinationId, 'destinationId'),
    destinationName: requiredString(value.destinationName, 'destinationName'),
    status: requiredString(value.status, 'status'),
    attemptCount: requiredInteger(value.attemptCount, 'attemptCount'),
    maxAttempts: requiredInteger(value.maxAttempts, 'maxAttempts'),
    nextAttemptAt,
    lastHttpStatus: nullableNumber(value.lastHttpStatus, 'lastHttpStatus'),
    lastErrorClass: nullableString(value.lastErrorClass, 'lastErrorClass'),
    failureReason: nullableString(value.failureReason, 'failureReason'),
    createdAt,
    updatedAt,
    completedAt,
  }
}

function parseAttempt(value: unknown, index: number): DeliveryAttempt {
  if (!record(value)) throw invalid(`attempts[${index}]`)
  const startedAt = requiredString(value.startedAt, `attempts[${index}].startedAt`)
  const finishedAt = requiredString(value.finishedAt, `attempts[${index}].finishedAt`)
  if (Number.isNaN(Date.parse(startedAt)) || Number.isNaN(Date.parse(finishedAt))) {
    throw invalid(`attempts[${index}].timestamps`)
  }
  const attemptNumber = requiredInteger(value.attemptNumber, `attempts[${index}].attemptNumber`)
  if (attemptNumber < 1) throw invalid(`attempts[${index}].attemptNumber`)
  return {
    attemptNumber,
    startedAt,
    finishedAt,
    durationMs: requiredInteger(value.durationMs, `attempts[${index}].durationMs`),
    outcome: requiredString(value.outcome, `attempts[${index}].outcome`),
    httpStatus: nullableNumber(value.httpStatus, `attempts[${index}].httpStatus`),
    errorClass: nullableString(value.errorClass, `attempts[${index}].errorClass`),
    terminalReason: nullableString(value.terminalReason, `attempts[${index}].terminalReason`),
  }
}

export function parseDeliveryDetail(value: unknown): DeliveryDetail {
  if (!record(value) || !Array.isArray(value.attempts)) throw invalid('response')
  const delivery = parseDelivery(value.delivery)
  const attempts = value.attempts.map(parseAttempt)
  if (attempts.length > delivery.attemptCount) throw invalid('attempts exceed attemptCount')
  if (
    attempts.some(
      (attempt, index) => index > 0 && attempts[index - 1]!.attemptNumber >= attempt.attemptNumber,
    )
  ) {
    throw invalid('attempt order')
  }
  return { delivery, attempts }
}

/** The code of the error for an HTTP 200 whose body cannot be trusted: the redrive may have been applied. */
export const UNREADABLE_SUCCESS = 'unreadable_success'

function unreadable(field: string): ApiError {
  return new ApiError(
    200,
    UNREADABLE_SUCCESS,
    `The server accepted the request but sent a response that could not be read (${field}).`,
  )
}

function hasExactKeys(value: Record<string, unknown>, keys: string[]): boolean {
  const actual = Object.keys(value).sort()
  return actual.length === keys.length && [...keys].sort().every((key, i) => key === actual[i])
}

/** Each redrive raises the retry budget by this much, up to the ceiling (internal/delivery/redrive.go, ADR 34). */
export const REDRIVE_BUDGET_STEP = 5
export const MAX_ATTEMPTS_CEILING = 50

function parseRedriveResult(
  value: unknown,
  orgId: string,
  deliveryId: string,
  expectedPreviousMaxAttempts: number,
): RedriveResult {
  if (!record(value) || !hasExactKeys(value, ['delivery', 'redrive'])) throw unreadable('response')
  const redrive = value.redrive
  if (
    !record(redrive) ||
    !hasExactKeys(redrive, ['maxAttempts', 'previousFailureReason', 'previousMaxAttempts'])
  ) {
    throw unreadable('redrive')
  }
  const { maxAttempts, previousMaxAttempts, previousFailureReason } = redrive
  if (
    typeof maxAttempts !== 'number' ||
    typeof previousMaxAttempts !== 'number' ||
    !Number.isInteger(maxAttempts) ||
    !Number.isInteger(previousMaxAttempts) ||
    previousMaxAttempts < 1 ||
    previousMaxAttempts > MAX_ATTEMPTS_CEILING ||
    maxAttempts < 1 ||
    maxAttempts > MAX_ATTEMPTS_CEILING ||
    maxAttempts !== Math.min(previousMaxAttempts + REDRIVE_BUDGET_STEP, MAX_ATTEMPTS_CEILING)
  ) {
    throw unreadable('redrive budget')
  }
  // The response must describe this operation: its previous budget is the one the dialog snapshot showed.
  if (previousMaxAttempts !== expectedPreviousMaxAttempts) throw unreadable('previous budget')
  if (previousFailureReason !== null && typeof previousFailureReason !== 'string') {
    throw unreadable('redrive.previousFailureReason')
  }
  const delivery = parseDelivery(value.delivery)
  if (delivery.organizationId !== orgId || delivery.id !== deliveryId)
    throw unreadable('delivery scope')
  // The backend returns the delivery it just moved to PENDING, with the budget it reports.
  if (delivery.status !== 'PENDING' || delivery.maxAttempts !== maxAttempts) {
    throw unreadable('delivery state')
  }
  return { delivery, previousMaxAttempts, maxAttempts, previousFailureReason }
}

/**
 * Redrives one FAILED delivery. Sends exactly one POST (the client repeats it only after a csrf_invalid answer, which
 * the backend gives before the handler runs). An HTTP 200 whose body is unreadable or inconsistent is reported as an
 * ApiError with code `unreadable_success`: the server may have applied the redrive, so callers must treat it as an
 * ambiguous outcome and must not repeat the request. The budget in a success response must follow the contract:
 * `maxAttempts = min(previousMaxAttempts + 5, 50)`, both within 1..50, and `previousMaxAttempts` equal to the snapshot's.
 */
export async function redriveDelivery(
  orgId: string,
  deliveryId: string,
  body: RedriveRequest,
  /** `maxAttempts` of the delivery snapshot the request was made from. The success response must report it as previous. */
  expectedPreviousMaxAttempts: number,
  signal?: AbortSignal,
): Promise<RedriveResult> {
  const path = `/organizations/${encodeURIComponent(orgId)}/deliveries/${encodeURIComponent(deliveryId)}/redrive`
  let raw: unknown
  try {
    raw = await request<unknown>(path, {
      method: 'POST',
      body: { reason: body.reason, expectedAttemptCount: body.expectedAttemptCount },
      signal,
    })
  } catch (error) {
    // res.json() of an HTTP 200 whose body is not JSON.
    if (error instanceof SyntaxError) throw unreadable('body')
    throw error
  }
  try {
    return parseRedriveResult(raw, orgId, deliveryId, expectedPreviousMaxAttempts)
  } catch (error) {
    if (isApiError(error) && error.code === 'invalid_response') throw unreadable('delivery')
    throw error
  }
}

export async function getDeliveryDetail(
  orgId: string,
  deliveryId: string,
  signal?: AbortSignal,
): Promise<DeliveryDetail> {
  const path = `/organizations/${encodeURIComponent(orgId)}/deliveries/${encodeURIComponent(deliveryId)}`
  const detail = parseDeliveryDetail(await request<unknown>(path, { signal }))
  if (detail.delivery.organizationId !== orgId || detail.delivery.id !== deliveryId) {
    throw invalid('delivery scope')
  }
  return detail
}
