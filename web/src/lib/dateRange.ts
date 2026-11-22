// UTC date-time ranges for the operations filters. The browser's own time zone and clock are never used: every value
// is read and written as UTC, and the backend's default window is never computed here (it is requested by sending
// no range at all).

/** The longest window the backend accepts (31 days, inclusive). */
export const MAX_RANGE_MS = 31 * 24 * 60 * 60 * 1000

/** A window of whole minutes in UTC: from inclusive, to exclusive. Both are `YYYY-MM-DDTHH:mm:00Z`. */
export interface UtcRange {
  from: string
  to: string
}

export interface RangeErrors {
  from?: string
  to?: string
}

const RFC_3339 = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2})(?::00)?Z$/
const INPUT_VALUE = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2})$/

function utcDate(match: RegExpExecArray): Date | null {
  const [year, month, day, hour, minute] = match.slice(1).map(Number) as [
    number,
    number,
    number,
    number,
    number,
  ]
  const date = new Date(0)
  date.setUTCFullYear(year, month - 1, day)
  date.setUTCHours(hour, minute, 0, 0)
  // Reject what the Date constructor would roll over (February 30th, hour 25) and the year 0000.
  const same =
    year >= 1 &&
    date.getUTCFullYear() === year &&
    date.getUTCMonth() === month - 1 &&
    date.getUTCDate() === day &&
    date.getUTCHours() === hour &&
    date.getUTCMinutes() === minute
  return same ? date : null
}

function toRfc3339(date: Date): string {
  return `${date.toISOString().slice(0, 16)}:00Z`
}

/** Reads the value of a datetime-local input (`2026-11-22T17:30`) as UTC. */
export function parseInputValue(value: string): Date | null {
  const match = INPUT_VALUE.exec(value)
  return match ? utcDate(match) : null
}

/** Reads a URL value (`2026-11-22T17:30:00Z`, seconds optional but zero) as UTC. */
export function parseUrlValue(value: string): Date | null {
  const match = RFC_3339.exec(value)
  return match ? utcDate(match) : null
}

/** The value a datetime-local input shows for an RFC 3339 string produced by this module. */
export function toInputValue(rfc3339: string): string {
  return parseUrlValue(rfc3339) ? rfc3339.slice(0, 16) : ''
}

/** Checks a range that was read from the URL. Anything that is not exactly a valid range is rejected. */
export function readUrlRange(from: string, to: string): UtcRange | null {
  const start = parseUrlValue(from)
  const end = parseUrlValue(to)
  if (!start || !end) return null
  if (end.getTime() <= start.getTime() || end.getTime() - start.getTime() > MAX_RANGE_MS)
    return null
  return { from: toRfc3339(start), to: toRfc3339(end) }
}

export type RangeCheck = { ok: true; range: UtcRange | null } | { ok: false; errors: RangeErrors }

/**
 * Validates the two datetime-local values of a form. Both empty means "the default window" (range null). The
 * backend validates again; this only gives a clear message before a request is sent.
 */
export function checkRange(fromValue: string, toValue: string): RangeCheck {
  if (fromValue === '' && toValue === '') return { ok: true, range: null }
  const errors: RangeErrors = {}
  const start = fromValue === '' ? null : parseInputValue(fromValue)
  const end = toValue === '' ? null : parseInputValue(toValue)
  if (fromValue === '')
    errors.from = 'Enter both dates, or leave both empty for the default window.'
  else if (!start) errors.from = 'Enter a valid UTC date and time.'
  if (toValue === '') errors.to = 'Enter both dates, or leave both empty for the default window.'
  else if (!end) errors.to = 'Enter a valid UTC date and time.'
  if (start && end) {
    if (end.getTime() <= start.getTime()) errors.to = 'To must be after From.'
    else if (end.getTime() - start.getTime() > MAX_RANGE_MS) {
      errors.to = 'The window must not exceed 31 days.'
    }
  }
  if (errors.from !== undefined || errors.to !== undefined) return { ok: false, errors }
  return { ok: true, range: { from: toRfc3339(start!), to: toRfc3339(end!) } }
}

export interface AttemptWindow {
  range: UtcRange
  /**
   * True when the delivery lived longer than the 31 days the attempt feed can span, so the range was cut and the
   * oldest attempts are NOT in it.
   */
  truncated: boolean
}

/**
 * The window that holds the attempts of one delivery: from the minute it was created to the first whole minute at
 * or after its latest completion. Every attempt of a delivery starts between those two instants, so the window is
 * complete when it fits the backend's 31-day maximum. When it does not, it is cut to the 31 days that end at the
 * latest completion and `truncated` is set, so the caller can say that older attempts are excluded. Both instants come
 * from the delivery itself (the server), never from the browser's clock or from the date range of another feed.
 */
export function attemptWindowFor(createdAt: string, completedAt: string): AttemptWindow | null {
  const created = new Date(createdAt).getTime()
  const completed = new Date(completedAt).getTime()
  if (!Number.isFinite(created) || !Number.isFinite(completed)) return null
  const minute = 60_000
  const to = Math.ceil(completed / minute) * minute
  let from = Math.floor(created / minute) * minute
  // The end is exclusive: a window must contain at least one minute.
  if (from >= to) from = to - minute
  let truncated = false
  if (to - from > MAX_RANGE_MS) {
    from = to - MAX_RANGE_MS
    truncated = true
  }
  if (new Date(from).getUTCFullYear() < 1 || new Date(to).getUTCFullYear() > 9999) return null
  return { range: { from: toRfc3339(new Date(from)), to: toRfc3339(new Date(to)) }, truncated }
}
