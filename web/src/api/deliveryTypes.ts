/** Operational delivery detail returned by GET /organizations/{organizationID}/deliveries/{deliveryID}. */
export interface DeliveryRecord {
  id: string
  organizationId: string
  eventId: string
  destinationId: string
  destinationName: string
  status: string
  attemptCount: number
  maxAttempts: number
  nextAttemptAt: string | null
  lastHttpStatus: number | null
  lastErrorClass: string | null
  failureReason: string | null
  createdAt: string
  updatedAt: string
  completedAt: string | null
}

export interface DeliveryAttempt {
  attemptNumber: number
  startedAt: string
  finishedAt: string
  durationMs: number
  outcome: string
  httpStatus: number | null
  errorClass: string | null
  terminalReason: string | null
}

export interface DeliveryDetail {
  delivery: DeliveryRecord
  attempts: DeliveryAttempt[]
}
