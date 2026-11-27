import { useEffect, useRef } from 'react'
import type { KeyboardEvent, ReactNode, RefObject, SyntheticEvent } from 'react'

interface DialogProps {
  /** Id of the element that names the dialog. */
  labelledBy: string
  /** Id of the element that describes it. */
  describedBy?: string
  /** Called when the user asks to close (Escape). Ignored while `dismissible` is false. */
  onClose: () => void
  /** False while work is in flight: Escape then does nothing. */
  dismissible?: boolean
  /** The element that receives focus when the dialog opens. Defaults to the first focusable element. */
  initialFocus?: RefObject<HTMLElement | null>
  /**
   * Called when the browser closes the dialog by itself while it is mounted (an unexpected native close). A normal
   * unmount does not fire it. The owner should discard whatever the dialog shows.
   */
  onUnexpectedClose?: () => void
  children: ReactNode
}

const FOCUSABLE =
  'a[href], button:not([disabled]), input:not([disabled]), select, textarea, [tabindex]:not([tabindex="-1"])'

/**
 * A modal dialog on the native <dialog> element. It is mounted only while it is open and opened with showModal(),
 * so the browser traps focus inside it, makes the rest of the page inert and draws a backdrop. When it unmounts,
 * focus returns to the element that was focused before it opened (a caller that wants focus elsewhere moves it
 * afterwards; the dialog's cleanup runs first).
 */
export function Dialog({
  labelledBy,
  describedBy,
  onClose,
  dismissible = true,
  initialFocus,
  onUnexpectedClose,
  children,
}: DialogProps) {
  const ref = useRef<HTMLDialogElement>(null)
  const unexpectedClose = useRef(onUnexpectedClose)
  useEffect(() => {
    unexpectedClose.current = onUnexpectedClose
  })

  useEffect(() => {
    const dialog = ref.current
    if (dialog === null) return undefined
    const previous = document.activeElement
    if (!dialog.open) dialog.showModal()
    const target = initialFocus?.current ?? dialog.querySelector<HTMLElement>(FOCUSABLE)
    target?.focus()
    const onNativeClose = () => unexpectedClose.current?.()
    dialog.addEventListener('close', onNativeClose)
    return () => {
      dialog.removeEventListener('close', onNativeClose)
      if (previous instanceof HTMLElement && previous.isConnected) previous.focus()
    }
    // Opening happens once per mount; later changes of the focus target must not steal focus.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  // Escape is handled here and the native close is cancelled, so the owner decides whether the dialog closes.
  const request = (event: SyntheticEvent) => {
    event.preventDefault()
    if (dismissible) onClose()
  }
  const onKeyDown = (event: KeyboardEvent<HTMLDialogElement>) => {
    if (event.key === 'Escape') request(event)
  }

  return (
    <dialog
      ref={ref}
      className="dialog"
      aria-labelledby={labelledBy}
      aria-describedby={describedBy}
      onCancel={request}
      onKeyDown={onKeyDown}
    >
      {children}
    </dialog>
  )
}
