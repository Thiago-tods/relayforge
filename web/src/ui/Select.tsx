import { useId } from 'react'
import type { SelectHTMLAttributes } from 'react'

interface SelectProps extends Omit<SelectHTMLAttributes<HTMLSelectElement>, 'id'> {
  label: string
  hint?: string | undefined
}

/** A labeled native select, laid out like Field. */
export function Select({ label, hint, children, ...select }: SelectProps) {
  const id = useId()
  const hintId = `${id}-hint`
  return (
    <div className="field">
      <label htmlFor={id}>{label}</label>
      <select {...select} id={id} aria-describedby={hint ? hintId : undefined}>
        {children}
      </select>
      {hint ? (
        <p id={hintId} className="field-hint">
          {hint}
        </p>
      ) : null}
    </div>
  )
}
