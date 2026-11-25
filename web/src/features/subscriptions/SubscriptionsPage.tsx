import { useEffect, useRef, useState } from 'react'
import { Link, useSearchParams } from 'react-router-dom'
import { listDestinationRecords } from '../../api/destinations'
import { isAbortError, userMessage } from '../../api/errors'
import { listIngestionEndpoints } from '../../api/ingestionEndpoints'
import { createSubscription, listSubscriptions } from '../../api/subscriptions'
import { SUBSCRIPTION_STATUSES } from '../../api/subscriptionTypes'
import type { Subscription, SubscriptionStatus } from '../../api/subscriptionTypes'
import { useResource } from '../../lib/useResource'
import { useOrg } from '../../org/orgContext'
import { Button } from '../../ui/Button'
import { DateTime } from '../../ui/DateTime'
import { Pager } from '../../ui/Pager'
import { Select } from '../../ui/Select'
import { ErrorState, LoadingState } from '../../ui/states'
import { useToast } from '../../ui/toastContext'
import { NoticeBanner } from './NoticeBanner'
import type { PageNotice } from './NoticeBanner'
import { PagedSelect } from './PagedSelect'
import type { PagedPage } from './PagedSelect'
import {
  DestinationStatusBadge,
  EndpointStatusBadge,
  SubscriptionStatusBadge,
} from './StatusBadges'
import { SubscriptionForm } from './SubscriptionForm'
import type { SubscriptionFormValues } from './SubscriptionForm'
import { shortId, useEndpointLookups } from './useEndpointLookups'
import { useSubscriptionMutation } from './useSubscriptionMutation'

const PAGE_SIZE = 25
const FILTER_PAGE_SIZE = 100
const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i

const STATUS_FILTER_LABELS: Record<SubscriptionStatus | '', string> = {
  '': 'All',
  ACTIVE: 'Active',
  REVOKED: 'Revoked',
}
const RESOURCE_STATUS_LABELS = { ACTIVE: 'Active', PAUSED: 'Paused', REVOKED: 'Revoked' } as const

function statusFrom(value: string | null): SubscriptionStatus | undefined {
  return (SUBSCRIPTION_STATUSES as readonly string[]).includes(value ?? '')
    ? (value as SubscriptionStatus)
    : undefined
}

/** A filter id from the URL. Anything that is not a UUID is ignored, so the server never answers 400 for it. */
function idFrom(value: string | null): string | undefined {
  return value !== null && UUID.test(value) ? value : undefined
}

function offsetFrom(value: string | null): number {
  const n = Number(value)
  return Number.isInteger(n) && n > 0 && n % PAGE_SIZE === 0 ? n : 0
}

export function SubscriptionsPage() {
  const { organization, can } = useOrg()
  const orgId = organization.id
  const toast = useToast()
  const [params, setParams] = useSearchParams()
  const status = statusFrom(params.get('status'))
  const endpointId = idFrom(params.get('endpointId'))
  const destinationId = idFrom(params.get('destinationId'))
  const offset = offsetFrom(params.get('offset'))

  const resource = useResource(
    `${orgId}:${status ?? 'all'}:${endpointId ?? ''}:${destinationId ?? ''}:${offset}`,
    (signal) =>
      listSubscriptions(
        orgId,
        { limit: PAGE_SIZE, offset, status, endpointId, destinationId },
        signal,
      ),
  )
  const page = resource.data
  const lookup = useEndpointLookups(
    orgId,
    page?.items.map((subscription) => subscription.endpointId) ?? [],
    page,
  )

  const [creating, setCreating] = useState(false)
  const [existing, setExisting] = useState<Subscription | null>(null)
  const [checking, setChecking] = useState(false)
  const [notice, setNotice] = useState<PageNotice | null>(null)
  const [denied, setDenied] = useState(false)
  const mutation = useSubscriptionMutation(orgId)
  const canCreate = can('subscriptions:write') && !denied

  const heading = useRef<HTMLHeadingElement>(null)
  const [settled, setSettled] = useState(0)
  useEffect(() => {
    if (settled > 0) heading.current?.focus()
  }, [settled])

  // The pair check and the read after an unknown result are reads that belong to this page only.
  const checkInFlight = useRef(false)
  const reads = useRef<AbortController | null>(null)
  useEffect(() => () => reads.current?.abort(), [])

  const update = (next: {
    status?: string
    endpointId?: string
    destinationId?: string
    offset?: number
  }) => {
    const query = new URLSearchParams(params)
    for (const key of ['status', 'endpointId', 'destinationId'] as const) {
      if (key in next) {
        const value = next[key]
        if (value) query.set(key, value)
        else query.delete(key)
        query.delete('offset')
      }
    }
    if (next.offset !== undefined) {
      if (next.offset > 0) query.set('offset', String(next.offset))
      else query.delete('offset')
    }
    setParams(query)
  }

  /** Reads whether an ACTIVE subscription exists for the pair. Used before sending and after an unknown result. */
  const readPair = async (values: SubscriptionFormValues, signal: AbortSignal) => {
    const result = await listSubscriptions(
      orgId,
      { limit: 1, offset: 0, status: 'ACTIVE', ...values },
      signal,
    )
    return result.items[0] ?? null
  }

  const afterUnknown = async (values: SubscriptionFormValues) => {
    const controller = new AbortController()
    reads.current = controller
    let found: Subscription | null = null
    let readFailed = false
    try {
      found = await readPair(values, controller.signal)
    } catch (caught) {
      if (controller.signal.aborted || isAbortError(caught)) return
      readFailed = true
    }
    if (controller.signal.aborted) return
    if (found !== null) {
      setNotice({
        tone: 'info',
        title: 'The subscription exists',
        message:
          'The server did not answer in a way this page could read, but an active subscription for this endpoint and destination exists now. Nothing was submitted again.',
        link: { to: `/o/${orgId}/subscriptions/${encodeURIComponent(found.id)}`, label: 'Open it' },
      })
    } else {
      setNotice({
        tone: 'info',
        title: 'The result is unknown',
        message: readFailed
          ? 'The subscription may or may not have been created. It was not submitted again, and the follow-up check failed too. Refresh the list before you create it again.'
          : 'The subscription may or may not have been created, and no active subscription for this pair was found yet. It was not submitted again. Check the list before you create it again.',
      })
    }
    resource.reload()
    setSettled((count) => count + 1)
  }

  const create = async (values: SubscriptionFormValues) => {
    if (checkInFlight.current || mutation.submitting) return
    checkInFlight.current = true
    setChecking(true)
    setExisting(null)
    mutation.clearError()
    const controller = new AbortController()
    reads.current = controller
    let found: Subscription | null = null
    try {
      found = await readPair(values, controller.signal)
    } catch {
      // The check is advisory: when it fails the server still decides (409 already_subscribed).
    }
    checkInFlight.current = false
    if (controller.signal.aborted) return
    setChecking(false)
    if (found !== null) {
      setExisting(found)
      return
    }
    void mutation.run((signal) => createSubscription(orgId, values, signal), {
      onSuccess: () => {
        setCreating(false)
        setNotice(null)
        toast.push('success', 'Subscription created.')
        resource.reload()
        setSettled((count) => count + 1)
      },
      onReaction: (reaction) => {
        setCreating(false)
        if (reaction.kind === 'forbidden') {
          setDenied(true)
          setNotice({
            tone: 'error',
            title: 'Not permitted',
            message: 'You no longer have permission to create subscriptions. Nothing was created.',
          })
          setSettled((count) => count + 1)
        } else if (reaction.kind === 'gone') {
          setNotice({
            tone: 'error',
            title: 'Subscription not created',
            message:
              'The endpoint or the destination could not be found in this organization, or you no longer have access to it. Nothing was created.',
          })
          setSettled((count) => count + 1)
        } else {
          // Unknown (or an alreadyRevoked answer, which cannot belong to a create): read the pair once.
          void afterUnknown(values)
        }
      },
    })
  }

  const loadEndpointFilter = async (
    offsetValue: number,
    signal: AbortSignal,
  ): Promise<PagedPage> => {
    const result = await listIngestionEndpoints(
      orgId,
      { limit: FILTER_PAGE_SIZE, offset: offsetValue },
      signal,
    )
    return {
      fetched: result.items.length,
      total: result.total,
      options: result.items.map((endpoint) => ({
        id: endpoint.id,
        label: `${endpoint.name} (${RESOURCE_STATUS_LABELS[endpoint.status]})`,
      })),
    }
  }
  const loadDestinationFilter = async (
    offsetValue: number,
    signal: AbortSignal,
  ): Promise<PagedPage> => {
    const result = await listDestinationRecords(
      orgId,
      { limit: FILTER_PAGE_SIZE, offset: offsetValue },
      signal,
    )
    return {
      fetched: result.items.length,
      total: result.total,
      options: result.items.map((destination) => ({
        id: destination.id,
        label: `${destination.name} (${RESOURCE_STATUS_LABELS[destination.status]})`,
      })),
    }
  }

  const hasPrevious = offset > 0
  const hasNext = page !== null && page.offset + page.items.length < page.total
  const filtered = status !== undefined || endpointId !== undefined || destinationId !== undefined

  return (
    <div className="page subscriptions-page">
      <header className="page-header page-header-row">
        <div>
          <h1 tabIndex={-1} ref={heading}>
            Subscriptions
          </h1>
          <p>
            A subscription sends the events of one ingestion endpoint to one destination. Its status
            only says whether the subscription itself has ended: a destination can be paused or
            revoked while its subscription is still active.
          </p>
        </div>
        {canCreate ? (
          <Button
            variant="primary"
            onClick={() => {
              mutation.clearError()
              setExisting(null)
              setNotice(null)
              setCreating(true)
            }}
          >
            Create subscription
          </Button>
        ) : null}
      </header>

      {notice !== null ? <NoticeBanner notice={notice} /> : null}

      <section aria-labelledby="subscriptions-list-heading">
        <div className="section-heading-row">
          <h2 id="subscriptions-list-heading">All subscriptions</h2>
          <Button variant="secondary" busy={resource.loading} onClick={resource.reload}>
            Refresh
          </Button>
        </div>
        <div className="filter-row subscription-filters">
          <Select
            label="Subscription status"
            value={status ?? ''}
            onChange={(event) => update({ status: event.target.value })}
          >
            {(['', ...SUBSCRIPTION_STATUSES] as const).map((value) => (
              <option key={value} value={value}>
                {STATUS_FILTER_LABELS[value]}
              </option>
            ))}
          </Select>
          <PagedSelect
            key={`endpoint-${orgId}`}
            label="Ingestion endpoint"
            noun="endpoints"
            value={endpointId ?? ''}
            onChange={(id) => update({ endpointId: id })}
            load={loadEndpointFilter}
            allLabel="All endpoints"
            unloadedLabel={(id) => {
              const known = lookup(id)
              return known.state === 'loaded'
                ? known.endpoint.name
                : `Endpoint ${shortId(id)} (not loaded)`
            }}
          />
          <PagedSelect
            key={`destination-${orgId}`}
            label="Destination"
            noun="destinations"
            value={destinationId ?? ''}
            onChange={(id) => update({ destinationId: id })}
            load={loadDestinationFilter}
            allLabel="All destinations"
            unloadedLabel={(id) => {
              const row = page?.items.find((item) => item.destinationId === id)
              return row ? row.destinationName : `Destination ${shortId(id)} (not loaded)`
            }}
          />
        </div>

        {resource.error !== null && page === null ? (
          <ErrorState
            title="Could not load subscriptions"
            message={userMessage(resource.error)}
            onRetry={resource.reload}
          />
        ) : page === null ? (
          <LoadingState label="Loading subscriptions" />
        ) : (
          <>
            {resource.error !== null ? (
              <p className="notice" role="status">
                The refresh failed. The last loaded list is still shown.{' '}
                {userMessage(resource.error)}
              </p>
            ) : null}
            {page.items.length === 0 ? (
              <div className="empty" data-testid="subscriptions-empty">
                {!filtered && offset === 0 ? (
                  <>
                    <p>No subscriptions yet.</p>
                    {canCreate ? (
                      <p>Create one to deliver the events of an endpoint to a destination.</p>
                    ) : (
                      <p>Someone who can create subscriptions needs to add the first one.</p>
                    )}
                  </>
                ) : offset > 0 ? (
                  <>
                    <p>There are no subscriptions on this page.</p>
                    <Button onClick={() => update({ offset: 0 })}>Back to the first page</Button>
                  </>
                ) : (
                  <p>No subscriptions match these filters.</p>
                )}
              </div>
            ) : (
              <div
                className="table-scroll"
                tabIndex={0}
                role="region"
                aria-label="Subscriptions table"
              >
                <table className="data-table subscriptions-table">
                  <caption className="visually-hidden">
                    Subscriptions, newest first. {page.total} in total.
                  </caption>
                  <thead>
                    <tr>
                      <th scope="col">Ingestion endpoint</th>
                      <th scope="col">Destination</th>
                      <th scope="col">Destination status</th>
                      <th scope="col">Subscription status</th>
                      <th scope="col">Created</th>
                      <th scope="col">Revoked</th>
                      <th scope="col">
                        <span className="visually-hidden">Actions</span>
                      </th>
                    </tr>
                  </thead>
                  <tbody>
                    {page.items.map((subscription) => {
                      const found = lookup(subscription.endpointId)
                      return (
                        <tr key={subscription.id}>
                          <th scope="row" className="wrap">
                            {found.state === 'loaded' ? (
                              <>
                                {found.endpoint.name}
                                {found.endpoint.status !== 'ACTIVE' ? (
                                  <>
                                    {' '}
                                    <EndpointStatusBadge status={found.endpoint.status} />
                                  </>
                                ) : null}
                              </>
                            ) : found.state === 'loading' ? (
                              <span className="muted">Loading name…</span>
                            ) : (
                              <span title={subscription.endpointId}>
                                Endpoint {shortId(subscription.endpointId)}
                              </span>
                            )}
                          </th>
                          <td className="wrap">
                            <Link
                              to={`/o/${orgId}/destinations/${encodeURIComponent(subscription.destinationId)}`}
                            >
                              {subscription.destinationName}
                            </Link>
                          </td>
                          <td>
                            <DestinationStatusBadge status={subscription.destinationStatus} />
                          </td>
                          <td>
                            <SubscriptionStatusBadge status={subscription.status} />
                          </td>
                          <td>
                            <DateTime value={subscription.createdAt} />
                          </td>
                          <td>
                            {subscription.revokedAt !== null ? (
                              <DateTime value={subscription.revokedAt} />
                            ) : (
                              <span className="muted">—</span>
                            )}
                          </td>
                          <td>
                            <Link
                              to={`/o/${orgId}/subscriptions/${encodeURIComponent(subscription.id)}`}
                              aria-label={`Manage subscription to ${subscription.destinationName}, created ${subscription.createdAt}`}
                            >
                              Manage
                            </Link>
                          </td>
                        </tr>
                      )
                    })}
                  </tbody>
                </table>
              </div>
            )}
            <Pager
              label="Subscriptions"
              pageNumber={offset / PAGE_SIZE + 1}
              hasPrevious={hasPrevious}
              hasNext={hasNext}
              busy={resource.loading}
              onPrevious={() => update({ offset: Math.max(0, offset - PAGE_SIZE) })}
              onNext={() => update({ offset: offset + PAGE_SIZE })}
            />
            <p className="muted">
              {page.total} {page.total === 1 ? 'subscription' : 'subscriptions'}
              {filtered ? ' match the filters' : ''}.
            </p>
          </>
        )}
      </section>

      {creating ? (
        <SubscriptionForm
          orgId={orgId}
          submitting={mutation.submitting || checking}
          error={mutation.error}
          existing={existing}
          onSubmit={(values) => void create(values)}
          onCancel={() => setCreating(false)}
        />
      ) : null}
    </div>
  )
}
