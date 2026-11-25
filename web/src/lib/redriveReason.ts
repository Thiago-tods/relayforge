// Advisory validation of a redrive reason (ADR 34). The backend is authoritative: it trims the reason with Go's
// strings.TrimSpace, then requires 1 to 500 runes of valid UTF-8 without control characters (below U+0020 or U+007F).
// These checks only give early feedback. JavaScript trim() and TrimSpace differ for a few characters, and a lone
// surrogate cannot be represented in UTF-8, so the UI never treats its own verdict as final and never blocks on it.

export const REASON_MAX_CODE_POINTS = 500

export type ReasonIssue = 'empty' | 'too_long' | 'control'

export interface ReasonAdvice {
  /** The first problem the client can see, or null when nothing suspicious is visible. */
  issue: ReasonIssue | null
  /** Unicode code points of the trimmed reason (an astral character counts once). */
  count: number
}

/** Counts Unicode code points. A valid surrogate pair counts once; a lone surrogate counts as one. */
export function countCodePoints(value: string): number {
  let count = 0
  for (let i = 0; i < value.length; i += 1) {
    const high = value.charCodeAt(i)
    if (high >= 0xd800 && high <= 0xdbff && i + 1 < value.length) {
      const low = value.charCodeAt(i + 1)
      if (low >= 0xdc00 && low <= 0xdfff) i += 1
    }
    count += 1
  }
  return count
}

// C0 controls (which include newline, carriage return and tab) and DEL. C1 controls are not rejected by the backend.
// eslint-disable-next-line no-control-regex
const CONTROL = /[\u0000-\u001f\u007f]/

export function adviseReason(value: string): ReasonAdvice {
  const trimmed = value.trim()
  const count = countCodePoints(trimmed)
  if (trimmed === '') return { issue: 'empty', count }
  if (CONTROL.test(trimmed)) return { issue: 'control', count }
  if (count > REASON_MAX_CODE_POINTS) return { issue: 'too_long', count }
  return { issue: null, count }
}

export function reasonIssueMessage(issue: ReasonIssue): string {
  switch (issue) {
    case 'empty':
      return 'A reason is required.'
    case 'too_long':
      return `This reason looks longer than ${REASON_MAX_CODE_POINTS} characters.`
    case 'control':
      return 'This reason contains a line break, tab or other control character. Use a single line of plain text.'
  }
}
