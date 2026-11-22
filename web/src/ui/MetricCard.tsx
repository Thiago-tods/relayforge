import type { ReactNode } from 'react'

/** One labeled figure with a sentence that says exactly what it counts. */
export function MetricCard({
  label,
  value,
  detail,
  help,
}: {
  label: string
  value: ReactNode
  /** A secondary line under the value, such as the exact seconds behind a formatted age. */
  detail?: ReactNode
  help: string
}) {
  return (
    <article className="metric-card">
      <h3 className="metric-label">{label}</h3>
      <p className="metric-value">{value}</p>
      {detail ? <p className="metric-detail">{detail}</p> : null}
      <p className="metric-help">{help}</p>
    </article>
  )
}
