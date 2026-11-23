import { useState } from 'react'
import { getDeliverySummary } from '../../api/operations'
import type { DeliverySummary } from '../../api/operationsTypes'
import { filtersKey } from '../../lib/activityQuery'
import type { SummaryFilters } from '../../lib/activityQuery'
import { checkRange, toInputValue } from '../../lib/dateRange'
import type { RangeErrors } from '../../lib/dateRange'
import { formatPercent, isCapped } from '../../lib/format'
import { useDraft } from '../../lib/useDraft'
import { useResource } from '../../lib/useResource'
import { useOrg } from '../../org/orgContext'
import { Button } from '../../ui/Button'
import { Count } from '../../ui/Count'
import { DateTime } from '../../ui/DateTime'
import { MetricCard } from '../../ui/MetricCard'
import { LoadingState } from '../../ui/states'
import {
  COMPLETED_NOTE,
  CREATED_NOTE,
  DELIVERY_COUNTS_NOTE,
  FAILURE_RATE_HELP,
  FAILURE_RATE_UNAVAILABLE,
  LOWER_BOUND_NOTE,
  REASON_OPTIONS,
  reasonInfo,
  SUMMARY_INTRO,
  SUMMARY_TIMEOUT_HINT,
} from './activityLabels'
import { ErrorBanner } from './ErrorBanner'
import { FilterForm, RangeFields } from './FilterForm'

function everyCount(summary: DeliverySummary): number[] {
  const { created, completed } = summary
  return [
    created.total,
    ...Object.values(created.byStatus),
    completed.succeeded,
    completed.failed,
    completed.cancelled,
    ...Object.values(completed.byFailureReason),
  ]
}

const CREATED_CARDS = [
  { key: 'PENDING', label: 'Pending now' },
  { key: 'IN_FLIGHT', label: 'In flight now' },
  { key: 'SUCCEEDED', label: 'Succeeded' },
  { key: 'FAILED', label: 'Failed' },
  { key: 'CANCELLED', label: 'Cancelled' },
] as const

/**
 * The delivery summary of one window (GET /operations/deliveries): aggregate delivery counts only, never individual
 * deliveries. With no range applied, the server's own default window is requested and the page shows the window
 * the server echoes. The browser's clock is never used to compute it.
 */
export function DeliverySummarySection({
  applied,
  onApply,
}: {
  applied: SummaryFilters
  onApply: (filters: SummaryFilters) => void
}) {
  const orgId = useOrg().organization.id
  const appliedKey = filtersKey(applied)
  const summary = useResource(`${orgId}|${appliedKey}`, (signal) =>
    getDeliverySummary(orgId, applied.range ?? {}, signal),
  )
  const [draft, setDraft] = useDraft(
    {
      from: applied.range ? toInputValue(applied.range.from) : '',
      to: applied.range ? toInputValue(applied.range.to) : '',
    },
    appliedKey,
  )
  const [errors, setErrors] = useState<RangeErrors>({})

  function submit(next: SummaryFilters) {
    if (filtersKey(next) === appliedKey) summary.reload()
    else onApply(next)
  }

  function apply() {
    const check = checkRange(draft.from, draft.to)
    if (!check.ok) {
      setErrors(check.errors)
      return
    }
    setErrors({})
    submit({ range: check.range })
  }

  const data = summary.data
  const capped = data !== null && (data.capped || everyCount(data).some(isCapped))

  return (
    <section aria-labelledby="activity-summary-heading" aria-busy={summary.loading}>
      <div className="section-head">
        <h2 id="activity-summary-heading">Delivery summary</h2>
        <Button busy={summary.loading} onClick={summary.reload}>
          {summary.loading ? 'Loading…' : 'Refresh'}
        </Button>
      </div>
      <p className="muted">
        {SUMMARY_INTRO} {DELIVERY_COUNTS_NOTE}
      </p>
      <FilterForm
        label="Summary filters"
        errors={errors}
        onApply={apply}
        onReset={() => {
          setErrors({})
          setDraft({ from: '', to: '' })
          submit({ range: null })
        }}
      >
        <RangeFields
          from={draft.from}
          to={draft.to}
          errors={errors}
          onFrom={(from) => setDraft({ ...draft, from })}
          onTo={(to) => setDraft({ ...draft, to })}
        />
      </FilterForm>

      {summary.error !== null ? (
        <ErrorBanner
          error={summary.error}
          onRetry={summary.reload}
          timeoutHint={SUMMARY_TIMEOUT_HINT}
        />
      ) : null}
      {data === null ? (
        summary.error === null ? (
          <LoadingState label="Loading the delivery summary" />
        ) : null
      ) : (
        <>
          <p className="muted">
            Window <DateTime value={data.from} /> to <DateTime value={data.to} /> (UTC, the end is
            not included). Read <DateTime value={data.generatedAt} />.
          </p>

          <div role="group" aria-labelledby="summary-created" className="summary-group">
            <p id="summary-created" className="group-title">
              Created in this window
            </p>
            <p className="muted">{CREATED_NOTE}</p>
            <div className="metric-grid">
              <MetricCard
                label="Total created"
                value={<Count value={data.created.total} />}
                help="All deliveries created in the window."
              />
              {CREATED_CARDS.map((card) => (
                <MetricCard
                  key={card.key}
                  label={card.label}
                  value={<Count value={data.created.byStatus[card.key]} />}
                  help={`Created in the window and currently ${card.key.replace('_', ' ')}.`}
                />
              ))}
            </div>
          </div>

          <div role="group" aria-labelledby="summary-completed" className="summary-group">
            <p id="summary-completed" className="group-title">
              Completed in this window
            </p>
            <p className="muted">{COMPLETED_NOTE}</p>
            <div className="metric-grid">
              <MetricCard
                label="Succeeded"
                value={<Count value={data.completed.succeeded} />}
                help="Reached SUCCEEDED in the window."
              />
              <MetricCard
                label="Failed"
                value={<Count value={data.completed.failed} />}
                help="Reached FAILED in the window."
              />
              <MetricCard
                label="Cancelled"
                value={<Count value={data.completed.cancelled} />}
                help="Reached CANCELLED in the window, for example because a destination was revoked."
              />
              <MetricCard
                label="Failure rate"
                value={
                  data.completed.failureRate === null
                    ? '—'
                    : formatPercent(data.completed.failureRate)
                }
                detail={data.completed.failureRate === null ? FAILURE_RATE_UNAVAILABLE : undefined}
                help={FAILURE_RATE_HELP}
              />
            </div>

            <div
              className="table-scroll"
              tabIndex={0}
              role="region"
              aria-label="Failed deliveries by reason"
            >
              <table className="data-table">
                <caption className="visually-hidden">
                  Deliveries that failed in the window, by failure reason
                </caption>
                <thead>
                  <tr>
                    <th scope="col">Reason</th>
                    <th scope="col">What it means</th>
                    <th scope="col" className="num">
                      Deliveries
                    </th>
                  </tr>
                </thead>
                <tbody>
                  {REASON_OPTIONS.map((option) => (
                    <tr key={option.value}>
                      <th scope="row">{option.label}</th>
                      <td className="wrap">{reasonInfo(option.value).meaning}</td>
                      <td className="num">
                        <Count value={data.completed.byFailureReason[option.value]} />
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          </div>
          {capped ? <p className="notice">{LOWER_BOUND_NOTE}</p> : null}
        </>
      )}
    </section>
  )
}
