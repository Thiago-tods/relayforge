import { useEffect, useMemo, useState } from 'react'
import { useSearchParams } from 'react-router-dom'
import type { FailureItem } from '../../api/operationsTypes'
import { normalizedParams, readActivityQuery, withSection } from '../../lib/activityQuery'
import type { AttemptFilters, FailureFilters, SummaryFilters } from '../../lib/activityQuery'
import { attemptWindowFor } from '../../lib/dateRange'
import type { UtcRange } from '../../lib/dateRange'
import { Button } from '../../ui/Button'
import { truncatedAttemptsWarning } from './activityLabels'
import { ATTEMPTS_HEADING_ID, AttemptFeedSection } from './AttemptFeedSection'
import { DeliverySummarySection } from './DeliverySummarySection'
import { FailureFeedSection } from './FailureFeedSection'

/** The attempts of one delivery, as "Show attempts" asked for them. */
interface CrossLink {
  deliveryId: string
  range: UtcRange
  createdAt: string
  /** True when the delivery is older than the 31 days the attempt feed can span: its oldest attempts are excluded. */
  truncated: boolean
}

/**
 * Delivery activity of one organization: an aggregate summary and two cursor-paged feeds, each with its own filters,
 * date range, loading state and error state. Applied filters live in the URL; cursors and the default window the
 * server chose never do. Every read is made on open and again only when the user asks. Nothing here polls.
 */
export function ActivityPage() {
  const [params, setParams] = useSearchParams()
  const paramsText = params.toString()
  const query = useMemo(() => readActivityQuery(new URLSearchParams(paramsText)), [paramsText])
  const [crossLink, setCrossLink] = useState<CrossLink | null>(null)
  // Every "Show attempts" click moves focus to the attempt section, even when the URL does not change. The click
  // itself is the trigger, so nothing can be left pending for a later, unrelated URL change.
  const [focusRequest, setFocusRequest] = useState(0)

  /**
   * Applies one section's filters. Values of the link that were ignored are dropped at the same time, so the
   * "not valid" notice goes away once the user has applied anything.
   */
  function apply(
    section: 'summary' | 'failures' | 'attempts',
    filters: SummaryFilters | FailureFilters | AttemptFilters,
  ) {
    setParams(withSection(normalizedParams(params, query), section, filters), { replace: true })
  }

  useEffect(() => {
    if (focusRequest > 0) document.getElementById(ATTEMPTS_HEADING_ID)?.focus()
  }, [focusRequest])

  function showAttempts(item: FailureItem) {
    // The window comes from the delivery itself, not from the date range of the failure feed: every attempt of a
    // delivery starts between its creation and its latest completion. A delivery older than 31 days gets the last
    // 31 days and an explicit warning, because the attempt feed cannot span more.
    const window = attemptWindowFor(item.createdAt, item.completedAt)
    setCrossLink(
      window === null
        ? null
        : {
            deliveryId: item.deliveryId,
            range: window.range,
            createdAt: item.createdAt,
            truncated: window.truncated,
          },
    )
    apply('attempts', {
      range: window?.range ?? null,
      outcome: '',
      deliveryId: item.deliveryId,
    })
    setFocusRequest((n) => n + 1)
  }

  // The warning belongs to the filters "Show attempts" applied. If the user changes them, they chose another view.
  const attempts = query.attempts
  const warning =
    crossLink !== null &&
    crossLink.truncated &&
    attempts.deliveryId === crossLink.deliveryId &&
    attempts.range?.from === crossLink.range.from &&
    attempts.range?.to === crossLink.range.to
      ? truncatedAttemptsWarning(crossLink.createdAt)
      : null

  return (
    <div className="page">
      <header className="page-header">
        <h1>Activity</h1>
        <p>
          Delivery counts, recent failures and recorded attempts. Each section has its own date
          range and filters, and is read only when this page opens or you press Refresh.
        </p>
      </header>
      {query.ignored ? (
        <div className="notice" role="status">
          <p>Some filters in the link were not valid and were ignored.</p>
          <Button onClick={() => setParams(normalizedParams(params, query), { replace: true })}>
            Clear ignored filters
          </Button>
        </div>
      ) : null}
      <DeliverySummarySection
        applied={query.summary}
        onApply={(filters) => apply('summary', filters)}
      />
      <FailureFeedSection
        applied={query.failures}
        onApply={(filters) => apply('failures', filters)}
        onShowAttempts={showAttempts}
      />
      <AttemptFeedSection
        applied={query.attempts}
        onApply={(filters) => apply('attempts', filters)}
        warning={warning}
      />
    </div>
  )
}
