import { useEffect, useRef, useState } from 'react'
import { Link, useParams } from 'react-router-dom'
import { getDestination } from '../../api/destinations'
import type { Destination } from '../../api/destinationTypes'
import { isApiError, userMessage } from '../../api/errors'
import { useResource } from '../../lib/useResource'
import { useOrg } from '../../org/orgContext'
import { Button } from '../../ui/Button'
import { CopyText } from '../../ui/CopyText'
import { DateTime } from '../../ui/DateTime'
import { ErrorState, LoadingState } from '../../ui/states'
import { DestinationActions } from './DestinationActions'
import type { Notice } from './DestinationActions'
import { StatusBadge } from './StatusBadge'

function loadError(error: unknown): { title: string; message: string } {
  if (isApiError(error) && error.status === 404) {
    return {
      title: 'Destination not found',
      message:
        'This destination does not exist in this organization, or you do not have access to it.',
    }
  }
  if (isApiError(error) && error.status === 403) {
    return {
      title: 'Not permitted',
      message: 'You do not have permission to view destinations in this organization.',
    }
  }
  return { title: 'Could not load destination', message: userMessage(error) }
}

export function DestinationDetailPage() {
  const { organization } = useOrg()
  const { destinationId = '' } = useParams()
  const orgId = organization.id
  const resource = useResource(`${orgId}:${destinationId}`, (signal) =>
    getDestination(orgId, destinationId, signal),
  )
  const loaded = resource.data

  // The answer of a successful operation is shown at once. It is replaced as soon as the resource settles a newer
  // load (Refresh, or the single read after an unknown result), so it can never outlive fresher data.
  const [fresh, setFresh] = useState<{ base: Destination | null; value: Destination } | null>(null)
  const loadedRef = useRef(loaded)
  useEffect(() => {
    loadedRef.current = loaded
  })
  const destination =
    fresh !== null &&
    fresh.base === loaded &&
    fresh.value.id === destinationId &&
    fresh.value.organizationId === orgId
      ? fresh.value
      : loaded

  const [notice, setNotice] = useState<Notice | null>(null)
  const [denied, setDenied] = useState(false)
  const heading = useRef<HTMLHeadingElement>(null)
  const [settled, setSettled] = useState(0)
  // After a dialog closed with a result its trigger may be gone, so focus moves to the page heading.
  useEffect(() => {
    if (settled > 0) heading.current?.focus()
  }, [settled])

  const back = `/o/${orgId}/destinations`

  return (
    <div className="page destination-detail-page">
      <p className="breadcrumb">
        <Link to={back}>← Back to Destinations</Link>
      </p>
      {resource.error !== null && destination === null ? (
        <ErrorState {...loadError(resource.error)} onRetry={resource.reload} />
      ) : destination === null ? (
        <LoadingState label="Loading destination" />
      ) : (
        <>
          <header className="page-header">
            <p className="eyebrow">Destination</p>
            <div className="detail-title-row">
              <h1 tabIndex={-1} ref={heading}>
                {destination.name}
              </h1>
              <StatusBadge status={destination.status} />
            </div>
          </header>
          {notice !== null ? (
            <section
              className={
                notice.tone === 'error'
                  ? 'form-error destination-notice'
                  : 'notice destination-notice'
              }
              role={notice.tone === 'error' ? 'alert' : 'status'}
              aria-label={notice.title}
            >
              <strong>{notice.title}.</strong> {notice.message}
            </section>
          ) : null}
          {resource.error !== null ? (
            <p className="notice" role="status">
              The refresh failed. The last loaded details are still shown.{' '}
              {userMessage(resource.error)}
            </p>
          ) : null}
          <section className="detail-panel" aria-labelledby="destination-overview-heading">
            <div className="section-heading-row">
              <h2 id="destination-overview-heading">Overview</h2>
              <Button variant="secondary" busy={resource.loading} onClick={resource.reload}>
                Refresh
              </Button>
            </div>
            <dl className="detail-grid">
              <dt>Name</dt>
              <dd>{destination.name}</dd>
              <dt>Status</dt>
              <dd>
                <StatusBadge status={destination.status} />
              </dd>
              <dt>URL</dt>
              <dd>
                <code className="destination-url">{destination.url}</code>
              </dd>
              <dt>Destination ID</dt>
              <dd>
                <CopyText value={destination.id} what="Destination ID" />
              </dd>
              <dt>Created</dt>
              <dd>
                <DateTime value={destination.createdAt} />
              </dd>
              <dt>Last updated</dt>
              <dd>
                <DateTime value={destination.updatedAt} />
              </dd>
              {destination.revokedAt !== null ? (
                <>
                  <dt>Revoked</dt>
                  <dd>
                    <DateTime value={destination.revokedAt} />
                  </dd>
                </>
              ) : null}
            </dl>
          </section>
          <section className="detail-panel" aria-labelledby="destination-security-heading">
            <h2 id="destination-security-heading">Security</h2>
            <ul className="security-list">
              <li>
                The signing secret is stored encrypted and is never shown again after it is created
                or rotated. Rotate it to get a new one.
              </li>
              {destination.urlHasQuery ? (
                <li className="notice" data-testid="query-hidden-notice">
                  This URL has query parameters. They are hidden for security and cannot be shown or
                  retrieved. To change them, enter the full new URL.
                </li>
              ) : (
                <li>This URL has no query parameters.</li>
              )}
              <li>
                Pending deliveries use the updated URL on their next attempt. Attempts already
                recorded keep their history.
              </li>
            </ul>
          </section>
          <section className="detail-panel" aria-labelledby="destination-actions-heading">
            <h2 id="destination-actions-heading">Actions</h2>
            <DestinationActions
              key={destination.id}
              destination={destination}
              denied={denied}
              onDenied={() => setDenied(true)}
              onChanged={(value) => setFresh({ base: loadedRef.current, value })}
              onNotice={setNotice}
              onReload={resource.reload}
              onSettled={() => setSettled((count) => count + 1)}
            />
          </section>
        </>
      )}
    </div>
  )
}
