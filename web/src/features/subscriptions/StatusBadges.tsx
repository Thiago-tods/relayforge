import type { DestinationStatus } from '../../api/destinationTypes'
import type { EndpointStatus } from '../../api/ingestionEndpointTypes'
import type { SubscriptionStatus } from '../../api/subscriptionTypes'

// Three different statuses live on one subscription screen, so each badge says what it describes. The label is text
// (never only a color), and the modifier class reuses the theme colors of the destination badges.

const SUBSCRIPTION_LABELS: Record<SubscriptionStatus, string> = {
  ACTIVE: 'Active',
  REVOKED: 'Revoked',
}
const RESOURCE_LABELS: Record<DestinationStatus, string> = {
  ACTIVE: 'Active',
  PAUSED: 'Paused',
  REVOKED: 'Revoked',
}

/** The status of the subscription itself: whether its interval has ended. */
export function SubscriptionStatusBadge({ status }: { status: SubscriptionStatus }) {
  return (
    <span className={`badge status-${status.toLowerCase()}`} data-kind="subscription">
      {SUBSCRIPTION_LABELS[status]}
    </span>
  )
}

/** The current status of the destination, which is independent of the subscription's status. */
export function DestinationStatusBadge({ status }: { status: DestinationStatus }) {
  return (
    <span className={`badge status-${status.toLowerCase()}`} data-kind="destination">
      {RESOURCE_LABELS[status]}
    </span>
  )
}

/** The current status of the ingestion endpoint. */
export function EndpointStatusBadge({ status }: { status: EndpointStatus }) {
  return (
    <span className={`badge status-${status.toLowerCase()}`} data-kind="endpoint">
      {RESOURCE_LABELS[status]}
    </span>
  )
}
