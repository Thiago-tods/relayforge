import type { QueueAge } from '../../api/operationsTypes'
import { formatAge, isCapped } from '../../lib/format'
import { Count } from '../../ui/Count'
import { AGE_EXPLANATION } from './queueLabels'

/**
 * The overdue-age distribution, exactly as the API returns it: fixed buckets with a count each. The table is the
 * content; the bars are decoration for sighted users and are hidden from assistive technology. There are no
 * percentiles, averages or totals, because the API provides none (a total of lower bounds would mean nothing).
 */
export function QueueAgeChart({ age }: { age: QueueAge }) {
  const largest = Math.max(0, ...age.buckets.map((bucket) => bucket.count))

  return (
    <div>
      <p className="muted">{AGE_EXPLANATION}</p>
      {largest === 0 ? (
        <p className="empty">No due deliveries.</p>
      ) : (
        <div className="table-scroll">
          <table className="age-table">
            <caption className="visually-hidden">
              Due deliveries by how long they have been overdue
            </caption>
            <thead>
              <tr>
                <th scope="col">Overdue for</th>
                <th scope="col" className="num">
                  Deliveries
                </th>
                <th scope="col">
                  <span className="visually-hidden">Relative size</span>
                </th>
              </tr>
            </thead>
            <tbody>
              {age.buckets.map((bucket) => (
                <tr key={bucket.label}>
                  <th scope="row">
                    {bucket.label}
                    <span className="visually-hidden">
                      {bucket.maxSeconds === null
                        ? `, overdue ${formatAge(bucket.minSeconds)} or more, with no upper bound`
                        : `, overdue from ${formatAge(bucket.minSeconds)} up to ${formatAge(bucket.maxSeconds)}`}
                    </span>
                  </th>
                  <td className="num">
                    <Count value={bucket.count} />
                  </td>
                  <td aria-hidden="true">
                    <span className="bar-track">
                      <span
                        className={isCapped(bucket.count) ? 'bar bar-lower-bound' : 'bar'}
                        style={{ width: `${(bucket.count / largest) * 100}%` }}
                      />
                    </span>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  )
}
