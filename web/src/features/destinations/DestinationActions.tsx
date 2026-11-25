import { useState } from 'react'
import {
  activateDestination,
  pauseDestination,
  revokeDestination,
  rotateDestinationSecret,
  updateDestination,
} from '../../api/destinations'
import type { Destination } from '../../api/destinationTypes'
import { useOrg } from '../../org/orgContext'
import { Button } from '../../ui/Button'
import { useToast } from '../../ui/toastContext'
import { ActivateDialog, PauseDialog, RevokeDialog, RotateDialog } from './DestinationDialogs'
import { DestinationForm } from './DestinationForm'
import { DestinationSecretDialog } from './DestinationSecretDialog'
import { useDestinationMutation } from './useDestinationMutation'
import type { Reaction } from './useDestinationMutation'

export interface Notice {
  tone: 'info' | 'error'
  title: string
  message: string
}

type Open = 'rename' | 'url' | 'pause' | 'activate' | 'rotate' | 'revoke'

interface Props {
  destination: Destination
  /** True after the server answered 403: every control stays hidden for this page. */
  denied: boolean
  onDenied: () => void
  /** Receives the destination a successful operation returned, so the page can show it at once. */
  onChanged: (destination: Destination) => void
  onNotice: (notice: Notice | null) => void
  /** Reads the destination once more (after an ambiguous result or a conflict). */
  onReload: () => void
  /** Called when a dialog closed with a result, so the page can move focus. */
  onSettled: () => void
}

const OPERATION_NAMES: Record<Open, string> = {
  rename: 'The rename',
  url: 'The URL change',
  pause: 'Pausing',
  activate: 'Activation',
  rotate: 'The secret rotation',
  revoke: 'Revocation',
}

/**
 * The lifecycle controls of one destination and their dialogs. Controls follow the user's permissions, and the
 * backend authorizes every request again (a 403 hides the controls). Nothing is sent before its dialog is
 * confirmed. Idempotent operations are never repeated on their own and neither is a rotation: after an unknown
 * result the destination is read once and the user decides.
 */
export function DestinationActions({
  destination,
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
  const [open, setOpen] = useState<Open | null>(null)
  const [secret, setSecret] = useState<string | null>(null)
  const mutation = useDestinationMutation(`${orgId}:${destination.id}`)

  const revoked = destination.status === 'REVOKED'
  const allowed = (permission: Parameters<typeof can>[0]) => !denied && !revoked && can(permission)
  const canUpdate = allowed('destinations:update')
  const canPause = allowed('destinations:pause')
  const canRotate = allowed('destinations:rotate_secret')
  const canRevoke = allowed('destinations:revoke')

  const show = (dialog: Open) => {
    mutation.clearError()
    onNotice(null)
    setOpen(dialog)
  }
  const close = () => {
    setOpen(null)
    mutation.clearError()
  }

  const react = (reaction: Exclude<Reaction, { kind: 'stay' }>, operation: Open) => {
    setOpen(null)
    if (reaction.kind === 'forbidden') {
      onDenied()
      onNotice({
        tone: 'error',
        title: 'Not permitted',
        message: 'You no longer have permission to do this. Nothing was changed.',
      })
    } else if (reaction.kind === 'gone') {
      onNotice({
        tone: 'error',
        title: 'Destination not found',
        message:
          'This destination does not exist in this organization, or you do not have access to it. Nothing was changed.',
      })
      onReload()
    } else if (reaction.kind === 'revoked') {
      onNotice({
        tone: 'error',
        title: 'The destination is revoked',
        message: 'A revoked destination can no longer be changed. Nothing was changed.',
      })
      onReload()
    } else {
      onNotice({
        tone: 'info',
        title: 'The result is unknown',
        message:
          operation === 'rotate'
            ? 'The server did not answer in a way this page could read, so the secret may or may not have been rotated. If it was, the new secret could not be shown: rotate again to get one. The current state is being read.'
            : `${OPERATION_NAMES[operation]} may or may not have been applied. It was not repeated. The current state is being read.`,
      })
      onReload()
    }
    onSettled()
  }

  const finished = (updated: Destination, message: string) => {
    onChanged(updated)
    onNotice(null)
    setOpen(null)
    toast.push('success', message)
    onSettled()
  }

  const submitUpdate = (changes: { name?: string; url?: string }, message: string, op: Open) =>
    void mutation.run((signal) => updateDestination(orgId, destination.id, changes, signal), {
      onSuccess: (updated) => finished(updated, message),
      onReaction: (reaction) => react(reaction, op),
    })

  const none = !canUpdate && !canPause && !canRotate && !canRevoke

  return (
    <>
      {revoked ? (
        <p className="muted">
          This destination is revoked. Revocation is permanent: it cannot be reactivated or changed.
          Its delivery history stays available.
        </p>
      ) : none ? (
        <p className="muted">
          {denied
            ? 'You can no longer change this destination.'
            : 'You have read-only access to destinations.'}
        </p>
      ) : (
        <div className="action-row" role="group" aria-label="Destination actions">
          {canUpdate ? (
            <>
              <Button onClick={() => show('rename')}>Rename</Button>
              <Button onClick={() => show('url')}>Change URL</Button>
            </>
          ) : null}
          {canPause && destination.status === 'ACTIVE' ? (
            <Button onClick={() => show('pause')}>Pause</Button>
          ) : null}
          {canPause && destination.status === 'PAUSED' ? (
            <Button onClick={() => show('activate')}>Activate</Button>
          ) : null}
          {canRotate ? <Button onClick={() => show('rotate')}>Rotate signing secret</Button> : null}
          {canRevoke ? (
            <Button variant="danger" onClick={() => show('revoke')}>
              Revoke
            </Button>
          ) : null}
        </div>
      )}

      {open === 'rename' ? (
        <DestinationForm
          mode="rename"
          title="Rename destination"
          description="Choose a new name for this destination. Names are unique among active destinations."
          submitLabel="Save name"
          initialName={destination.name}
          submitting={mutation.submitting}
          error={mutation.error}
          onSubmit={(values) =>
            submitUpdate({ name: values.name }, 'Destination renamed.', 'rename')
          }
          onCancel={close}
        />
      ) : null}
      {open === 'url' ? (
        <DestinationForm
          mode="url"
          title="Change destination URL"
          description="Enter the full new URL, including any query parameters it needs."
          submitLabel="Save URL"
          notes={
            <ul className="dialog-notes">
              <li>
                Current URL: <code>{destination.url}</code>
                {destination.urlHasQuery ? ' (its query parameters are hidden)' : ''}
              </li>
              <li>
                The field starts empty because the stored query string is never shown. Enter every
                part of the new URL.
              </li>
              <li>
                Pending deliveries use the new URL on their next attempt. Attempts already recorded
                keep their history.
              </li>
            </ul>
          }
          submitting={mutation.submitting}
          error={mutation.error}
          onSubmit={(values) =>
            submitUpdate({ url: values.url }, 'Destination URL changed.', 'url')
          }
          onCancel={close}
        />
      ) : null}
      {open === 'pause' ? (
        <PauseDialog
          name={destination.name}
          submitting={mutation.submitting}
          error={mutation.error}
          onCancel={close}
          onConfirm={() =>
            void mutation.run((signal) => pauseDestination(orgId, destination.id, signal), {
              onSuccess: (updated) => finished(updated, 'Destination paused.'),
              onReaction: (reaction) => react(reaction, 'pause'),
            })
          }
        />
      ) : null}
      {open === 'activate' ? (
        <ActivateDialog
          name={destination.name}
          submitting={mutation.submitting}
          error={mutation.error}
          onCancel={close}
          onConfirm={() =>
            void mutation.run((signal) => activateDestination(orgId, destination.id, signal), {
              onSuccess: (updated) => finished(updated, 'Destination activated.'),
              onReaction: (reaction) => react(reaction, 'activate'),
            })
          }
        />
      ) : null}
      {open === 'rotate' ? (
        <RotateDialog
          name={destination.name}
          submitting={mutation.submitting}
          error={mutation.error}
          onCancel={close}
          onConfirm={() =>
            void mutation.run((signal) => rotateDestinationSecret(orgId, destination.id, signal), {
              onSuccess: (result) => {
                onChanged(result.destination)
                onNotice(null)
                setOpen(null)
                setSecret(result.signingSecret)
              },
              onReaction: (reaction) => react(reaction, 'rotate'),
            })
          }
        />
      ) : null}
      {open === 'revoke' ? (
        <RevokeDialog
          name={destination.name}
          submitting={mutation.submitting}
          error={mutation.error}
          onCancel={close}
          onConfirm={() =>
            void mutation.run((signal) => revokeDestination(orgId, destination.id, signal), {
              onSuccess: (updated) => finished(updated, 'Destination revoked.'),
              onReaction: (reaction) => react(reaction, 'revoke'),
            })
          }
        />
      ) : null}
      {secret !== null ? (
        <DestinationSecretDialog
          kind="rotated"
          destinationName={destination.name}
          secret={secret}
          onAcknowledge={() => {
            setSecret(null)
            onSettled()
          }}
          onDiscard={() => {
            setSecret(null)
            onNotice({
              tone: 'error',
              title: 'The signing secret was discarded',
              message:
                'The window closed unexpectedly before you confirmed that you saved the new secret, so it was discarded and cannot be shown again. Rotate the signing secret again to get a new one.',
            })
            onSettled()
          }}
        />
      ) : null}
    </>
  )
}
