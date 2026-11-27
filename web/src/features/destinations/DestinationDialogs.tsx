import { useEffect, useId, useRef, useState } from 'react'
import type { ReactNode } from 'react'
import { Button } from '../../ui/Button'
import { Dialog } from '../../ui/Dialog'
import type { MutationError } from './useDestinationMutation'

interface ConfirmProps {
  title: string
  confirmLabel: string
  variant?: 'primary' | 'danger'
  /** The first paragraph is the accessible description. */
  children: ReactNode
  /** When set, the user must tick this statement before the confirm button works. */
  acknowledgment?: string
  submitting: boolean
  error: MutationError | null
  onConfirm: () => void
  onCancel: () => void
}

/**
 * One confirmation for one lifecycle operation. Nothing is sent until the confirm button is pressed, a second press
 * while the request runs does nothing (the button is disabled and the hook guards the same tick), and Escape and
 * Cancel are ignored while it runs.
 */
export function ConfirmDialog({
  title,
  confirmLabel,
  variant = 'primary',
  children,
  acknowledgment,
  submitting,
  error,
  onConfirm,
  onCancel,
}: ConfirmProps) {
  const titleId = useId()
  const descriptionId = useId()
  const [acknowledged, setAcknowledged] = useState(false)
  const alertRef = useRef<HTMLParagraphElement>(null)
  const statusRef = useRef<HTMLParagraphElement>(null)

  useEffect(() => {
    if (submitting) statusRef.current?.focus()
  }, [submitting])
  useEffect(() => {
    if (error !== null) alertRef.current?.focus()
  }, [error])

  return (
    <Dialog
      labelledBy={titleId}
      describedBy={descriptionId}
      onClose={onCancel}
      dismissible={!submitting}
    >
      <form
        className="dialog-body"
        onSubmit={(event) => {
          event.preventDefault()
          if (acknowledgment !== undefined && !acknowledged) return
          onConfirm()
        }}
      >
        <h2 id={titleId}>{title}</h2>
        <div id={descriptionId} className="dialog-text">
          {children}
        </div>
        {acknowledgment !== undefined ? (
          <label className="ack">
            <input
              type="checkbox"
              checked={acknowledged}
              disabled={submitting}
              onChange={(event) => setAcknowledged(event.target.checked)}
            />
            <span>{acknowledgment}</span>
          </label>
        ) : null}
        {error !== null ? (
          <p className="form-error" role="alert" tabIndex={-1} ref={alertRef}>
            {error.message}
          </p>
        ) : null}
        {submitting ? (
          <p className="notice" role="status" tabIndex={-1} ref={statusRef}>
            Working…
          </p>
        ) : null}
        <div className="dialog-actions">
          <Button variant="secondary" disabled={submitting} onClick={onCancel}>
            Cancel
          </Button>
          <Button
            type="submit"
            variant={variant}
            busy={submitting}
            disabled={acknowledgment !== undefined && !acknowledged}
          >
            {confirmLabel}
          </Button>
        </div>
      </form>
    </Dialog>
  )
}

type Wrapper = Pick<ConfirmProps, 'submitting' | 'error' | 'onConfirm' | 'onCancel'> & {
  name: string
}

export function PauseDialog({ name, ...rest }: Wrapper) {
  return (
    <ConfirmDialog title="Pause this destination?" confirmLabel="Pause destination" {...rest}>
      <p>
        Pausing <strong>{name}</strong> stops new delivery attempts to it. Attempts already in
        flight finish. Pending deliveries stay pending until you activate the destination again.
      </p>
    </ConfirmDialog>
  )
}

export function ActivateDialog({ name, ...rest }: Wrapper) {
  return (
    <ConfirmDialog title="Activate this destination?" confirmLabel="Activate destination" {...rest}>
      <p>
        <strong>{name}</strong> becomes active again. Its pending deliveries become eligible for
        delivery attempts.
      </p>
    </ConfirmDialog>
  )
}

export function RotateDialog({ name, ...rest }: Wrapper) {
  return (
    <ConfirmDialog title="Rotate the signing secret?" confirmLabel="Rotate secret" {...rest}>
      <p>
        A new signing secret is generated for <strong>{name}</strong>. New delivery attempts use the
        new secret, so receivers that still verify with the old one will reject them until they are
        updated. A request already in flight may still carry the old secret.
      </p>
      <p>The new secret is shown once, right after this step. It cannot be retrieved later.</p>
    </ConfirmDialog>
  )
}

export function RevokeDialog({ name, ...rest }: Wrapper) {
  return (
    <ConfirmDialog
      title="Revoke this destination?"
      confirmLabel="Revoke destination"
      variant="danger"
      acknowledgment="I understand that revoking this destination is permanent."
      {...rest}
    >
      <p>
        Revoking <strong>{name}</strong> is permanent.
      </p>
      <ul>
        <li>The destination cannot be reactivated, renamed, re-targeted or rotated.</li>
        <li>
          Its pending deliveries are cancelled. Attempts already in flight are checked before
          sending.
        </li>
        <li>Its delivery history stays available.</li>
      </ul>
    </ConfirmDialog>
  )
}
