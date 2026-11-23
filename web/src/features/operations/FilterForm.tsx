import type { FormEvent, ReactNode } from 'react'
import type { RangeErrors } from '../../lib/dateRange'
import { Button } from '../../ui/Button'
import { Field } from '../../ui/Field'
import { useFocusFirstError } from '../../ui/useFocusFirstError'
import { DEFAULT_WINDOW_HINT } from './activityLabels'

/** The From and To fields of a filter. Their values are read as UTC. */
export function RangeFields({
  from,
  to,
  errors,
  onFrom,
  onTo,
}: {
  from: string
  to: string
  errors: RangeErrors
  onFrom: (value: string) => void
  onTo: (value: string) => void
}) {
  return (
    <>
      <Field
        label="From (UTC)"
        type="datetime-local"
        value={from}
        onChange={(event) => onFrom(event.target.value)}
        error={errors.from}
      />
      <Field
        label="To (UTC)"
        type="datetime-local"
        value={to}
        onChange={(event) => onTo(event.target.value)}
        error={errors.to}
        hint={DEFAULT_WINDOW_HINT}
      />
    </>
  )
}

/**
 * The form around a section's filters. Submitting asks the section to validate and apply; a validation error moves
 * focus to the first invalid field.
 */
export function FilterForm({
  label,
  errors,
  onApply,
  onReset,
  children,
}: {
  label: string
  errors: object
  onApply: () => void
  onReset: () => void
  children: ReactNode
}) {
  const form = useFocusFirstError(errors)
  function submit(event: FormEvent) {
    event.preventDefault()
    onApply()
  }
  return (
    <form ref={form} className="filter-form" aria-label={label} onSubmit={submit} noValidate>
      <div className="filter-fields">{children}</div>
      <div className="filter-actions">
        <Button type="submit" variant="primary">
          Apply
        </Button>
        <Button onClick={onReset}>Reset</Button>
      </div>
    </form>
  )
}
