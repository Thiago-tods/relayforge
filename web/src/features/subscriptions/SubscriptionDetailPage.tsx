import { useEffect, useRef, useState } from 'react'
import { Link, useParams } from 'react-router-dom'
import { getDestination } from '../../api/destinations'
import { isApiError, userMessage } from '../../api/errors'
import { getIngestionEndpoint } from '../../api/ingestionEndpoints'
import { getSubscription, listSubscriptions } from '../../api/subscriptions'
import type { Subscription } from '../../api/subscriptionTypes'
import { useResource } from '../../lib/useResource'
import { useOrg } from '../../org/orgContext'
import { Button } from '../../ui/Button'
import { CopyText } from '../../ui/CopyText'
import { DateTime } from '../../ui/DateTime'
import { ErrorState, LoadingState } from '../../ui/states'
import { NoticeBanner } from './NoticeBanner'
import type { PageNotice } from './NoticeBanner'
import {
  DestinationStatusBadge,
  EndpointStatusBadge,
  SubscriptionStatusBadge,
} from './StatusBadges'
import { SubscriptionActions } from './SubscriptionActions'
import { shortId } from './useEndpointLookups'

/** The other intervals of the pair are read in one page; a pair rarely has more than a handful. */
const HISTORY_LIMIT = 100

function loadError(error: unknown): { title: string; message: string } {
  if (isApiError(error) && error.status === 404) {
    return {
      title: 'Subscription not found',
      message:
        'This subscription does not exist in this organization, or you do not have access to it.',
    }
  }
  if (isApiError(error) && error.status === 403) {
    return {
      title: 'Not permitted',
      message: 'You do not have permission to view subscriptions in this organization.',
    }
  }
  return { title: 'Could not load subscription', message: userMessage(error) }
}

/**
 * The route element. The page body is keyed by organization and subscription, so everything it holds (notices, a
 * 403 "denied" flag, the focus counter, the answer of a revocation, open dialogs) belongs to one subscription and
 * can never carry over to another one when the user follows a link between the intervals of a pair.
 */
export function SubscriptionDetailPage() {
  const { organization } = useOrg()
  const { subscriptionId = '' } = useParams()
  return (
    <SubscriptionDetailBody
      key={`${organization.id}:${subscriptionId}`}
      subscriptionId={subscriptionId}
    />
  )
}

function SubscriptionDetailBody({ subscriptionId }: { subscriptionId: string }) {
  const { organization } = useOrg()
  const orgId = organization.id
  const resource = useResource(`${orgId}:${subscriptionId}`, (signal) =>
    getSubscription(orgId, subscriptionId, signal),
  )
  const loaded = resource.data

  // The answer of a successful revocation is shown at once. It is replaced as soon as the resource settles a newer
  // load, so it can never outlive fresher data.
  const [fresh, setFresh] = useState<{ base: Subscription | null; value: Subscription } | null>(
    null,
  )
  const loadedRef = useRef(loaded)
  useEffect(() => {
    loadedRef.current = loaded
  })
  const subscription =
    fresh !== null &&
    fresh.base === loaded &&
    fresh.value.id === subscriptionId &&
    fresh.value.organizationId === orgId
      ? fresh.value
      : loaded

  // The endpoint and the destination are read separately: the subscription answer has no endpoint name, and the
  // destination answer carries more than the subscription's snapshot. Either read may fail without breaking the page.
  const endpointId = loaded?.endpointId ?? ''
  const destinationId = loaded?.destinationId ?? ''
  const endpoint = useResource(`${orgId}:endpoint:${endpointId}`, (signal) =>
    endpointId === '' ? Promise.resolve(null) : getIngestionEndpoint(orgId, endpointId, signal),
  )
  const destination = useResource(`${orgId}:destination:${destinationId}`, (signal) =>
    destinationId === '' ? Promise.resolve(null) : getDestination(orgId, destinationId, signal),
  )
  const history = useResource(`${orgId}:history:${endpointId}:${destinationId}`, (signal) =>
    endpointId === '' || destinationId === ''
      ? Promise.resolve(null)
      : listSubscriptions(
          orgId,
          { limit: HISTORY_LIMIT, offset: 0, endpointId, destinationId },
          signal,
        ),
  )

  const [notice, setNotice] = useState<PageNotice | null>(null)
  const [denied, setDenied] = useState(false)
  const heading = useRef<HTMLHeadingElement>(null)
  const [settled, setSettled] = useState(0)
  useEffect(() => {
    if (settled > 0) heading.current?.focus()
  }, [settled])

  const reloadAll = () => {
    resource.reload()
    history.reload()
  }

  const back = `/o/${orgId}/subscriptions`

  if (resource.error !== null && subscription === null) {
    return (
      <div className="page subscription-detail-page">
        <p className="breadcrumb">
          <Link to={back}>← Back to Subscriptions</Link>
        </p>
        <ErrorState {...loadError(resource.error)} onRetry={resource.reload} />
      </div>
    )
  }
  if (subscription === null) {
    return (
      <div className="page subscription-detail-page">
        <p className="breadcrumb">
          <Link to={back}>← Back to Subscriptions</Link>
        </p>
        <LoadingState label="Loading subscription" />
      </div>
    )
  }

  const endpointData = endpoint.data
  const destinationData = destination.data
  // The destination status the subscription answer carried is a snapshot; the destination read is fresher.
  const destinationStatus = destinationData?.status ?? subscription.destinationStatus
  const destinationName = destinationData?.name ?? subscription.destinationName
  const historyData = history.data

  return (
    <div className="page subscription-detail-page">
      <p className="breadcrumb">
        <Link to={back}>← Back to Subscriptions</Link>
      </p>
      <header className="page-header">
        <p className="eyebrow">Subscription</p>
        <div className="detail-title-row">
          <h1 tabIndex={-1} ref={heading}>
            {endpointData ? endpointData.name : `Endpoint ${shortId(subscription.endpointId)}`}
            {' → '}
            {destinationName}
          </h1>
          <SubscriptionStatusBadge status={subscription.status} />
        </div>
      </header>
      {notice !== null ? <NoticeBanner notice={notice} /> : null}
      {resource.error !== null ? (
        <p className="notice" role="status">
          The refresh failed. The last loaded details are still shown. {userMessage(resource.error)}
        </p>
      ) : null}

      <section className="detail-panel" aria-labelledby="subscription-overview-heading">
        <div className="section-heading-row">
          <h2 id="subscription-overview-heading">Overview</h2>
          <Button variant="secondary" busy={resource.loading} onClick={reloadAll}>
            Refresh
          </Button>
        </div>
        <dl className="detail-grid">
          <dt>Subscription status</dt>
          <dd>
            <SubscriptionStatusBadge status={subscription.status} />
          </dd>
          <dt>Subscription ID</dt>
          <dd>
            <CopyText value={subscription.id} what="Subscription ID" />
          </dd>
          <dt>Started</dt>
          <dd>
            <DateTime value={subscription.createdAt} />
          </dd>
          <dt>Ended</dt>
          <dd>
            {subscription.revokedAt !== null ? (
              <DateTime value={subscription.revokedAt} />
            ) : (
              <span className="muted">Not ended</span>
            )}
          </dd>
        </dl>
      </section>

      <section className="detail-panel" aria-labelledby="subscription-endpoint-heading">
        <h2 id="subscription-endpoint-heading">Ingestion endpoint</h2>
        {endpoint.error !== null && endpointData === null ? (
          <p className="notice" role="status">
            Endpoint details are unavailable. {userMessage(endpoint.error)}{' '}
            <Button variant="secondary" onClick={endpoint.reload}>
              Try again
            </Button>
          </p>
        ) : null}
        <dl className="detail-grid">
          <dt>Name</dt>
          <dd>
            {endpointData ? (
              endpointData.name
            ) : endpoint.loading ? (
              <span className="muted">Loading…</span>
            ) : (
              <span className="muted">Unavailable</span>
            )}
          </dd>
          <dt>Status</dt>
          <dd>
            {endpointData ? (
              <EndpointStatusBadge status={endpointData.status} />
            ) : (
              <span className="muted">Unknown</span>
            )}
          </dd>
          <dt>Endpoint ID</dt>
          <dd>
            <CopyText value={subscription.endpointId} what="Endpoint ID" />
          </dd>
        </dl>
      </section>

      <section className="detail-panel" aria-labelledby="subscription-destination-heading">
        <h2 id="subscription-destination-heading">Destination</h2>
        {destination.error !== null && destinationData === null ? (
          <p className="notice" role="status">
            Destination details are unavailable, so the name and status shown are the ones that came
            with the subscription. {userMessage(destination.error)}{' '}
            <Button variant="secondary" onClick={destination.reload}>
              Try again
            </Button>
          </p>
        ) : null}
        <dl className="detail-grid">
          <dt>Name</dt>
          <dd>
            <Link to={`/o/${orgId}/destinations/${encodeURIComponent(subscription.destinationId)}`}>
              {destinationName}
            </Link>
          </dd>
          <dt>Destination status</dt>
          <dd>
            <DestinationStatusBadge status={destinationStatus} />
          </dd>
          <dt>Destination ID</dt>
          <dd>
            <CopyText value={subscription.destinationId} what="Destination ID" />
          </dd>
        </dl>
        <p className="muted" data-testid="destination-status-note">
          {destinationStatus === 'REVOKED'
            ? 'The destination is revoked, so it is never sent to. This subscription stays active until you revoke it: revoking a destination does not revoke its subscriptions.'
            : destinationStatus === 'PAUSED'
              ? 'The destination is paused. Its deliveries stay pending until the destination is activated. This does not change the subscription.'
              : 'The destination is active. The subscription and the destination have separate statuses.'}
        </p>
      </section>

      <section className="detail-panel" aria-labelledby="subscription-history-heading">
        <div className="section-heading-row">
          <h2 id="subscription-history-heading">Intervals for this endpoint and destination</h2>
        </div>
        <p className="muted">
          Each subscription is one interval, from when it started until it was revoked. Subscribing
          again after a revocation starts a new one. Events accepted while an interval was active
          stay eligible for delivery.
        </p>
        {history.error !== null && historyData === null ? (
          <p className="notice" role="status">
            The other intervals could not be loaded. {userMessage(history.error)}{' '}
            <Button variant="secondary" onClick={history.reload}>
              Try again
            </Button>
          </p>
        ) : historyData === null ? (
          <LoadingState label="Loading intervals" />
        ) : (
          <>
            <div
              className="table-scroll"
              tabIndex={0}
              role="region"
              aria-label="Subscription intervals table"
            >
              <table className="data-table">
                <caption className="visually-hidden">
                  Subscriptions for this endpoint and destination, newest first.
                </caption>
                <thead>
                  <tr>
                    <th scope="col">Subscription</th>
                    <th scope="col">Subscription status</th>
                    <th scope="col">Started</th>
                    <th scope="col">Ended</th>
                  </tr>
                </thead>
                <tbody>
                  {historyData.items.map((item) => (
                    <tr
                      key={item.id}
                      aria-current={item.id === subscription.id ? 'true' : undefined}
                    >
                      <th scope="row">
                        {item.id === subscription.id ? (
                          <span>This subscription</span>
                        ) : (
                          <Link
                            to={`/o/${orgId}/subscriptions/${encodeURIComponent(item.id)}`}
                            aria-label={`Open the subscription that started ${item.createdAt}`}
                          >
                            Open
                          </Link>
                        )}
                      </th>
                      <td>
                        <SubscriptionStatusBadge status={item.status} />
                      </td>
                      <td>
                        <DateTime value={item.createdAt} />
                      </td>
                      <td>
                        {item.revokedAt !== null ? (
                          <DateTime value={item.revokedAt} />
                        ) : (
                          <span className="muted">Not ended</span>
                        )}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
            {historyData.total > historyData.items.length ? (
              <p className="muted">
                Showing the newest {historyData.items.length} of {historyData.total} intervals.
              </p>
            ) : null}
          </>
        )}
      </section>

      <section className="detail-panel" aria-labelledby="subscription-actions-heading">
        <h2 id="subscription-actions-heading">Actions</h2>
        <SubscriptionActions
          key={subscription.id}
          subscription={subscription}
          destinationName={destinationName}
          denied={denied}
          onDenied={() => setDenied(true)}
          onChanged={(value) => {
            setFresh({ base: loadedRef.current, value })
            history.reload()
          }}
          onNotice={setNotice}
          onReload={reloadAll}
          onSettled={() => setSettled((count) => count + 1)}
        />
      </section>
    </div>
  )
}
