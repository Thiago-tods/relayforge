import { useEffect, useId, useRef, useState } from 'react'
import type { FormEvent } from 'react'
import type { DeliveryRecord } from '../../api/deliveryTypes'
import { adviseReason, reasonIssueMessage, REASON_MAX_CODE_POINTS } from '../../lib/redriveReason'
import { Button } from '../../ui/Button'
import { Dialog } from '../../ui/Dialog'
import { Field } from '../../ui/Field'
import { expectedBudget } from './redriveOutcome'
import type { DialogError } from './useRedrive'

interface Props {
  /** The delivery as shown when the dialog opened. */
  snapshot: DeliveryRecord
  submitting: boolean
  error: DialogError | null
  onSubmit: (reason: string) => void
  onCancel: () => void
}

function optional(value: string | number | null, fallback: string): string | number {
  return value === null || value === '' ? fallback : value
}

/**
 * Confirmation of a redrive. Everything it says about the retry budget is an estimate; the server decides. The reason
 * checks are advisory too: Confirm is never disabled by them, and the server's answer is shown when it disagrees.
 */
export function RedriveDialog({ snapshot, submitting, error, onSubmit, onCancel }: Props) {
  const titleId = useId()
  const descriptionId = useId()
  const [reason, setReason] = useState('')
  const [attempted, setAttempted] = useState(false)
  const formRef = useRef<HTMLFormElement>(null)
  const statusRef = useRef<HTMLParagraphElement>(null)
  const alertRef = useRef<HTMLParagraphElement>(null)

  const advice = adviseReason(reason)
  // An empty reason is only pointed out after the user tried to confirm; the other hints show as they type.
  const advisory =
    advice.issue !== null && (advice.issue !== 'empty' || attempted)
      ? reasonIssueMessage(advice.issue)
      : undefined
  const fieldError = error?.field ? error.message : advisory

  // Focus follows the state: the status text while the request runs, the error when it comes back.
  useEffect(() => {
    if (submitting) statusRef.current?.focus()
  }, [submitting])
  useEffect(() => {
    if (error === null) return
    if (error.field) formRef.current?.querySelector('input')?.focus()
    else alertRef.current?.focus()
  }, [error])

  const submit = (event: FormEvent) => {
    event.preventDefault()
    setAttempted(true)
    onSubmit(reason)
  }

  return (
    <Dialog
      labelledBy={titleId}
      describedBy={descriptionId}
      onClose={onCancel}
      dismissible={!submitting}
    >
      <form ref={formRef} onSubmit={submit} noValidate className="dialog-body">
        <h2 id={titleId}>Redrive this delivery?</h2>
        <p id={descriptionId}>
          Redrive returns this failed delivery to the normal delivery pipeline. It does not
          guarantee that the destination will accept it.
        </p>
        <dl className="detail-grid redrive-facts">
          <dt>Destination</dt>
          <dd>{snapshot.destinationName}</dd>
          <dt>Failure reason</dt>
          <dd>
            {optional(
              snapshot.failureReason,
              'Not recorded (this delivery failed before reasons were recorded)',
            )}
          </dd>
          <dt>Last HTTP status</dt>
          <dd>{optional(snapshot.lastHttpStatus, '—')}</dd>
          <dt>Last error class</dt>
          <dd>{optional(snapshot.lastErrorClass, '—')}</dd>
          <dt>Attempts made</dt>
          <dd>{snapshot.attemptCount}</dd>
          <dt>Retry budget</dt>
          <dd>
            {snapshot.maxAttempts} → {expectedBudget(snapshot.maxAttempts)} (expected)
          </dd>
        </dl>
        <ul className="redrive-notes">
          <li>
            The retry budget estimate is advisory. The server decides the final budget and reports
            it after the redrive. The attempt count and attempt history are kept.
          </li>
          <li>
            Delivery is at-least-once. The destination may receive the same delivery ID again, so
            its receiver must handle repeats.
          </li>
          <li>
            Sending is asynchronous. The first new attempt is due immediately; later attempts use
            the normal retry backoff and can be hours apart.
          </li>
          <li>
            If the destination is paused, the delivery may stay PENDING until the destination is
            activated. If it is revoked, the server rejects the redrive.
          </li>
          <li>
            The reason is saved in the audit log and is visible to people with audit access. Do not
            include secrets or personal data.
          </li>
        </ul>
        <Field
          label="Reason"
          name="reason"
          value={reason}
          onChange={(event) => setReason(event.target.value)}
          autoComplete="off"
          aria-required="true"
          hint={`${advice.count} / ${REASON_MAX_CODE_POINTS} characters. Use one line of plain text.`}
          error={fieldError}
        />
        {error !== null && !error.field ? (
          <p className="form-error" role="alert" tabIndex={-1} ref={alertRef}>
            {error.message}
          </p>
        ) : null}
        {submitting ? (
          <p className="notice" role="status" tabIndex={-1} ref={statusRef}>
            Redriving delivery…
          </p>
        ) : null}
        <div className="dialog-actions">
          <Button variant="secondary" disabled={submitting} onClick={onCancel}>
            Cancel
          </Button>
          <Button type="submit" variant="primary" busy={submitting}>
            Redrive delivery
          </Button>
        </div>
      </form>
    </Dialog>
  )
}
