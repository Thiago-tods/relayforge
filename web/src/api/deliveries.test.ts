import { describe, expect, it } from 'vitest'
import { parseDeliveryDetail } from './deliveries'

const delivery = {
  id: 'd1',
  organizationId: 'o1',
  eventId: 'e1',
  destinationId: 'x1',
  destinationName: 'Webhook',
  status: 'FAILED',
  attemptCount: 2,
  maxAttempts: 7,
  lastHttpStatus: 500,
  lastErrorClass: 'server_error',
  failureReason: 'INTERNAL_ERROR',
  createdAt: '2026-11-20T10:00:00Z',
  updatedAt: '2026-11-20T10:02:00Z',
  completedAt: '2026-11-20T10:02:00Z',
}
const attempt = {
  attemptNumber: 1,
  startedAt: '2026-11-20T10:00:00Z',
  finishedAt: '2026-11-20T10:00:01Z',
  durationMs: 1000,
  outcome: 'HTTP_ERROR',
  httpStatus: 500,
  errorClass: 'server_error',
  terminalReason: null,
}

describe('delivery detail response parsing', () => {
  it('parses delivery metadata and preserves optional missing values as null', () => {
    const result = parseDeliveryDetail({ delivery: delivery, attempts: [attempt] })
    expect(result.delivery.id).toBe('d1')
    expect(result.delivery.nextAttemptAt).toBeNull()
    expect(result.attempts[0]?.terminalReason).toBeNull()
  })

  it('accepts unknown future status and outcome values as displayable strings', () => {
    const result = parseDeliveryDetail({
      delivery: { ...delivery, status: 'FUTURE_STATUS' },
      attempts: [{ ...attempt, outcome: 'FUTURE_OUTCOME' }],
    })
    expect(result.delivery.status).toBe('FUTURE_STATUS')
    expect(result.attempts[0]?.outcome).toBe('FUTURE_OUTCOME')
  })

  it('rejects malformed response shapes and attempts beyond the delivery attempt count', () => {
    expect(() => parseDeliveryDetail({ delivery, attempts: {} })).toThrow(
      /invalid delivery detail/i,
    )
    expect(() =>
      parseDeliveryDetail({ delivery: { ...delivery, attemptCount: 0 }, attempts: [attempt] }),
    ).toThrow(/invalid delivery detail/i)
    expect(() =>
      parseDeliveryDetail({ delivery, attempts: [{ ...attempt, startedAt: 'invalid' }] }),
    ).toThrow(/invalid delivery detail/i)
  })

  it('requires attempts in ascending unique attempt-number order from the API', () => {
    expect(() =>
      parseDeliveryDetail({
        delivery: { ...delivery, attemptCount: 2 },
        attempts: [attempt, { ...attempt, attemptNumber: 1 }],
      }),
    ).toThrow(/invalid delivery detail/i)
  })
})
