import { useState } from 'react'
import { getFailures } from '../../api/operations'
import type { FailureItem } from '../../api/operationsTypes'
import { filtersKey } from '../../lib/activityQuery'
import type { FailureFilters } from '../../lib/activityQuery'
import { checkRange, toInputValue } from '../../lib/dateRange'
import type { RangeErrors } from '../../lib/dateRange'
import { useCursorFeed } from '../../lib/useCursorFeed'
import { useDraft } from '../../lib/useDraft'
import { useOrg } from '../../org/orgContext'
import { Button } from '../../ui/Button'
import { CopyText } from '../../ui/CopyText'
import { DateTime } from '../../ui/DateTime'
import { Select } from '../../ui/Select'
import { FAILURES_INTRO, FEED_TIMEOUT_HINT, REASON_OPTIONS, reasonInfo } from './activityLabels'
import { DestinationSelect } from './DestinationSelect'
import { FeedFrame } from './FeedFrame'
import { FilterForm, RangeFields } from './FilterForm'

export const FAILURES_HEADING_ID = 'activity-failures-heading'

/** Rows per page. The server's own default is 50. */
const PAGE_SIZE = 25

interface Draft {
  from: string
  to: string
  reason: string
  destinationId: string
}

function draftOf(applied: FailureFilters): Draft {
  return {
    from: applied.range ? toInputValue(applied.range.from) : '',
    to: applied.range ? toInputValue(applied.range.to) : '',
    reason: applied.reason,
    destinationId: applied.destinationId,
  }
}

function lastResult(item: FailureItem): string {
  const parts = [
    item.lastHttpStatus === null ? '' : `HTTP ${item.lastHttpStatus}`,
    item.lastErrorClass ?? '',
  ].filter((part) => part !== '')
  return parts.length > 0 ? parts.join(' · ') : '—'
}

/**
 * The deliveries that are failed right now (GET /operations/failures). The destination filter is applied by the
 * server through destinationId. Rows are never filtered in the browser.
 */
export function FailureFeedSection({
  applied,
  onApply,
  onShowAttempts,
}: {
  applied: FailureFilters
  onApply: (filters: FailureFilters) => void
  /** Asks the page to show the attempts of the delivery of this row. */
  onShowAttempts: (item: FailureItem) => void
}) {
  const orgId = useOrg().organization.id
  const appliedKey = filtersKey(applied)
  const [draft, setDraft] = useDraft(draftOf(applied), appliedKey)
  const [errors, setErrors] = useState<RangeErrors>({})

  const feed = useCursorFeed(orgId, appliedKey, (cursor, pinned, signal) =>
    getFailures(
      orgId,
      {
        limit: PAGE_SIZE,
        cursor,
        failureReason: applied.reason,
        destinationId: applied.destinationId,
        // An explicit range is always sent. Otherwise only the window the server chose for the first page.
        from: applied.range?.from ?? pinned?.from,
        to: applied.range?.to ?? pinned?.to,
      },
      signal,
    ),
  )

  function submit(next: FailureFilters) {
    if (filtersKey(next) === appliedKey) feed.refresh()
    else onApply(next)
  }

  function apply() {
    const check = checkRange(draft.from, draft.to)
    if (!check.ok) {
      setErrors(check.errors)
      return
    }
    setErrors({})
    submit({ range: check.range, reason: draft.reason, destinationId: draft.destinationId })
  }

  function reset() {
    const empty: FailureFilters = { range: null, reason: '', destinationId: '' }
    setErrors({})
    setDraft(draftOf(empty))
    submit(empty)
  }

  return (
    <FeedFrame
      headingId={FAILURES_HEADING_ID}
      title="Recent failures"
      intro={FAILURES_INTRO}
      feed={feed}
      emptyText="No failed deliveries match these filters in this window."
      timeoutHint={FEED_TIMEOUT_HINT}
      filters={
        <>
          <FilterForm label="Failure filters" errors={errors} onApply={apply} onReset={reset}>
            <RangeFields
              from={draft.from}
              to={draft.to}
              errors={errors}
              onFrom={(from) => setDraft({ ...draft, from })}
              onTo={(to) => setDraft({ ...draft, to })}
            />
            <Select
              label="Failure reason"
              value={draft.reason}
              onChange={(event) => setDraft({ ...draft, reason: event.target.value })}
            >
              <option value="">Any reason</option>
              {REASON_OPTIONS.map((option) => (
                <option key={option.value} value={option.value}>
                  {option.label}
                </option>
              ))}
            </Select>
            <DestinationSelect
              orgId={orgId}
              value={draft.destinationId}
              onChange={(destinationId) => setDraft({ ...draft, destinationId })}
            />
          </FilterForm>
          <details className="legend">
            <summary>What the failure reasons mean</summary>
            <dl className="details">
              {REASON_OPTIONS.map((option) => (
                <div key={option.value} className="legend-row">
                  <dt>{option.label}</dt>
                  <dd>{reasonInfo(option.value).meaning}</dd>
                </div>
              ))}
            </dl>
          </details>
        </>
      }
    >
      {(items) => (
        <table className="data-table">
          <caption className="visually-hidden">Failed deliveries, newest completion first</caption>
          <thead>
            <tr>
              <th scope="col">Completed (UTC)</th>
              <th scope="col">Destination</th>
              <th scope="col">Reason</th>
              <th scope="col" className="num">
                Attempts
              </th>
              <th scope="col">Last result</th>
              <th scope="col">Delivery</th>
              <th scope="col">Attempt history</th>
            </tr>
          </thead>
          <tbody>
            {items.map((item) => (
              <tr key={item.deliveryId}>
                <td>
                  <DateTime value={item.completedAt} />
                </td>
                <td className="wrap">{item.destinationName}</td>
                <td>
                  <span className="badge">{reasonInfo(item.failureReason).label}</span>
                </td>
                <td className="num">{item.attemptCount.toLocaleString('en-US')}</td>
                <td>{lastResult(item)}</td>
                <td>
                  <CopyText value={item.deliveryId} what="Delivery ID" />
                </td>
                <td>
                  <Button
                    variant="ghost"
                    aria-label={`Show attempts of delivery ${item.deliveryId}`}
                    onClick={() => onShowAttempts(item)}
                  >
                    Show attempts
                  </Button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </FeedFrame>
  )
}
