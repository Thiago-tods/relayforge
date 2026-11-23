import { ATTEMPT_OUTCOMES, FAILURE_REASONS } from '../api/operationsTypes'
import { readUrlRange } from './dateRange'
import type { UtcRange } from './dateRange'

// The applied filters of the Activity page, as they live in the URL query string. Each section owns the parameters
// with its own prefix, so applying one section never touches another. A cursor is never stored here, and neither is
// the default window the server picked: only an explicit range the user applied.

export interface SummaryFilters {
  range: UtcRange | null
}

export interface FailureFilters {
  range: UtcRange | null
  /** '' = any. */
  reason: string
  /** '' = every destination. */
  destinationId: string
}

export interface AttemptFilters {
  range: UtcRange | null
  /** '' = any. */
  outcome: string
  /** '' = every delivery. */
  deliveryId: string
}

export type Section = 'summary' | 'failures' | 'attempts'

const KEYS: Record<Section, readonly string[]> = {
  summary: ['sFrom', 'sTo'],
  failures: ['fFrom', 'fTo', 'fReason', 'fDest'],
  attempts: ['aFrom', 'aTo', 'aOutcome', 'aDelivery'],
}

const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i

/** A canonical (lower case) UUID, or null. */
export function normalizeUuid(value: string): string | null {
  const trimmed = value.trim()
  return UUID.test(trimmed) ? trimmed.toLowerCase() : null
}

export interface ActivityQuery {
  summary: SummaryFilters
  failures: FailureFilters
  attempts: AttemptFilters
  /** True when the link held a value that was not valid and was ignored. */
  ignored: boolean
}

export function readActivityQuery(params: URLSearchParams): ActivityQuery {
  let ignored = false

  function range(fromKey: string, toKey: string): UtcRange | null {
    const from = params.get(fromKey)
    const to = params.get(toKey)
    if (from === null && to === null) return null
    const parsed = from !== null && to !== null ? readUrlRange(from, to) : null
    if (parsed === null) ignored = true
    return parsed
  }

  function oneOf(key: string, allowed: readonly string[]): string {
    const value = params.get(key)
    if (value === null || value === '') return ''
    if (allowed.includes(value)) return value
    ignored = true
    return ''
  }

  function uuid(key: string): string {
    const value = params.get(key)
    if (value === null || value === '') return ''
    const parsed = normalizeUuid(value)
    if (parsed === null) ignored = true
    return parsed ?? ''
  }

  const summary = { range: range('sFrom', 'sTo') }
  const failures = {
    range: range('fFrom', 'fTo'),
    reason: oneOf('fReason', FAILURE_REASONS),
    destinationId: uuid('fDest'),
  }
  const attempts = {
    range: range('aFrom', 'aTo'),
    outcome: oneOf('aOutcome', ATTEMPT_OUTCOMES),
    deliveryId: uuid('aDelivery'),
  }
  return { summary, failures, attempts, ignored }
}

/** The URL parameters of one section's applied filters. Empty values are left out. */
export function sectionEntries(
  section: Section,
  filters: SummaryFilters | FailureFilters | AttemptFilters,
): [string, string][] {
  const entries: [string, string][] = []
  const [fromKey, toKey, ...rest] = KEYS[section] as [string, string, ...string[]]
  if (filters.range) {
    entries.push([fromKey, filters.range.from], [toKey, filters.range.to])
  }
  if ('reason' in filters && filters.reason !== '') entries.push([rest[0]!, filters.reason])
  if ('destinationId' in filters && filters.destinationId !== '') {
    entries.push([rest[1]!, filters.destinationId])
  }
  if ('outcome' in filters && filters.outcome !== '') entries.push([rest[0]!, filters.outcome])
  if ('deliveryId' in filters && filters.deliveryId !== '') {
    entries.push([rest[1]!, filters.deliveryId])
  }
  return entries
}

/** A copy of `params` in which one section's parameters are replaced. The other sections stay as they are. */
export function withSection(
  params: URLSearchParams,
  section: Section,
  filters: SummaryFilters | FailureFilters | AttemptFilters,
): URLSearchParams {
  const next = new URLSearchParams(params)
  for (const key of KEYS[section]) next.delete(key)
  for (const [key, value] of sectionEntries(section, filters)) next.set(key, value)
  return next
}

/**
 * A copy of `params` in which every section holds exactly the filters that were read from it. A value that was
 * ignored (not valid, half a range, a duplicate) is dropped and a value that was normalized (an upper-case id) is
 * written back normalized. Parameters this page does not own are left alone.
 */
export function normalizedParams(params: URLSearchParams, query: ActivityQuery): URLSearchParams {
  let next = withSection(params, 'summary', query.summary)
  next = withSection(next, 'failures', query.failures)
  return withSection(next, 'attempts', query.attempts)
}

/** A stable text for a section's applied filters, used to tell one query from another. */
export function filtersKey(filters: SummaryFilters | FailureFilters | AttemptFilters): string {
  return JSON.stringify(filters)
}
