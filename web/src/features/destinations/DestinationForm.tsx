import { useEffect, useId, useRef, useState } from 'react'
import type { FormEvent, ReactNode } from 'react'
import { countCodePoints } from '../../lib/redriveReason'
import { Button } from '../../ui/Button'
import { Dialog } from '../../ui/Dialog'
import { Field } from '../../ui/Field'
import type { MutationError } from './useDestinationMutation'

export const NAME_MAX_CODE_POINTS = 100

interface FieldErrors {
  name?: string | undefined
  url?: string | undefined
}

export type FormMode = 'create' | 'rename' | 'url'

export interface FormValues {
  name: string
  url: string
}

interface Props {
  mode: FormMode
  title: string
  submitLabel: string
  /** Only for `rename`: the current name. The URL is never prefilled (see the change-URL notes). */
  initialName?: string
  /** Text under the heading (also the accessible description). */
  description: string
  /** Extra notes shown above the fields. */
  notes?: ReactNode
  submitting: boolean
  error: MutationError | null
  onSubmit: (values: FormValues) => void
  onCancel: () => void
}

/** Client checks only look for what is missing or plainly too long. The backend validates names and URLs. */
function validate(mode: FormMode, values: FormValues): FieldErrors {
  const errors: FieldErrors = {}
  if (mode !== 'url') {
    const name = values.name.trim()
    if (name === '') errors.name = 'Enter a name.'
    else if (countCodePoints(name) > NAME_MAX_CODE_POINTS) {
      errors.name = `Use at most ${NAME_MAX_CODE_POINTS} characters.`
    }
  }
  if (mode !== 'rename' && values.url.trim() === '') errors.url = 'Enter the destination URL.'
  return errors
}

/**
 * The dialog for creating a destination (name and URL), renaming it (name) or changing its URL (URL). The URL
 * field starts empty on purpose: the API shows the URL without its query string, so sending the displayed value
 * back would silently remove the query. The user enters the full new URL.
 */
export function DestinationForm({
  mode,
  title,
  submitLabel,
  initialName = '',
  description,
  notes,
  submitting,
  error,
  onSubmit,
  onCancel,
}: Props) {
  const titleId = useId()
  const descriptionId = useId()
  const [name, setName] = useState(initialName)
  const [url, setUrl] = useState('')
  const [clientErrors, setClientErrors] = useState<FieldErrors>({})
  const alertRef = useRef<HTMLParagraphElement>(null)
  const statusRef = useRef<HTMLParagraphElement>(null)

  // A server answer for a field replaces the client message of that field and moves focus to the field.
  const nameError = clientErrors.name ?? (error?.field === 'name' ? error.message : undefined)
  const urlError = clientErrors.url ?? (error?.field === 'url' ? error.message : undefined)
  const formRef = useRef<HTMLFormElement>(null)
  // Focus moves to the first invalid field after a submit attempt or a server answer, never while typing.
  const [attempt, setAttempt] = useState(0)
  useEffect(() => {
    if (attempt === 0 && error === null) return
    formRef.current?.querySelector<HTMLElement>('[aria-invalid="true"]')?.focus()
  }, [attempt, error])

  useEffect(() => {
    if (submitting) statusRef.current?.focus()
  }, [submitting])
  useEffect(() => {
    if (error !== null && error.field === null) alertRef.current?.focus()
  }, [error])

  const submit = (event: FormEvent) => {
    event.preventDefault()
    const values = { name, url }
    const errors = validate(mode, values)
    setClientErrors(errors)
    setAttempt((count) => count + 1)
    if (Object.keys(errors).length > 0) return
    onSubmit({ name: name.trim(), url: url.trim() })
  }

  return (
    <Dialog
      labelledBy={titleId}
      describedBy={descriptionId}
      onClose={onCancel}
      dismissible={!submitting}
    >
      <form ref={formRef} onSubmit={submit} noValidate className="dialog-body">
        <h2 id={titleId}>{title}</h2>
        <p id={descriptionId}>{description}</p>
        {notes}
        {mode !== 'url' ? (
          <Field
            label="Name"
            name="name"
            value={name}
            onChange={(event) => {
              setName(event.target.value)
              setClientErrors((previous) => ({ ...previous, name: undefined }))
            }}
            autoComplete="off"
            aria-required="true"
            hint={`Up to ${NAME_MAX_CODE_POINTS} characters. Names are unique among active destinations.`}
            error={nameError}
          />
        ) : null}
        {mode !== 'rename' ? (
          <Field
            label={mode === 'url' ? 'New URL' : 'HTTPS URL'}
            name="url"
            type="url"
            inputMode="url"
            value={url}
            onChange={(event) => {
              setUrl(event.target.value)
              setClientErrors((previous) => ({ ...previous, url: undefined }))
            }}
            autoComplete="off"
            spellCheck={false}
            aria-required="true"
            hint="The server checks the URL and rejects addresses it does not allow."
            error={urlError}
          />
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
            {submitLabel}
          </Button>
        </div>
      </form>
    </Dialog>
  )
}
