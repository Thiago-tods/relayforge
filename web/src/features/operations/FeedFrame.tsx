import { useEffect, useRef } from 'react'
import type { ReactNode } from 'react'
import type { CursorFeed } from '../../lib/useCursorFeed'
import type { NewActivity } from '../../lib/useNewActivity'
import { Button } from '../../ui/Button'
import { DateTime } from '../../ui/DateTime'
import { Pager } from '../../ui/Pager'
import { LoadingState } from '../../ui/states'
import { FEED_FRESHNESS_NOTE, NEW_ACTIVITY_CHECK_FAILED, NEW_ACTIVITY_NOTE } from './activityLabels'
import { ErrorBanner } from './ErrorBanner'

/**
 * What the two feeds share: heading, manual refresh, filters, the window that was read, loading and error states,
 * the empty state, the table area and the pager. The rows themselves come from the caller.
 */
export function FeedFrame<T>({
  headingId,
  title,
  intro,
  filters,
  feed,
  newActivity,
  emptyText,
  timeoutHint,
  children,
}: {
  /** The id of the section heading. The page uses it to move focus here. */
  headingId: string
  title: string
  intro: ReactNode
  filters: ReactNode
  feed: CursorFeed<T>
  /** What the periodic check for newer entries found. Reloading is always the user's choice. */
  newActivity: NewActivity
  emptyText: string
  timeoutHint: string
  children: (items: T[]) => ReactNode
}) {
  const moved = useRef(false)
  const { page, loading, error } = feed

  // After Previous or Next, focus returns to the heading so a keyboard or screen-reader user starts at the results.
  // The request is a navigation only while it runs: once it settles the flag is cleared whatever the outcome, so a
  // failed page request can never move focus later, for example after an unrelated Refresh.
  useEffect(() => {
    if (!moved.current || loading) return
    moved.current = false
    if (error === null && page !== null) document.getElementById(headingId)?.focus()
  }, [page, loading, error, headingId])

  return (
    <section aria-labelledby={headingId} aria-busy={feed.loading}>
      <div className="section-head">
        <h2 id={headingId} tabIndex={-1}>
          {title}
        </h2>
        <Button busy={feed.loading} onClick={feed.refresh}>
          {feed.loading ? 'Loading…' : 'Refresh'}
        </Button>
      </div>
      {/* Mounted before any data, so a screen reader is already watching it when the text first appears. */}
      <p role="status" className="visually-hidden">
        {page === null ? '' : `Page ${feed.pageNumber} loaded, ${page.items.length} rows.`}
      </p>
      <p role="status" className="visually-hidden">
        {newActivity.available ? NEW_ACTIVITY_NOTE : ''}
      </p>
      {newActivity.available ? (
        <div className="notice new-activity">
          <p>New activity available.</p>
          <Button onClick={feed.refresh}>Show newest</Button>
        </div>
      ) : newActivity.failed ? (
        <p className="notice">{NEW_ACTIVITY_CHECK_FAILED}</p>
      ) : null}
      <p className="muted">{intro}</p>
      {filters}
      {feed.error !== null ? (
        <ErrorBanner error={feed.error} onRetry={feed.retry} timeoutHint={timeoutHint} />
      ) : null}
      {page === null ? (
        feed.error === null ? (
          <LoadingState label={`Loading ${title.toLowerCase()}`} />
        ) : null
      ) : (
        <>
          <p className="muted">
            Window <DateTime value={page.from} /> to <DateTime value={page.to} /> (UTC, the end is
            not included). Read <DateTime value={page.generatedAt} />.
          </p>
          {page.items.length === 0 ? (
            <p className="empty">{emptyText}</p>
          ) : (
            <div className="table-scroll" tabIndex={0} role="region" aria-label={`${title} table`}>
              {children(page.items)}
            </div>
          )}
          <Pager
            label={title}
            pageNumber={feed.pageNumber}
            hasPrevious={feed.hasPrevious}
            hasNext={feed.hasNext}
            busy={feed.loading}
            onPrevious={() => {
              moved.current = true
              feed.previous()
            }}
            onNext={() => {
              moved.current = true
              feed.next()
            }}
          />
          <p className="muted">{FEED_FRESHNESS_NOTE}</p>
        </>
      )}
    </section>
  )
}
