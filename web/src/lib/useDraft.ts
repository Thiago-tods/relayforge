import { useState } from 'react'

/**
 * A draft that follows the applied filters: when the applied value changes (an Apply, a Reset, or the browser's
 * Back button) the draft is replaced by it. Until then, editing the draft changes nothing on the page.
 */
export function useDraft<T>(applied: T, appliedKey: string): [T, (draft: T) => void] {
  const [state, setState] = useState({ key: appliedKey, draft: applied })
  const set = (draft: T) => setState({ key: appliedKey, draft })
  if (state.key !== appliedKey) {
    // Adjusting state while rendering is the documented way to reset state when an input changes.
    setState({ key: appliedKey, draft: applied })
    return [applied, set]
  }
  return [state.draft, set]
}
