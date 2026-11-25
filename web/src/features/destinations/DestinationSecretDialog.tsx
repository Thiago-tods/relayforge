import { useId, useRef, useState } from 'react'
import { Button } from '../../ui/Button'
import { Dialog } from '../../ui/Dialog'

interface Props {
  kind: 'created' | 'rotated'
  destinationName: string
  /** The raw secret. It lives only in this component's props: never in storage, the URL, a toast or a log. */
  secret: string
  /** Called after the user confirmed that they saved the secret. The caller then discards the secret. */
  onAcknowledge: () => void
  /**
   * Called when the browser closed the dialog without the acknowledgment. The caller must discard the secret (it
   * can no longer be shown) and tell the user to rotate it.
   */
  onDiscard: () => void
}

/**
 * Shows a signing secret exactly once. The dialog cannot be dismissed with Escape, has no Cancel and closes only
 * after the user ticks that they saved the secret and presses Done. The copy button never puts the secret into an
 * accessible name, a toast or a message.
 */
export function DestinationSecretDialog({
  kind,
  destinationName,
  secret,
  onAcknowledge,
  onDiscard,
}: Props) {
  const titleId = useId()
  const descriptionId = useId()
  const [acknowledged, setAcknowledged] = useState(false)
  const [copyMessage, setCopyMessage] = useState('')
  const secretRef = useRef<HTMLElement>(null)

  async function copy() {
    try {
      await navigator.clipboard.writeText(secret)
      setCopyMessage('Secret copied to the clipboard.')
    } catch {
      setCopyMessage('Could not copy. Select the secret and copy it yourself.')
      const node = secretRef.current
      if (node) {
        const range = document.createRange()
        range.selectNodeContents(node)
        const selection = window.getSelection()
        selection?.removeAllRanges()
        selection?.addRange(range)
      }
    }
  }

  return (
    <Dialog
      labelledBy={titleId}
      describedBy={descriptionId}
      onClose={() => undefined}
      dismissible={false}
      onUnexpectedClose={onDiscard}
    >
      <div className="dialog-body">
        <h2 id={titleId}>{kind === 'created' ? 'Destination created' : 'New signing secret'}</h2>
        <div id={descriptionId} className="dialog-text">
          <p>
            This is the signing secret for <strong>{destinationName}</strong>. It is shown only now
            and cannot be retrieved later. Copy it and store it safely before you close this window.
          </p>
          {kind === 'rotated' ? (
            <p>
              New delivery attempts use this secret. Update every receiver that verifies signatures.
            </p>
          ) : null}
        </div>
        <p className="secret-box">
          <code ref={secretRef} data-testid="signing-secret">
            {secret}
          </code>
        </p>
        <div className="secret-actions">
          <Button variant="secondary" onClick={() => void copy()}>
            Copy secret
          </Button>
          <span role="status" className="muted">
            {copyMessage}
          </span>
        </div>
        <label className="ack">
          <input
            type="checkbox"
            checked={acknowledged}
            onChange={(event) => setAcknowledged(event.target.checked)}
          />
          <span>I have copied the secret and stored it safely.</span>
        </label>
        <div className="dialog-actions">
          <Button variant="primary" disabled={!acknowledged} onClick={onAcknowledge}>
            Done
          </Button>
        </div>
      </div>
    </Dialog>
  )
}
