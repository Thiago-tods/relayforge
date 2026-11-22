import { useEffect, useRef } from 'react'

/**
 * Moves focus to the first invalid field of a form whenever its validation errors change. The field announces
 * its label and its linked error message, so keyboard and screen-reader users learn that submitting failed and
 * where. Fields mark themselves with aria-invalid (see Field).
 */
export function useFocusFirstError(errors: object) {
  const form = useRef<HTMLFormElement>(null)
  useEffect(() => {
    form.current?.querySelector<HTMLElement>('[aria-invalid="true"]')?.focus()
  }, [errors])
  return form
}
