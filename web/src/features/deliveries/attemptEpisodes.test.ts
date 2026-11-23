import { describe, expect, it } from 'vitest'
import type { DeliveryAttempt } from '../../api/deliveryTypes'
import { groupAttemptEpisodes } from './attemptEpisodes'

const attempt = (attemptNumber: number, terminalReason: string | null = null): DeliveryAttempt => ({
  attemptNumber,
  terminalReason,
  startedAt: `2026-11-20T10:0${attemptNumber}:00Z`,
  finishedAt: `2026-11-20T10:0${attemptNumber}:01Z`,
  durationMs: 1000,
  outcome: attemptNumber === 2 ? 'ABANDONED' : 'HTTP_ERROR',
  httpStatus: null,
  errorClass: null,
})

describe('groupAttemptEpisodes', () => {
  it('splits only at recorded terminal markers and returns newest episode and attempt first', () => {
    const groups = groupAttemptEpisodes([
      attempt(1),
      attempt(2, 'RETRIES_EXHAUSTED'),
      attempt(3),
      attempt(4, 'INTERNAL_ERROR'),
    ])
    expect(groups.map((g) => g.attempts.map((a) => a.attemptNumber))).toEqual([
      [4, 3],
      [2, 1],
    ])
    expect(groups.map((g) => g.terminalReason)).toEqual(['INTERNAL_ERROR', 'RETRIES_EXHAUSTED'])
  })

  it('keeps an unmarked trailing group distinct without inventing an episode ending', () => {
    const groups = groupAttemptEpisodes([attempt(1, 'RETRIES_EXHAUSTED'), attempt(2), attempt(3)])
    expect(groups.map((g) => g.attempts.map((a) => a.attemptNumber))).toEqual([[3, 2], [1]])
    expect(groups[0]?.terminalReason).toBeNull()
  })

  it('returns no episodes for an empty history', () => {
    expect(groupAttemptEpisodes([])).toEqual([])
  })
})
