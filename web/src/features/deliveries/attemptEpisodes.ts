import type { DeliveryAttempt } from '../../api/deliveryTypes'

export interface AttemptEpisode {
  attempts: DeliveryAttempt[]
  terminalReason: string | null
}

/**
 * The backend marks only attempts that ended a FAILED episode. Walking the stored ascending attempt sequence lets
 * us split only at those explicit markers; an unfinished trailing group remains unclassified, never inferred.
 */
export function groupAttemptEpisodes(attempts: DeliveryAttempt[]): AttemptEpisode[] {
  const episodes: AttemptEpisode[] = []
  let current: DeliveryAttempt[] = []
  for (const attempt of attempts) {
    current.push(attempt)
    if (attempt.terminalReason !== null) {
      episodes.push({ attempts: current, terminalReason: attempt.terminalReason })
      current = []
    }
  }
  if (current.length > 0) episodes.push({ attempts: current, terminalReason: null })
  return episodes
    .reverse()
    .map((episode) => ({ ...episode, attempts: [...episode.attempts].reverse() }))
}
