import { useOrg } from '../../org/orgContext'

export function OverviewPage() {
  const { organization, can } = useOrg()
  return (
    <div className="page">
      <header className="page-header">
        <h1>{organization.name}</h1>
        <p>
          Your role is <span className="badge">{organization.role}</span>
        </p>
      </header>
      <section className="panel" aria-labelledby="org-details">
        <h2 id="org-details">Organization</h2>
        <dl className="details">
          <dt>Slug</dt>
          <dd>{organization.slug}</dd>
          <dt>Created</dt>
          <dd>
            <time dateTime={organization.createdAt}>
              {new Date(organization.createdAt).toLocaleString()}
            </time>
          </dd>
          <dt>Can view delivery data</dt>
          <dd>{can('deliveries:read') ? 'Yes' : 'No'}</dd>
        </dl>
      </section>
      <p className="muted">
        Operations, deliveries, destinations, events and access management will appear here as they
        are built.
      </p>
    </div>
  )
}
