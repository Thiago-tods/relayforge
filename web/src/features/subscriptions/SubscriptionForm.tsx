import { useEffect, useId, useRef, useState } from 'react'
import type { FormEvent } from 'react'
import { Link } from 'react-router-dom'
import { listDestinationRecords } from '../../api/destinations'
import { listIngestionEndpoints } from '../../api/ingestionEndpoints'
import type { Subscription } from '../../api/subscriptionTypes'
import { Button } from '../../ui/Button'
import { Dialog } from '../../ui/Dialog'
import { PagedSelect } from './PagedSelect'
import type { PagedPage } from './PagedSelect'
import type { SubscriptionMutationError } from './useSubscriptionMutation'

const PICKER_PAGE_SIZE = 100

interface ClientErrors {
  endpoint?: string | undefined
  destination?: string | undefined
}

const STATUS_LABELS = { ACTIVE: 'Active', PAUSED: 'Paused', REVOKED: 'Revoked' } as const

export interface SubscriptionFormValues {
  endpointId: string
  destinationId: string
}

interface Props {
  orgId: string
  /** True while the pair is being checked or the request runs. */
  submitting: boolean
  error: SubscriptionMutationError | null
  /** An ACTIVE subscription for the chosen pair that was found before sending anything. */
  existing: Subscription | null
  onSubmit: (values: SubscriptionFormValues) => void
  onCancel: () => void
}

/**
 * The dialog for creating a subscription: one ingestion endpoint and one destination of this organization. REVOKED
 * endpoints and destinations are not offered (the server rejects them); PAUSED ones are, labelled as paused. The
 * server stays authoritative for every rule, including "one ACTIVE subscription per pair".
 */
export function SubscriptionForm({
  orgId,
  submitting,
  error,
  existing,
  onSubmit,
  onCancel,
}: Props) {
  const titleId = useId()
  const descriptionId = useId()
  const [endpointId, setEndpointId] = useState('')
  const [destinationId, setDestinationId] = useState('')
  const [clientErrors, setClientErrors] = useState<ClientErrors>({})
  const alertRef = useRef<HTMLParagraphElement>(null)
  const statusRef = useRef<HTMLParagraphElement>(null)
  const formRef = useRef<HTMLFormElement>(null)
  const [attempt, setAttempt] = useState(0)

  const endpointError =
    clientErrors.endpoint ?? (error?.field === 'endpoint' ? error.message : undefined)
  const destinationError =
    clientErrors.destination ?? (error?.field === 'destination' ? error.message : undefined)

  // Focus moves to the first invalid field after a submit attempt or a server answer, never while choosing.
  useEffect(() => {
    if (attempt === 0 && error === null) return
    formRef.current?.querySelector<HTMLElement>('[aria-invalid="true"]')?.focus()
  }, [attempt, error])
  useEffect(() => {
    if (submitting) statusRef.current?.focus()
  }, [submitting])
  useEffect(() => {
    if ((error !== null && error.field === null) || existing !== null) alertRef.current?.focus()
  }, [error, existing])

  const loadEndpoints = async (offset: number, signal: AbortSignal): Promise<PagedPage> => {
    const page = await listIngestionEndpoints(orgId, { limit: PICKER_PAGE_SIZE, offset }, signal)
    return {
      fetched: page.items.length,
      total: page.total,
      options: page.items
        .filter((endpoint) => endpoint.status !== 'REVOKED')
        .map((endpoint) => ({
          id: endpoint.id,
          label: `${endpoint.name} (${STATUS_LABELS[endpoint.status]})`,
        })),
    }
  }
  const loadDestinations = async (offset: number, signal: AbortSignal): Promise<PagedPage> => {
    const page = await listDestinationRecords(orgId, { limit: PICKER_PAGE_SIZE, offset }, signal)
    return {
      fetched: page.items.length,
      total: page.total,
      options: page.items
        .filter((destination) => destination.status !== 'REVOKED')
        .map((destination) => ({
          id: destination.id,
          label: `${destination.name} (${STATUS_LABELS[destination.status]})`,
        })),
    }
  }

  const submit = (event: FormEvent) => {
    event.preventDefault()
    const errors: ClientErrors = {}
    if (endpointId === '') errors.endpoint = 'Choose an ingestion endpoint.'
    if (destinationId === '') errors.destination = 'Choose a destination.'
    setClientErrors(errors)
    setAttempt((count) => count + 1)
    if (Object.keys(errors).length > 0) return
    onSubmit({ endpointId, destinationId })
  }

  return (
    <Dialog
      labelledBy={titleId}
      describedBy={descriptionId}
      onClose={onCancel}
      dismissible={!submitting}
    >
      <form ref={formRef} onSubmit={submit} noValidate className="dialog-body">
        <h2 id={titleId}>Create subscription</h2>
        <p id={descriptionId}>
          Events accepted by the ingestion endpoint from now on are delivered to the destination.
          Paused endpoints and destinations can be chosen; revoked ones cannot.
        </p>
        <PagedSelect
          label="Ingestion endpoint"
          noun="endpoints"
          value={endpointId}
          onChange={(id) => {
            setEndpointId(id)
            setClientErrors((previous) => ({ ...previous, endpoint: undefined }))
          }}
          load={loadEndpoints}
          autoLoad
          error={endpointError}
          disabled={submitting}
        />
        <PagedSelect
          label="Destination"
          noun="destinations"
          value={destinationId}
          onChange={(id) => {
            setDestinationId(id)
            setClientErrors((previous) => ({ ...previous, destination: undefined }))
          }}
          load={loadDestinations}
          autoLoad
          error={destinationError}
          disabled={submitting}
        />
        {existing !== null ? (
          <p className="form-error" role="alert" tabIndex={-1} ref={alertRef}>
            An active subscription already exists for this endpoint and destination. Nothing was
            created.{' '}
            <Link to={`/o/${orgId}/subscriptions/${encodeURIComponent(existing.id)}`}>Open it</Link>
            .
          </p>
        ) : null}
        {error !== null && error.field === null ? (
          <p className="form-error" role="alert" tabIndex={-1} ref={alertRef}>
            {error.message}
          </p>
        ) : null}
        {submitting ? (
          <p className="notice" role="status" tabIndex={-1} ref={statusRef}>
            Saving…
          </p>
        ) : null}
        <div className="dialog-actions">
          <Button variant="secondary" disabled={submitting} onClick={onCancel}>
            Cancel
          </Button>
          <Button type="submit" variant="primary" busy={submitting}>
            Create subscription
          </Button>
        </div>
      </form>
    </Dialog>
  )
}
