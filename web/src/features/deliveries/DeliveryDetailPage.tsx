import { Link, useParams } from 'react-router-dom'
import { getDeliveryDetail } from '../../api/deliveries'
import { userMessage, isApiError } from '../../api/errors'
import { useResource } from '../../lib/useResource'
import { useOrg } from '../../org/orgContext'
import { Button } from '../../ui/Button'
import { CopyText } from '../../ui/CopyText'
import { DateTime } from '../../ui/DateTime'
import { ErrorState, LoadingState } from '../../ui/states'
import { groupAttemptEpisodes } from './attemptEpisodes'

function optional(value: string | number | null): string | number {
  return value === null || value === '' ? '—' : value
}

function detailError(error: unknown): { title: string; message: string } {
  if (isApiError(error) && error.status === 404) {
    return {
      title: 'Delivery not found',
      message:
        'This delivery does not exist in this organization, or you do not have access to it.',
    }
  }
  if (isApiError(error) && error.status === 401) {
    return {
      title: 'Sign-in required',
      message: 'Your session may have ended. Sign in again to view this delivery.',
    }
  }
  return { title: 'Could not load delivery', message: userMessage(error) }
}

export function DeliveryDetailPage() {
  const { organization } = useOrg()
  const { deliveryId = '' } = useParams()
  const orgId = organization.id
  const resource = useResource(`${orgId}:${deliveryId}`, (signal) =>
    getDeliveryDetail(orgId, deliveryId, signal),
  )
  const detail = resource.data
  const back = `/o/${orgId}/operations/activity`
  const episodes = detail === null ? [] : groupAttemptEpisodes(detail.attempts)

  return (
    <div className="page delivery-detail-page">
      <p className="breadcrumb">
        <Link to={back}>← Back to Activity</Link>
      </p>
      {resource.error !== null && detail === null ? (
        <ErrorState {...detailError(resource.error)} onRetry={resource.reload} />
      ) : detail === null ? (
        <LoadingState label="Loading delivery detail" />
      ) : (
        <>
          <header className="page-header">
            <p className="eyebrow">Delivery detail</p>
            <div className="detail-title-row">
              <h1>{detail.delivery.status}</h1>
              <span className="badge">
                {detail.delivery.attemptCount} / {detail.delivery.maxAttempts} attempts
              </span>
            </div>
            <CopyText value={detail.delivery.id} what="Delivery ID" />
          </header>
          {resource.error !== null ? (
            <p className="notice" role="status">
              The refresh failed. The last loaded detail is still shown.{' '}
              {userMessage(resource.error)}
            </p>
          ) : null}
          <section className="detail-panel" aria-labelledby="delivery-overview-heading">
            <h2 id="delivery-overview-heading">Delivery overview</h2>
            <dl className="detail-grid">
              <dt>Destination</dt>
              <dd>{detail.delivery.destinationName}</dd>
              <dt>Destination ID</dt>
              <dd>
                <CopyText value={detail.delivery.destinationId} what="Destination ID" />
              </dd>
              <dt>Event ID</dt>
              <dd>
                <CopyText value={detail.delivery.eventId} what="Event ID" />
              </dd>
              <dt>Attempt count</dt>
              <dd>
                {detail.delivery.attemptCount} of {detail.delivery.maxAttempts}
              </dd>
              <dt>Failure reason</dt>
              <dd>{optional(detail.delivery.failureReason)}</dd>
              <dt>Last HTTP status</dt>
              <dd>{optional(detail.delivery.lastHttpStatus)}</dd>
              <dt>Last error class</dt>
              <dd>{optional(detail.delivery.lastErrorClass)}</dd>
              <dt>Next attempt</dt>
              <dd>
                {detail.delivery.nextAttemptAt ? (
                  <DateTime value={detail.delivery.nextAttemptAt} />
                ) : (
                  '—'
                )}
              </dd>
              <dt>Created</dt>
              <dd>
                <DateTime value={detail.delivery.createdAt} />
              </dd>
              <dt>Updated</dt>
              <dd>
                <DateTime value={detail.delivery.updatedAt} />
              </dd>
              <dt>Completed</dt>
              <dd>
                {detail.delivery.completedAt ? (
                  <DateTime value={detail.delivery.completedAt} />
                ) : (
                  '—'
                )}
              </dd>
            </dl>
          </section>
          <section className="detail-panel" aria-labelledby="attempt-history-heading">
            <div className="section-heading-row">
              <div>
                <h2 id="attempt-history-heading">Attempt history</h2>
                <p className="muted">
                  Attempts returned by the delivery detail endpoint, newest first. A worker append
                  during the read may appear after refresh.
                </p>
              </div>
              <Button variant="secondary" busy={resource.loading} onClick={resource.reload}>
                Refresh
              </Button>
            </div>
            {detail.attempts.length === 0 ? (
              <p className="empty">No attempts have been recorded for this delivery.</p>
            ) : (
              <div className="episode-list">
                {episodes.map((episode, index) => (
                  <section
                    className="episode"
                    key={`${episode.attempts[0]?.attemptNumber ?? index}-${episode.terminalReason ?? 'open'}`}
                    aria-label={`Failure episode ${episodes.length - index}`}
                  >
                    <h3>
                      {episode.terminalReason === null
                        ? 'Attempts without a recorded episode ending'
                        : `Failure episode ended: ${episode.terminalReason}`}
                    </h3>
                    <ol className="attempt-list">
                      {episode.attempts.map((attempt) => (
                        <li
                          className={
                            attempt.terminalReason !== null
                              ? 'attempt-item attempt-terminal'
                              : 'attempt-item'
                          }
                          key={attempt.attemptNumber}
                        >
                          <div className="attempt-heading">
                            <strong>Attempt #{attempt.attemptNumber}</strong>
                            <span className="badge">{attempt.outcome}</span>
                            {attempt.terminalReason !== null ? (
                              <span className="terminal-label">Episode ending</span>
                            ) : null}
                          </div>
                          <dl className="attempt-grid">
                            <dt>Started</dt>
                            <dd>
                              <DateTime value={attempt.startedAt} />
                            </dd>
                            <dt>Finished</dt>
                            <dd>
                              <DateTime value={attempt.finishedAt} />
                            </dd>
                            <dt>Duration</dt>
                            <dd>{attempt.durationMs.toLocaleString('en-US')} ms</dd>
                            <dt>HTTP status</dt>
                            <dd>{optional(attempt.httpStatus)}</dd>
                            <dt>Error class</dt>
                            <dd>{optional(attempt.errorClass)}</dd>
                            <dt>Terminal reason</dt>
                            <dd>{optional(attempt.terminalReason)}</dd>
                          </dl>
                          {attempt.outcome === 'ABANDONED' ? (
                            <p className="muted">
                              The worker did not finish this attempt. This does not establish that
                              the destination failed.
                            </p>
                          ) : null}
                        </li>
                      ))}
                    </ol>
                  </section>
                ))}
              </div>
            )}
          </section>
          <p>
            <Link to={back}>Back to Activity</Link>
          </p>
        </>
      )}
    </div>
  )
}
