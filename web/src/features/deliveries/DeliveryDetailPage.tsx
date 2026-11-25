import { useEffect, useRef, useState } from 'react'
import { Link, useParams } from 'react-router-dom'
import { getDeliveryDetail } from '../../api/deliveries'
import type { DeliveryDetail } from '../../api/deliveryTypes'
import { userMessage, isApiError } from '../../api/errors'
import { useResource } from '../../lib/useResource'
import { useOrg } from '../../org/orgContext'
import { Button } from '../../ui/Button'
import { CopyText } from '../../ui/CopyText'
import { DateTime } from '../../ui/DateTime'
import { ErrorState, LoadingState } from '../../ui/states'
import { groupAttemptEpisodes } from './attemptEpisodes'
import { RedriveBanner } from './RedriveBanner'
import { RedriveDialog } from './RedriveDialog'
import { budgetLooksExhausted, MAX_ATTEMPTS_CEILING, noticeFor } from './redriveOutcome'
import { useRedrive } from './useRedrive'

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
  const { organization, can } = useOrg()
  const { deliveryId = '' } = useParams()
  const orgId = organization.id
  const resource = useResource(`${orgId}:${deliveryId}`, (signal) =>
    getDeliveryDetail(orgId, deliveryId, signal),
  )
  const loaded = resource.data

  // A detail the redrive flow read itself (the follow-up or verification GET) is shown instead of the loaded one,
  // until the resource settles a newer load. That avoids a second GET for the same information.
  const [fresh, setFresh] = useState<{
    base: DeliveryDetail | null
    detail: DeliveryDetail
  } | null>(null)
  const loadedRef = useRef(loaded)
  useEffect(() => {
    loadedRef.current = loaded
  })
  const showFresh = (next: DeliveryDetail) => setFresh({ base: loadedRef.current, detail: next })
  const detail =
    fresh !== null &&
    fresh.base === loaded &&
    fresh.detail.delivery.id === deliveryId &&
    fresh.detail.delivery.organizationId === orgId
      ? fresh.detail
      : loaded

  const mayRedrive = can('deliveries:redrive')
  const redrive = useRedrive({ orgId, deliveryId, allowed: mayRedrive, onFresh: showFresh })
  const flow = redrive.flow
  const notice = noticeFor(flow)
  const bannerHeading = useRef<HTMLHeadingElement>(null)
  // When the dialog closes with a result the trigger may be gone, so focus moves to the result heading.
  useEffect(() => {
    if (flow.phase === 'outcome' || flow.phase === 'checking' || flow.phase === 'unverified') {
      bannerHeading.current?.focus()
    }
  }, [flow.phase, notice?.title])

  const back = `/o/${orgId}/operations/activity`
  const episodes = detail === null ? [] : groupAttemptEpisodes(detail.attempts)
  // The action is only for a FAILED delivery, only with deliveries:redrive, and not while a redrive is unresolved.
  const actionVisible =
    detail !== null &&
    mayRedrive &&
    !redrive.permissionLost &&
    detail.delivery.status === 'FAILED' &&
    flow.phase !== 'checking' &&
    flow.phase !== 'unverified' &&
    // After an accepted redrive the shown detail is out of date until it is refreshed: do not offer a second one.
    !(flow.phase === 'outcome' && flow.outcome.tone === 'success')
  const announcement =
    notice !== null && notice.tone !== 'error' ? `${notice.title}. ${notice.message}` : ''

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
            {actionVisible ? (
              <div className="redrive-action">
                {budgetLooksExhausted(detail.delivery.maxAttempts) ? (
                  <p className="muted">
                    This delivery already has the maximum retry budget ({MAX_ATTEMPTS_CEILING}{' '}
                    attempts), so it cannot be redriven again.
                  </p>
                ) : (
                  <Button variant="secondary" onClick={() => redrive.open(detail.delivery)}>
                    Redrive…
                  </Button>
                )}
              </div>
            ) : null}
          </header>
          {mayRedrive ? (
            <p className="visually-hidden" role="status" data-testid="redrive-live">
              {announcement}
            </p>
          ) : null}
          <RedriveBanner flow={flow} headingRef={bannerHeading} onCheckAgain={redrive.checkAgain} />
          {flow.phase === 'confirming' || flow.phase === 'submitting' ? (
            <RedriveDialog
              snapshot={flow.snapshot}
              submitting={flow.phase === 'submitting'}
              error={redrive.dialogError}
              onSubmit={redrive.submit}
              onCancel={redrive.cancel}
            />
          ) : null}
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
