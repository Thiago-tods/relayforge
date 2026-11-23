import { useState } from 'react'
import { Link } from 'react-router-dom'
import { getAttempts } from '../../api/operations'
import { filtersKey, normalizeUuid } from '../../lib/activityQuery'
import type { AttemptFilters } from '../../lib/activityQuery'
import { checkRange, toInputValue } from '../../lib/dateRange'
import type { RangeErrors } from '../../lib/dateRange'
import { useCursorFeed } from '../../lib/useCursorFeed'
import { useDraft } from '../../lib/useDraft'
import { useNewActivity } from '../../lib/useNewActivity'
import { useOrg } from '../../org/orgContext'
import { CopyText } from '../../ui/CopyText'
import { DateTime } from '../../ui/DateTime'
import { Field } from '../../ui/Field'
import { Select } from '../../ui/Select'
import {
  ATTEMPTS_INTRO,
  ATTEMPTS_NO_DESTINATION_NOTE,
  DELIVERY_WINDOW_NOTE,
  FEED_TIMEOUT_HINT,
  OUTCOME_OPTIONS,
  outcomeInfo,
  reasonInfo,
} from './activityLabels'
import { FeedFrame } from './FeedFrame'
import { FilterForm, RangeFields } from './FilterForm'

export const ATTEMPTS_HEADING_ID = 'activity-attempts-heading'

/** Rows per page. The server's own default is 50. */
const PAGE_SIZE = 25

interface Draft {
  from: string
  to: string
  outcome: string
  deliveryId: string
}

interface Errors extends RangeErrors {
  deliveryId?: string
}

function draftOf(applied: AttemptFilters): Draft {
  return {
    from: applied.range ? toInputValue(applied.range.from) : '',
    to: applied.range ? toInputValue(applied.range.to) : '',
    outcome: applied.outcome,
    deliveryId: applied.deliveryId,
  }
}

/**
 * The recorded attempts of the organization (GET /operations/attempts). An attempt is not a delivery's final state:
 * the marker in the last column appears only when the backend says the attempt ended a failure episode.
 */
export function AttemptFeedSection({
  applied,
  onApply,
  warning,
}: {
  applied: AttemptFilters
  onApply: (filters: AttemptFilters) => void
  /** Said above the table when the shown attempts are known not to be the whole history of the delivery. */
  warning: string | null
}) {
  const orgId = useOrg().organization.id
  const appliedKey = filtersKey(applied)
  const [draft, setDraft] = useDraft(draftOf(applied), appliedKey)
  const [errors, setErrors] = useState<Errors>({})

  const feed = useCursorFeed(orgId, appliedKey, (cursor, pinned, signal) =>
    getAttempts(
      orgId,
      {
        limit: PAGE_SIZE,
        cursor,
        outcome: applied.outcome,
        deliveryId: applied.deliveryId,
        from: applied.range?.from ?? pinned?.from,
        to: applied.range?.to ?? pinned?.to,
      },
      signal,
    ),
  )

  // Checks for newer attempts without touching the pages held, the cursors or the position (see the failure feed).
  const newActivity = useNewActivity({
    base: `${orgId}|${appliedKey}`,
    first: feed.firstPage,
    busy: feed.loading,
    probe: (signal) =>
      getAttempts(
        orgId,
        {
          limit: 1,
          outcome: applied.outcome,
          deliveryId: applied.deliveryId,
          from: applied.range?.from,
          to: applied.range?.to,
        },
        signal,
      ),
    id: (item) => item.attemptId,
    time: (item) => item.startedAt,
  })

  function submit(next: AttemptFilters) {
    if (filtersKey(next) === appliedKey) feed.refresh()
    else onApply(next)
  }

  function apply() {
    const check = checkRange(draft.from, draft.to)
    const rangeErrors: Errors = check.ok ? {} : check.errors
    const deliveryId = draft.deliveryId.trim() === '' ? '' : normalizeUuid(draft.deliveryId)
    if (deliveryId === null) rangeErrors.deliveryId = 'Enter a delivery ID in UUID format.'
    if (!check.ok || deliveryId === null) {
      setErrors(rangeErrors)
      return
    }
    setErrors({})
    submit({ range: check.range, outcome: draft.outcome, deliveryId })
  }

  function reset() {
    const empty: AttemptFilters = { range: null, outcome: '', deliveryId: '' }
    setErrors({})
    setDraft(draftOf(empty))
    submit(empty)
  }

  return (
    <FeedFrame
      headingId={ATTEMPTS_HEADING_ID}
      title="Attempt activity"
      intro={
        <>
          {ATTEMPTS_INTRO} {ATTEMPTS_NO_DESTINATION_NOTE}
        </>
      }
      feed={feed}
      newActivity={newActivity}
      emptyText="No attempts match these filters in this window."
      timeoutHint={FEED_TIMEOUT_HINT}
      filters={
        <>
          <FilterForm label="Attempt filters" errors={errors} onApply={apply} onReset={reset}>
            <RangeFields
              from={draft.from}
              to={draft.to}
              errors={errors}
              onFrom={(from) => setDraft({ ...draft, from })}
              onTo={(to) => setDraft({ ...draft, to })}
            />
            <Select
              label="Outcome"
              value={draft.outcome}
              onChange={(event) => setDraft({ ...draft, outcome: event.target.value })}
            >
              <option value="">Any outcome</option>
              {OUTCOME_OPTIONS.map((option) => (
                <option key={option.value} value={option.value}>
                  {option.label}
                </option>
              ))}
            </Select>
            <Field
              label="Delivery ID"
              value={draft.deliveryId}
              onChange={(event) => setDraft({ ...draft, deliveryId: event.target.value })}
              error={errors.deliveryId}
              autoComplete="off"
              spellCheck={false}
            />
          </FilterForm>
          {warning === null ? null : (
            <p className="notice" role="status">
              {warning}
            </p>
          )}
          {applied.deliveryId === '' ? null : <p className="muted">{DELIVERY_WINDOW_NOTE}</p>}
          <details className="legend">
            <summary>What the outcomes mean</summary>
            <dl className="details">
              {OUTCOME_OPTIONS.map((option) => (
                <div key={option.value} className="legend-row">
                  <dt>{option.label}</dt>
                  <dd>{outcomeInfo(option.value).meaning}</dd>
                </div>
              ))}
            </dl>
          </details>
        </>
      }
    >
      {(items) => (
        <table className="data-table">
          <caption className="visually-hidden">Recorded attempts, newest start first</caption>
          <thead>
            <tr>
              <th scope="col">Started (UTC)</th>
              <th scope="col" className="num">
                Attempt
              </th>
              <th scope="col">Delivery</th>
              <th scope="col">Destination</th>
              <th scope="col">Outcome</th>
              <th scope="col">HTTP</th>
              <th scope="col">Error class</th>
              <th scope="col" className="num">
                Duration
              </th>
              <th scope="col">Failure episode</th>
            </tr>
          </thead>
          <tbody>
            {items.map((item) => (
              <tr key={item.attemptId}>
                <td>
                  <DateTime value={item.startedAt} />
                </td>
                <td className="num">#{item.attemptNumber}</td>
                <td>
                  <span className="delivery-cell">
                    <CopyText value={item.deliveryId} what="Delivery ID" />
                    <Link
                      to={`/o/${orgId}/operations/deliveries/${encodeURIComponent(item.deliveryId)}`}
                    >
                      View
                    </Link>
                  </span>
                </td>
                <td className="wrap">{item.destinationName}</td>
                <td>
                  <span className="badge">{outcomeInfo(item.outcome).label}</span>
                </td>
                <td>{item.httpStatus ?? '—'}</td>
                <td>{item.errorClass ?? '—'}</td>
                <td className="num">{item.durationMs.toLocaleString('en-US')} ms</td>
                <td>
                  {item.terminalReason === null
                    ? '—'
                    : `Ended a failure episode: ${reasonInfo(item.terminalReason).label}`}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </FeedFrame>
  )
}
