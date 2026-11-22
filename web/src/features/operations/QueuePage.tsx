import { getQueue, getQueueAge } from '../../api/operations'
import { formatAge, isCapped } from '../../lib/format'
import { useResource } from '../../lib/useResource'
import { useOrg } from '../../org/orgContext'
import { Button } from '../../ui/Button'
import { Count } from '../../ui/Count'
import { DateTime } from '../../ui/DateTime'
import { MetricCard } from '../../ui/MetricCard'
import { LoadingState } from '../../ui/states'
import { ErrorBanner } from './ErrorBanner'
import { QueueAgeChart } from './QueueAgeChart'
import {
  COUNTS_NOTE,
  LOWER_BOUND_NOTE,
  OLDEST_DUE,
  QUEUE_METRICS,
  TWO_READS_NOTE,
} from './queueLabels'

const TIMEOUT_HINT = 'This read has no time window to narrow. Try again in a moment.'

/**
 * The queue of one organization right now: five delivery counts, the age of the oldest due delivery, and how long
 * due deliveries have been overdue. Two independent read-only requests, made once on open and again only when the
 * user presses Refresh. Nothing here polls.
 */
export function QueuePage() {
  const { organization } = useOrg()
  const orgId = organization.id
  const queue = useResource(orgId, (signal) => getQueue(orgId, signal))
  const age = useResource(orgId, (signal) => getQueueAge(orgId, signal))
  const busy = queue.loading || age.loading

  const snapshot = queue.data
  const anyCapped =
    snapshot !== null &&
    (snapshot.capped || QUEUE_METRICS.some((metric) => isCapped(snapshot[metric.key])))

  return (
    <div className="page">
      <header className="page-header page-header-row">
        <div>
          <h1>Queue</h1>
          <p>{COUNTS_NOTE}</p>
        </div>
        <Button
          busy={busy}
          onClick={() => {
            queue.reload()
            age.reload()
          }}
        >
          {busy ? 'Refreshing…' : 'Refresh'}
        </Button>
      </header>

      <section aria-labelledby="queue-snapshot" aria-busy={queue.loading}>
        <h2 id="queue-snapshot">Queue snapshot</h2>
        {queue.error !== null ? (
          <ErrorBanner error={queue.error} onRetry={queue.reload} timeoutHint={TIMEOUT_HINT} />
        ) : null}
        {snapshot === null ? (
          queue.error === null ? (
            <LoadingState label="Loading the queue" />
          ) : null
        ) : (
          <>
            <p className="muted" role="status">
              Snapshot taken <DateTime value={snapshot.generatedAt} />
            </p>
            <div className="metric-grid">
              {QUEUE_METRICS.map((metric) => (
                <MetricCard
                  key={metric.key}
                  label={metric.label}
                  value={<Count value={snapshot[metric.key]} />}
                  help={metric.help}
                />
              ))}
              <MetricCard
                label={OLDEST_DUE.label}
                value={
                  snapshot.oldestDueAgeSeconds > 0
                    ? formatAge(snapshot.oldestDueAgeSeconds)
                    : OLDEST_DUE.none
                }
                detail={
                  snapshot.oldestDueAgeSeconds > 0
                    ? `${snapshot.oldestDueAgeSeconds.toLocaleString('en-US')} seconds`
                    : undefined
                }
                help={OLDEST_DUE.help}
              />
            </div>
            {anyCapped ? <p className="notice">{LOWER_BOUND_NOTE}</p> : null}
          </>
        )}
      </section>

      <section aria-labelledby="queue-age" aria-busy={age.loading}>
        <h2 id="queue-age">Overdue age of due deliveries</h2>
        {age.error !== null ? (
          <ErrorBanner error={age.error} onRetry={age.reload} timeoutHint={TIMEOUT_HINT} />
        ) : null}
        {age.data === null ? (
          age.error === null ? (
            <LoadingState label="Loading the overdue age" />
          ) : null
        ) : (
          <>
            <p className="muted" role="status">
              Snapshot taken <DateTime value={age.data.generatedAt} />
            </p>
            <QueueAgeChart age={age.data} />
            {age.data.capped || age.data.buckets.some((bucket) => isCapped(bucket.count)) ? (
              <p className="notice">{LOWER_BOUND_NOTE}</p>
            ) : null}
          </>
        )}
      </section>

      <p className="muted">{TWO_READS_NOTE}</p>
    </div>
  )
}
