import type { DestinationStatus } from '../../api/destinationTypes'

const LABELS: Record<DestinationStatus, string> = {
  ACTIVE: 'Active',
  PAUSED: 'Paused',
  REVOKED: 'Revoked',
}

/** The status as text (never only a color), with a modifier class for light and dark theme colors. */
export function StatusBadge({ status }: { status: DestinationStatus }) {
  return <span className={`badge status-${status.toLowerCase()}`}>{LABELS[status]}</span>
}
