import { useEffect, useRef, useState } from 'react'
import { Link, useSearchParams } from 'react-router-dom'
import { createDestination, listDestinationRecords } from '../../api/destinations'
import { DESTINATION_STATUSES } from '../../api/destinationTypes'
import type { DestinationStatus } from '../../api/destinationTypes'
import { userMessage } from '../../api/errors'
import { useResource } from '../../lib/useResource'
import { useOrg } from '../../org/orgContext'
import { Button } from '../../ui/Button'
import { DateTime } from '../../ui/DateTime'
import { Pager } from '../../ui/Pager'
import { Select } from '../../ui/Select'
import { ErrorState, LoadingState } from '../../ui/states'
import { useToast } from '../../ui/toastContext'
import type { Notice } from './DestinationActions'
import { DestinationForm } from './DestinationForm'
import { DestinationSecretDialog } from './DestinationSecretDialog'
import { StatusBadge } from './StatusBadge'
import { useDestinationMutation } from './useDestinationMutation'

const PAGE_SIZE = 25

const FILTER_LABELS: Record<DestinationStatus | '', string> = {
  '': 'All',
  ACTIVE: 'Active',
  PAUSED: 'Paused',
  REVOKED: 'Revoked',
}

function statusFrom(value: string | null): DestinationStatus | undefined {
  return (DESTINATION_STATUSES as readonly string[]).includes(value ?? '')
    ? (value as DestinationStatus)
    : undefined
}

function offsetFrom(value: string | null): number {
  const n = Number(value)
  return Number.isInteger(n) && n > 0 && n % PAGE_SIZE === 0 ? n : 0
}

export function DestinationsPage() {
  const { organization, can } = useOrg()
  const orgId = organization.id
  const toast = useToast()
  const [params, setParams] = useSearchParams()
  const status = statusFrom(params.get('status'))
  const offset = offsetFrom(params.get('offset'))

  const resource = useResource(`${orgId}:${status ?? 'all'}:${offset}`, (signal) =>
    listDestinationRecords(orgId, { limit: PAGE_SIZE, offset, status }, signal),
  )
  const page = resource.data

  const [creating, setCreating] = useState(false)
  const [secret, setSecret] = useState<{ name: string; value: string } | null>(null)
  const [notice, setNotice] = useState<Notice | null>(null)
  const [denied, setDenied] = useState(false)
  const mutation = useDestinationMutation(orgId)
  const canCreate = can('destinations:create') && !denied

  const heading = useRef<HTMLHeadingElement>(null)
  const [settled, setSettled] = useState(0)
  useEffect(() => {
    if (settled > 0) heading.current?.focus()
  }, [settled])

  const update = (next: { status?: string; offset?: number }) => {
    const query = new URLSearchParams(params)
    if ('status' in next) {
      if (next.status) query.set('status', next.status)
      else query.delete('status')
      query.delete('offset')
    }
    if (next.offset !== undefined) {
      if (next.offset > 0) query.set('offset', String(next.offset))
      else query.delete('offset')
    }
    setParams(query)
  }

  const create = (values: { name: string; url: string }) =>
    void mutation.run((signal) => createDestination(orgId, values, signal), {
      onSuccess: (result) => {
        setCreating(false)
        setNotice(null)
        setSecret({ name: result.destination.name, value: result.signingSecret })
        toast.push('success', 'Destination created.')
        resource.reload()
      },
      onReaction: (reaction) => {
        setCreating(false)
        if (reaction.kind === 'forbidden') {
          setDenied(true)
          setNotice({
            tone: 'error',
            title: 'Not permitted',
            message: 'You no longer have permission to create destinations. Nothing was created.',
          })
        } else if (reaction.kind === 'gone' || reaction.kind === 'revoked') {
          // A definite rejection of the API (404, 409): the destination was not created.
          setNotice({
            tone: 'error',
            title: 'Destination not created',
            message:
              reaction.kind === 'gone'
                ? 'This organization could not be found, or you no longer have access to it. Nothing was created.'
                : 'The server rejected the request as a conflict with a revoked destination. Nothing was created.',
          })
        } else {
          // The result is unknown: the destination may exist, and if it does its secret was not shown.
          setNotice({
            tone: 'info',
            title: 'The result is unknown',
            message:
              'The destination may or may not have been created. It was not submitted again. The list is being refreshed: if a destination with this name appears, its signing secret was not shown, so rotate it from its page. Check the list before you create it again.',
          })
          resource.reload()
        }
        setSettled((count) => count + 1)
      },
    })

  const hasPrevious = offset > 0
  const hasNext = page !== null && page.offset + page.items.length < page.total

  return (
    <div className="page destinations-page">
      <header className="page-header page-header-row">
        <div>
          <h1 tabIndex={-1} ref={heading}>
            Destinations
          </h1>
          <p>
            Destinations are the HTTPS endpoints that RelayForge delivers events to. Each one has
            its own signing secret.
          </p>
        </div>
        {canCreate ? (
          <Button
            variant="primary"
            onClick={() => {
              mutation.clearError()
              setNotice(null)
              setCreating(true)
            }}
          >
            Create destination
          </Button>
        ) : null}
      </header>

      {notice !== null ? (
        <section
          className={
            notice.tone === 'error' ? 'form-error destination-notice' : 'notice destination-notice'
          }
          role={notice.tone === 'error' ? 'alert' : 'status'}
          aria-label={notice.title}
        >
          <strong>{notice.title}.</strong> {notice.message}
        </section>
      ) : null}

      <section aria-labelledby="destinations-list-heading">
        <div className="section-heading-row">
          <h2 id="destinations-list-heading">All destinations</h2>
          <div className="filter-row">
            <Select
              label="Status"
              value={status ?? ''}
              onChange={(event) => update({ status: event.target.value })}
            >
              {(['', ...DESTINATION_STATUSES] as const).map((value) => (
                <option key={value} value={value}>
                  {FILTER_LABELS[value]}
                </option>
              ))}
            </Select>
            <Button variant="secondary" busy={resource.loading} onClick={resource.reload}>
              Refresh
            </Button>
          </div>
        </div>

        {resource.error !== null && page === null ? (
          <ErrorState
            title="Could not load destinations"
            message={userMessage(resource.error)}
            onRetry={resource.reload}
          />
        ) : page === null ? (
          <LoadingState label="Loading destinations" />
        ) : (
          <>
            {resource.error !== null ? (
              <p className="notice" role="status">
                The refresh failed. The last loaded list is still shown.{' '}
                {userMessage(resource.error)}
              </p>
            ) : null}
            {page.items.length === 0 ? (
              <div className="empty" data-testid="destinations-empty">
                {status === undefined && offset === 0 ? (
                  <>
                    <p>No destinations yet.</p>
                    {canCreate ? (
                      <p>Create one to start delivering events to your own HTTPS endpoint.</p>
                    ) : (
                      <p>Someone who can create destinations needs to add the first one.</p>
                    )}
                  </>
                ) : offset > 0 ? (
                  <>
                    <p>There are no destinations on this page.</p>
                    <Button onClick={() => update({ offset: 0 })}>Back to the first page</Button>
                  </>
                ) : (
                  <p>No {FILTER_LABELS[status ?? ''].toLowerCase()} destinations.</p>
                )}
              </div>
            ) : (
              <div
                className="table-scroll"
                tabIndex={0}
                role="region"
                aria-label="Destinations table"
              >
                <table className="data-table destinations-table">
                  <caption className="visually-hidden">
                    Destinations, newest first. {page.total} in total.
                  </caption>
                  <thead>
                    <tr>
                      <th scope="col">Name</th>
                      <th scope="col">URL</th>
                      <th scope="col">Status</th>
                      <th scope="col">Created</th>
                      <th scope="col">Updated</th>
                      <th scope="col">
                        <span className="visually-hidden">Actions</span>
                      </th>
                    </tr>
                  </thead>
                  <tbody>
                    {page.items.map((destination) => (
                      <tr key={destination.id}>
                        <th scope="row" className="wrap">
                          <Link
                            to={`/o/${orgId}/destinations/${encodeURIComponent(destination.id)}`}
                          >
                            {destination.name}
                          </Link>
                        </th>
                        <td className="wrap">
                          <code className="destination-url">{destination.url}</code>
                          {destination.urlHasQuery ? (
                            <span className="muted"> (query hidden)</span>
                          ) : null}
                        </td>
                        <td>
                          <StatusBadge status={destination.status} />
                        </td>
                        <td>
                          <DateTime value={destination.createdAt} />
                        </td>
                        <td>
                          <DateTime value={destination.updatedAt} />
                        </td>
                        <td>
                          <Link
                            to={`/o/${orgId}/destinations/${encodeURIComponent(destination.id)}`}
                            aria-label={`Manage ${destination.name}`}
                          >
                            Manage
                          </Link>
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}
            <Pager
              label="Destinations"
              pageNumber={offset / PAGE_SIZE + 1}
              hasPrevious={hasPrevious}
              hasNext={hasNext}
              busy={resource.loading}
              onPrevious={() => update({ offset: Math.max(0, offset - PAGE_SIZE) })}
              onNext={() => update({ offset: offset + PAGE_SIZE })}
            />
            <p className="muted">
              {page.total} {page.total === 1 ? 'destination' : 'destinations'}
              {status !== undefined ? ` (${FILTER_LABELS[status].toLowerCase()})` : ''}.
            </p>
          </>
        )}
      </section>

      {creating ? (
        <DestinationForm
          mode="create"
          title="Create destination"
          description="Add an HTTPS endpoint that RelayForge delivers events to. The server checks the name and the URL."
          submitLabel="Create destination"
          notes={
            <p className="muted">
              A signing secret is generated for the new destination and shown once, right after
              creation.
            </p>
          }
          submitting={mutation.submitting}
          error={mutation.error}
          onSubmit={create}
          onCancel={() => setCreating(false)}
        />
      ) : null}
      {secret !== null ? (
        <DestinationSecretDialog
          kind="created"
          destinationName={secret.name}
          secret={secret.value}
          onAcknowledge={() => {
            setSecret(null)
            setSettled((count) => count + 1)
          }}
          onDiscard={() => {
            setSecret(null)
            setNotice({
              tone: 'error',
              title: 'The signing secret was discarded',
              message:
                'The window closed unexpectedly before you confirmed that you saved the secret, so it was discarded and cannot be shown again. The destination exists: rotate its signing secret from its page to get a new one.',
            })
            setSettled((count) => count + 1)
          }}
        />
      ) : null}
    </div>
  )
}
