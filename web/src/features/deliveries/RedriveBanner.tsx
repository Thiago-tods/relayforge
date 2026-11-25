import type { Ref } from 'react'
import { Button } from '../../ui/Button'
import { noticeFor } from './redriveOutcome'
import type { RedriveFlow } from './useRedrive'

interface Props {
  flow: RedriveFlow
  /** The heading receives focus when the dialog closes with a result. */
  headingRef: Ref<HTMLHeadingElement>
  onCheckAgain: () => void
}

/** The result of a redrive, shown on the page after the dialog closes. Errors are announced as alerts. */
export function RedriveBanner({ flow, headingRef, onCheckAgain }: Props) {
  const notice = noticeFor(flow)
  if (notice === null) return null
  const refreshFailed = flow.phase === 'outcome' && flow.refreshFailed
  return (
    <section
      className={`redrive-banner redrive-banner-${notice.tone}`}
      aria-labelledby="redrive-banner-heading"
      role={notice.tone === 'error' ? 'alert' : undefined}
    >
      <h2 id="redrive-banner-heading" tabIndex={-1} ref={headingRef}>
        {notice.title}
      </h2>
      <p>{notice.message}</p>
      {refreshFailed ? (
        <p>The delivery details could not be refreshed. Use Refresh to load the latest state.</p>
      ) : null}
      {flow.phase === 'unverified' ? (
        <Button variant="secondary" onClick={onCheckAgain}>
          Check again
        </Button>
      ) : null}
    </section>
  )
}
