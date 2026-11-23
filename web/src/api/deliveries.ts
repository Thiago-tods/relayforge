import { request } from './client'
import { ApiError } from './errors'
import type { DeliveryAttempt, DeliveryDetail, DeliveryRecord } from './deliveryTypes'

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
