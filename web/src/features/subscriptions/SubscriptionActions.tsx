import { useState } from 'react'
import { revokeSubscription } from '../../api/subscriptions'
import type { Subscription } from '../../api/subscriptionTypes'
import { useOrg } from '../../org/orgContext'
import { Button } from '../../ui/Button'
import { useToast } from '../../ui/toastContext'
import { ConfirmDialog } from '../destinations/DestinationDialogs'
import type { PageNotice } from './NoticeBanner'
import { useSubscriptionMutation } from './useSubscriptionMutation'
import type { SubscriptionReaction } from './useSubscriptionMutation'

interface Props {
  subscription: Subscription
  /** The destination's name, for the confirmation text. */
  destinationName: string
  /** True after the server answered 403: the control stays hidden for this page. */
  denied: boolean
  onDenied: () => void
  /** Receives the subscription a successful revocation returned, so the page can show it at once. */
  onChanged: (subscription: Subscription) => void
  onNotice: (notice: PageNotice | null) => void
  /** Reads the subscription (and its history) once more, after an unknown result or a conflict. */
  onReload: () => void
  /** Called when the dialog closed with a result, so the page can move focus. */
  onSettled: () => void
}

/**
 * The one lifecycle action of a subscription: revoke it. The confirmation explains what revocation does, needs an
 * explicit acknowledgment, and nothing is sent before it. The request is never repeated: after an unknown result
 * the subscription is read once and the user decides.
 */
export function SubscriptionActions({
  subscription,
  destinationName,
  denied,
  onDenied,
  onChanged,
  onNotice,
  onReload,
  onSettled,
}: Props) {
  const { organization, can } = useOrg()
  const orgId = organization.id
  const toast = useToast()
  const [open, setOpen] = useState(false)
  const mutation = useSubscriptionMutation(`${orgId}:${subscription.id}`)

  const revoked = subscription.status === 'REVOKED'
  const canWrite = can('subscriptions:write') && !denied
  const canRevoke = canWrite && !revoked

  const react = (reaction: Exclude<SubscriptionReaction, { kind: 'stay' }>) => {
    setOpen(false)
    if (reaction.kind === 'forbidden') {
      onDenied()
      onNotice({
        tone: 'error',
        title: 'Not permitted',
        message: 'You no longer have permission to revoke subscriptions. Nothing was changed.',
      })
    } else if (reaction.kind === 'gone') {
      onNotice({
        tone: 'error',
        title: 'Subscription not found',
        message:
          'This subscription does not exist in this organization, or you do not have access to it. Nothing was changed.',
      })
      onReload()
    } else if (reaction.kind === 'alreadyRevoked') {
      onNotice({
        tone: 'info',
        title: 'Already revoked',
        message:
          'This subscription was already revoked, so nothing was changed. The current state is being read.',
      })
      onReload()
    } else {
      onNotice({
        tone: 'info',
        title: 'The result is unknown',
        message:
          'The revocation may or may not have been applied. It was not repeated. The current state is being read.',
      })
      onReload()
    }
    onSettled()
  }

  return (
    <>
      {revoked ? (
        <p className="muted">
          This subscription has ended. Revocation is permanent: it is never reactivated or changed.
          To deliver events to this destination again, create a new subscription.
        </p>
      ) : !canRevoke ? (
        <p className="muted">
          {denied
            ? 'You can no longer change this subscription.'
            : 'You have read-only access to subscriptions.'}
        </p>
      ) : (
        <div className="action-row" role="group" aria-label="Subscription actions">
          <Button
            variant="danger"
            onClick={() => {
              mutation.clearError()
              onNotice(null)
              setOpen(true)
            }}
          >
            Revoke subscription
          </Button>
        </div>
      )}

      {open ? (
        <ConfirmDialog
          title="Revoke this subscription?"
          confirmLabel="Revoke subscription"
          variant="danger"
          acknowledgment="I understand that revoking this subscription is permanent."
          submitting={mutation.submitting}
          error={mutation.error ? { message: mutation.error.message, field: null } : null}
          onCancel={() => {
            setOpen(false)
            mutation.clearError()
          }}
          onConfirm={() =>
            void mutation.run((signal) => revokeSubscription(orgId, subscription.id, signal), {
              onSuccess: (updated) => {
                onChanged(updated)
                onNotice(null)
                setOpen(false)
                toast.push('success', 'Subscription revoked.')
                onSettled()
              },
              onReaction: react,
            })
          }
        >
          <p>
            Revoking the subscription to <strong>{destinationName}</strong> is permanent.
          </p>
          <ul>
            <li>It is never reactivated. To deliver again, you create a new subscription.</li>
            <li>
              Events the endpoint accepted while it was active stay eligible for delivery. Only
              events accepted from now on are no longer sent through it.
            </li>
            <li>
              It does not change the destination or the endpoint, and nothing else is revoked.
            </li>
          </ul>
        </ConfirmDialog>
      ) : null}
    </>
  )
}
